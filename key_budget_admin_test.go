package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestValidateKeyBudgetPreviewInput(t *testing.T) {
	tests := []struct {
		name string
		in   PreviewKeyBudgetInput
		max  float64
		ok   bool
	}{
		{name: "有效额度", in: PreviewKeyBudgetInput{TeamAlias: "math", KeyAlias: "batch-a", MaxBudget: 50}, max: 100, ok: true},
		{name: "缺少团队", in: PreviewKeyBudgetInput{KeyAlias: "batch-a", MaxBudget: 50}, max: 100},
		{name: "缺少 key 别名", in: PreviewKeyBudgetInput{TeamAlias: "math", MaxBudget: 50}, max: 100},
		{name: "拒绝零额度", in: PreviewKeyBudgetInput{TeamAlias: "math", KeyAlias: "batch-a", MaxBudget: 0}, max: 100},
		{name: "拒绝负数", in: PreviewKeyBudgetInput{TeamAlias: "math", KeyAlias: "batch-a", MaxBudget: -1}, max: 100},
		{name: "拒绝超过上限", in: PreviewKeyBudgetInput{TeamAlias: "math", KeyAlias: "batch-a", MaxBudget: 101}, max: 100},
		{name: "拒绝非有限值", in: PreviewKeyBudgetInput{TeamAlias: "math", KeyAlias: "batch-a", MaxBudget: math.Inf(1)}, max: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateKeyBudgetPreviewInput(tt.in, tt.max)
			if (err == nil) != tt.ok {
				t.Fatalf("校验结果 err=%v，期望有效=%v", err, tt.ok)
			}
		})
	}
}

func TestNormalizeKeyBudgetDurationDefaultsAndAcceptsOverride(t *testing.T) {
	for input, expected := range map[string]string{"": "1d", " 7d ": "7d", "MONTHLY": "monthly"} {
		got, err := normalizeKeyBudgetDuration(input)
		if err != nil || got != expected {
			t.Fatalf("周期 %q 应解析为 %q，得到 %q，错误=%v", input, expected, got, err)
		}
	}
	for _, input := range []string{"0d", "7x", "1d30m", "-1d"} {
		if _, err := normalizeKeyBudgetDuration(input); err == nil {
			t.Fatalf("无效周期 %q 应拒绝", input)
		}
	}
}

func TestEffectiveKeyBudgetAfterChangeHonorsLinkedBudget(t *testing.T) {
	linked := 30.0
	if got := effectiveKeyBudgetAfterChange(50, &linked); got == nil || *got != 30 {
		t.Fatalf("关联预算较小时生效额度应为 30，得到 %v", got)
	}
	if got := effectiveKeyBudgetAfterChange(50, nil); got == nil || *got != 50 {
		t.Fatalf("没有关联额度时生效额度应为 50，得到 %v", got)
	}
}

func TestUpdateKeyBudgetSendsOnlyScopedBudgetFields(t *testing.T) {
	var path string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Method == http.MethodGet && path == "/openapi.json" {
			_, _ = w.Write([]byte(`{"paths":{"/key/update":{"post":{}}}}`))
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解析请求体失败: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()

	client, err := newAdminClient(server.URL, "admin-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckKeyBudgetUpdateSupport(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.UpdateKeyBudget(context.Background(), "team-1", "batch-a", 50, "7d"); err != nil {
		t.Fatal(err)
	}
	if path != "/key/update" {
		t.Fatalf("请求路径为 %q", path)
	}
	if len(body) != 4 || body["team_id"] != "team-1" || body["key_alias"] != "batch-a" ||
		body["max_budget"] != float64(50) || body["budget_duration"] != "7d" {
		t.Fatalf("请求只应包含目标团队、key 别名、max_budget 和 budget_duration，实际为 %#v", body)
	}
	for _, forbidden := range []string{"key", "spend", "tpm_limit", "rpm_limit"} {
		if _, ok := body[forbidden]; ok {
			t.Fatalf("请求不应包含字段 %q", forbidden)
		}
	}
}

type keyBudgetAPIStub struct {
	mu    sync.Mutex
	calls int
	apply func(string, float64, string)
}

func (a *keyBudgetAPIStub) CheckKeyBudgetUpdateSupport(context.Context) error { return nil }

func (a *keyBudgetAPIStub) UpdateKeyBudget(_ context.Context, teamID, keyAlias string, amount float64, duration string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if teamID != "team-math" || keyAlias != "batch-a" {
		return fmt.Errorf("目标标识不匹配")
	}
	a.calls++
	a.apply(keyAlias, amount, duration)
	return nil
}

func TestPreviewAndApplyTeamKeyBudgetChange(t *testing.T) {
	keyMax := 100.0
	linkedMax := 80.0
	keyDuration := "30d"
	created := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	reset := created.Add(30 * 24 * time.Hour)
	query := budgetQueryFunc(func(context.Context, string, ...any) (pgx.Rows, error) {
		effective := keyMax
		if linkedMax < effective {
			effective = linkedMax
		}
		return &budgetRowsStub{data: [][]any{{
			"team-math", "math", "batch-a", "Batch A", "user-1", 12.5,
			keyMax, linkedMax, effective, keyDuration, reset, nil, false, created,
		}}}, nil
	})
	store, err := newChangeStore(filepath.Join(t.TempDir(), "changes"))
	if err != nil {
		t.Fatal(err)
	}
	api := &keyBudgetAPIStub{apply: func(_ string, amount float64, duration string) {
		keyMax, keyDuration = amount, duration
		reset = time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	}}
	service := &keyBudgetAdminService{
		query: &keyBudgetService{pool: query}, api: api, store: store,
		owner: "admin-owner", target: "https://litellm.example", max: 500,
		now: func() time.Time { return time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC) },
	}
	_, preview, err := service.Preview(context.Background(), nil, PreviewKeyBudgetInput{
		TeamAlias: "math", KeyAlias: "batch-a", MaxBudget: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Status != "pending" || preview.OldKeyMaxBudget == nil || *preview.OldKeyMaxBudget != 100 ||
		preview.LinkedMaxBudget == nil || *preview.LinkedMaxBudget != 80 ||
		preview.NewEffectiveBudget == nil || *preview.NewEffectiveBudget != 80 ||
		!strings.Contains(strings.Join(preview.Warnings, " "), "关联预算") ||
		!strings.Contains(strings.Join(preview.Warnings, " "), "当前消费会保留") || preview.NewBudgetDuration != "1d" ||
		preview.CurrentBudgetDuration == nil || *preview.CurrentBudgetDuration != "30d" {
		t.Fatalf("预览必须清楚区分 key 额度、关联额度和新生效额度: %+v", preview)
	}
	if _, _, err := service.Apply(context.Background(), nil, ApplyKeyBudgetInput{ChangeID: preview.ChangeID}); err == nil {
		t.Fatal("未明确确认时必须拒绝执行")
	}
	if api.calls != 0 {
		t.Fatal("缺少确认时不能调用 LiteLLM")
	}
	_, applied, err := service.Apply(context.Background(), nil, ApplyKeyBudgetInput{ChangeID: preview.ChangeID, Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Status != "applied" || !applied.BudgetConfirmed || applied.NewKeyMaxBudget != 90 ||
		applied.NewEffectiveBudget == nil || *applied.NewEffectiveBudget != 80 || api.calls != 1 ||
		applied.NewBudgetDuration != "1d" || applied.Observed == nil || applied.Observed.BudgetDuration == nil ||
		*applied.Observed.BudgetDuration != "1d" {
		t.Fatalf("执行结果应回读确认，且保留较高的关联额度: %+v calls=%d", applied, api.calls)
	}
	_, replay, err := service.Apply(context.Background(), nil, ApplyKeyBudgetInput{ChangeID: preview.ChangeID, Confirm: true})
	if err != nil || replay.Status != "applied" || api.calls != 1 {
		t.Fatalf("重复调用同一编号只能读取结果，不能重复写入: %+v calls=%d err=%v", replay, api.calls, err)
	}
}

func TestKeyBudgetPreviewRejectsZeroAndAmbiguousAliases(t *testing.T) {
	if err := validateKeyBudgetPreviewInput(PreviewKeyBudgetInput{TeamAlias: "math", KeyAlias: "batch-a", MaxBudget: 0}, 100); err == nil {
		t.Fatal("首版不允许零额度")
	}
	query := budgetQueryFunc(func(context.Context, string, ...any) (pgx.Rows, error) {
		created := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
		row := []any{"team-math", "math", "batch-a", "Batch A", nil, 0.0, 10.0, nil, 10.0, nil, nil, nil, false, created}
		return &budgetRowsStub{data: [][]any{row, row}}, nil
	})
	store, err := newChangeStore(filepath.Join(t.TempDir(), "changes"))
	if err != nil {
		t.Fatal(err)
	}
	service := &keyBudgetAdminService{query: &keyBudgetService{pool: query}, api: &keyBudgetAPIStub{}, store: store,
		owner: "admin-owner", target: "target", max: 100, now: time.Now}
	if _, _, err := service.Preview(context.Background(), nil, PreviewKeyBudgetInput{TeamAlias: "math", KeyAlias: "batch-a", MaxBudget: 20}); err == nil {
		t.Fatal("别名在目标团队下不唯一时必须拒绝生成预览")
	}
}

func TestKeyBudgetPreviewRejectsAliasSharedAcrossTeams(t *testing.T) {
	created := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	teamKey := []any{"team-math", "math", "batch-a", "Batch A", nil, 0.0, 10.0, nil, 10.0, nil, nil, nil, false, created}
	otherTeamKey := []any{"team-other", "other", "batch-a", "Batch A", nil, 0.0, 10.0, nil, 10.0, nil, nil, nil, false, created}
	query := budgetQueryFunc(func(_ context.Context, statement string, _ ...any) (pgx.Rows, error) {
		if strings.Contains(statement, "WHERE key_alias = $1") {
			return &budgetRowsStub{data: [][]any{teamKey, otherTeamKey}}, nil
		}
		return &budgetRowsStub{data: [][]any{teamKey}}, nil
	})
	store, err := newChangeStore(filepath.Join(t.TempDir(), "changes"))
	if err != nil {
		t.Fatal(err)
	}
	service := &keyBudgetAdminService{query: &keyBudgetService{pool: query}, api: &keyBudgetAPIStub{}, store: store,
		owner: "admin-owner", target: "target", max: 100, now: time.Now}
	if _, _, err := service.Preview(context.Background(), nil, PreviewKeyBudgetInput{TeamAlias: "math", KeyAlias: "batch-a", MaxBudget: 20}); err == nil {
		t.Fatal("LiteLLM 无法用重复 key_alias 唯一定位时，不得生成修改预览")
	}
}
