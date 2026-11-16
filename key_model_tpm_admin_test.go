package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestChangeModelTPMLimitPreservesOtherModels(t *testing.T) {
	current := map[string]int64{"gpt-6-sol": 1000, "claude-sonnet": 2000}
	newLimit := int64(1500)
	updated, err := changeModelTPMLimit(current, "gpt-6-sol", "set", &newLimit)
	if err != nil {
		t.Fatal(err)
	}
	if updated["gpt-6-sol"] != 1500 || updated["claude-sonnet"] != 2000 {
		t.Fatalf("设置一个模型时应保留其他模型限制，实际为 %#v", updated)
	}
}

func TestRemoveModelTPMLimitPreservesOtherModels(t *testing.T) {
	current := map[string]int64{"gpt-6-sol": 1000, "claude-sonnet": 2000}
	updated, err := changeModelTPMLimit(current, "gpt-6-sol", "remove", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := updated["gpt-6-sol"]; exists || updated["claude-sonnet"] != 2000 {
		t.Fatalf("清除一个模型时应只移除目标并保留其他限制，实际为 %#v", updated)
	}
	cleared, err := changeModelTPMLimit(map[string]int64{"gpt-6-sol": 1000}, "gpt-6-sol", "remove", nil)
	if err != nil || len(cleared) != 0 {
		t.Fatalf("清除最后一个模型限制应产生空映射，实际为 %#v，错误=%v", cleared, err)
	}
}

func TestChangeModelTPMLimitRejectsAmbiguousInputs(t *testing.T) {
	limit := int64(100)
	for _, test := range []struct {
		action string
		limit  *int64
	}{
		{action: "set"},
		{action: "set", limit: func() *int64 { v := int64(0); return &v }()},
		{action: "remove", limit: &limit},
		{action: "unknown"},
	} {
		if _, err := changeModelTPMLimit(nil, "gpt-6-sol", test.action, test.limit); err == nil {
			t.Fatalf("action=%q limit=%v 应拒绝", test.action, test.limit)
		}
	}
}

func TestValidateKeyModelTPMInput(t *testing.T) {
	positive := int64(100)
	zero := int64(0)
	tests := []struct {
		name string
		in   PreviewKeyModelTPMInput
		ok   bool
	}{
		{name: "设置正整数", in: PreviewKeyModelTPMInput{TeamAlias: "math", KeyAlias: "batch-a", ModelName: "gpt-6-sol", Action: "set", TPMLimit: &positive}, ok: true},
		{name: "清除指定模型", in: PreviewKeyModelTPMInput{TeamAlias: "math", KeyAlias: "batch-a", ModelName: "gpt-6-sol", Action: "remove"}, ok: true},
		{name: "设置缺少上限", in: PreviewKeyModelTPMInput{TeamAlias: "math", KeyAlias: "batch-a", ModelName: "gpt-6-sol", Action: "set"}},
		{name: "拒绝零上限", in: PreviewKeyModelTPMInput{TeamAlias: "math", KeyAlias: "batch-a", ModelName: "gpt-6-sol", Action: "set", TPMLimit: &zero}},
		{name: "清除时拒绝上限", in: PreviewKeyModelTPMInput{TeamAlias: "math", KeyAlias: "batch-a", ModelName: "gpt-6-sol", Action: "remove", TPMLimit: &positive}},
		{name: "拒绝空模型", in: PreviewKeyModelTPMInput{TeamAlias: "math", KeyAlias: "batch-a", Action: "remove"}},
		{name: "拒绝未知操作", in: PreviewKeyModelTPMInput{TeamAlias: "math", KeyAlias: "batch-a", ModelName: "gpt-6-sol", Action: "delete"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateKeyModelTPMInput(test.in)
			if (err == nil) != test.ok {
				t.Fatalf("校验 err=%v，期望有效=%v", err, test.ok)
			}
		})
	}
}

func TestUpdateKeyModelTPMSubmitsOnlyScopedFields(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi.json" {
			_, _ = w.Write([]byte(`{"paths":{"/key/update":{"post":{"requestBody":{"content":{"application/json":{"schema":{"properties":{"model_tpm_limit":{}}}}}}}}}}`))
			return
		}
		if r.URL.Path != "/key/update" || r.Method != http.MethodPost {
			t.Fatalf("收到非预期请求 %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解析请求失败: %v", err)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client, err := newAdminClient(server.URL, "admin-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckKeyModelTPMUpdateSupport(context.Background()); err != nil {
		t.Fatal(err)
	}
	limits := map[string]int64{"gpt-6-sol": 1500, "claude-sonnet": 2000}
	if err := client.UpdateKeyModelTPM(context.Background(), "team-1", "batch-a", limits); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"team_id": "team-1", "key_alias": "batch-a",
		"model_tpm_limit": map[string]any{"gpt-6-sol": float64(1500), "claude-sonnet": float64(2000)},
	}
	if len(body) != len(want) {
		t.Fatalf("请求字段应只包含团队、key 别名和 model_tpm_limit，实际为 %#v", body)
	}
	if body["team_id"] != want["team_id"] || body["key_alias"] != want["key_alias"] {
		t.Fatalf("key 更新目标不匹配: %#v", body)
	}
	gotLimits, ok := body["model_tpm_limit"].(map[string]any)
	if !ok || gotLimits["gpt-6-sol"] != float64(1500) || gotLimits["claude-sonnet"] != float64(2000) {
		t.Fatalf("model_tpm_limit 映射不匹配: %#v", body["model_tpm_limit"])
	}
}

func TestCheckKeyModelTPMUpdateSupportRejectsMissingField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"paths":{"/key/update":{"post":{"requestBody":{"content":{"application/json":{"schema":{"properties":{"max_budget":{}}}}}}}}}}`))
	}))
	defer server.Close()

	client, err := newAdminClient(server.URL, "admin-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckKeyModelTPMUpdateSupport(context.Background()); err == nil {
		t.Fatal("OpenAPI 未声明 model_tpm_limit 时应拒绝预览")
	}
}

func TestCheckKeyModelTPMUpdateSupportResolvesSchemaReferences(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"paths":{"/key/update":{"post":{"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/UpdateKeyRequest"}}}}}}},"components":{"schemas":{"UpdateKeyRequest":{"allOf":[{"$ref":"#/components/schemas/KeyRequestBase"},{"type":"object","properties":{"model_tpm_limit":{"type":"object"}}}]},"KeyRequestBase":{"type":"object","properties":{"key_alias":{"type":"string"}}}}}}`))
	}))
	defer server.Close()
	client, err := newAdminClient(server.URL, "admin-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckKeyModelTPMUpdateSupport(context.Background()); err != nil {
		t.Fatalf("应能在 allOf 和 $ref 中找到 model_tpm_limit: %v", err)
	}
}

func TestUpdateKeyModelTPMSubmitsEmptyMapToClearLastModel(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/key/update" {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("解析请求失败: %v", err)
			}
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := newAdminClient(server.URL, "admin-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.UpdateKeyModelTPM(context.Background(), "team-1", "batch-a", nil); err != nil {
		t.Fatal(err)
	}
	limits, ok := body["model_tpm_limit"].(map[string]any)
	if !ok || len(limits) != 0 {
		t.Fatalf("清除最后一个模型时应显式提交空映射，实际为 %#v", body)
	}
}

func TestDecodeModelTPMLimitMap(t *testing.T) {
	got, err := decodeModelTPMLimitMap(pgtype.Text{String: `{"gpt-6-sol":1200}`, Valid: true})
	if err != nil || got["gpt-6-sol"] != 1200 {
		t.Fatalf("合法 TPM 映射解析失败: %#v, err=%v", got, err)
	}
	for _, value := range []string{`null`, `{}`, `{"gpt-6-sol":1.5}`, `{"":100}`} {
		parsed, err := decodeModelTPMLimitMap(pgtype.Text{String: value, Valid: true})
		if value == `null` || value == `{}` {
			if err != nil || len(parsed) != 0 {
				t.Fatalf("空映射 %s 应规范化为空，得到 %#v，err=%v", value, parsed, err)
			}
		} else if err == nil {
			t.Fatalf("无效映射 %s 应拒绝", value)
		}
	}
}

func TestApplyKeyModelTPMReturnsEmptyTeamMapAsObjectWhenUnset(t *testing.T) {
	keyLimits := map[string]int64{"gpt-6-sol": 1000}
	var teamLimits map[string]int64
	query := budgetQueryFunc(func(context.Context, string, ...any) (pgx.Rows, error) {
		keyJSON, _ := json.Marshal(keyLimits)
		teamJSON, _ := json.Marshal(teamLimits)
		return &budgetRowsStub{data: [][]any{{"team-math", "math", "batch-a", string(keyJSON), string(teamJSON)}}}, nil
	})
	store, err := newChangeStore(filepath.Join(t.TempDir(), "changes"))
	if err != nil {
		t.Fatal(err)
	}
	api := &keyModelTPMAPIStub{apply: func(_ string, limits map[string]int64) { keyLimits = limits }}
	service := &keyModelTPMAdminService{
		query: &keyBudgetService{pool: query}, api: api, store: store, owner: "owner", target: "https://litellm.example",
		now: func() time.Time { return time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC) },
	}
	limit := int64(300000)
	_, preview, err := service.Preview(context.Background(), nil, PreviewKeyModelTPMInput{
		TeamAlias: "math", KeyAlias: "batch-a", ModelName: "gpt-6-sol", Action: "set", TPMLimit: &limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := service.Apply(context.Background(), nil, ApplyKeyModelTPMInput{ChangeID: preview.ChangeID, Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "applied" || result.Observed == nil || result.Observed.TeamModelTPMLimit == nil {
		t.Fatalf("未配置团队 TPM 时 observed.team_model_tpm_limit 必须是空对象: %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	observed, ok := payload["observed"].(map[string]any)
	if !ok {
		t.Fatalf("observed 应为对象: %s", encoded)
	}
	if teamLimits, ok := observed["team_model_tpm_limit"].(map[string]any); !ok || len(teamLimits) != 0 {
		t.Fatalf("team_model_tpm_limit 应编码为 {} 而不是 null: %s", encoded)
	}
}

func TestApplyKeyModelTPMThroughMCPAcceptsEmptyTeamLimitObject(t *testing.T) {
	keyLimits := map[string]int64{"gpt-6-sol": 1000}
	query := budgetQueryFunc(func(context.Context, string, ...any) (pgx.Rows, error) {
		keyJSON, _ := json.Marshal(keyLimits)
		return &budgetRowsStub{data: [][]any{{"team-math", "math", "batch-a", string(keyJSON), `null`}}}, nil
	})
	store, err := newChangeStore(filepath.Join(t.TempDir(), "changes"))
	if err != nil {
		t.Fatal(err)
	}
	api := &keyModelTPMAPIStub{apply: func(_ string, limits map[string]int64) { keyLimits = limits }}
	service := &keyModelTPMAdminService{
		query: &keyBudgetService{pool: query}, api: api, store: store, owner: "owner", target: "https://litellm.example",
		now: func() time.Time { return time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC) },
	}
	admin := &budgetAdminService{keyTPM: service}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := newAdminMCPServer(query, admin).Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	previewResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "preview_team_key_model_tpm_change",
		Arguments: map[string]any{
			"team_alias": "math", "key_alias": "batch-a", "model_name": "gpt-6-sol",
			"action": "set", "tpm_limit": 300000,
		},
	})
	if err != nil || previewResult.IsError {
		t.Fatalf("MCP TPM 预览失败: %+v, err=%v", previewResult, err)
	}
	encoded, err := json.Marshal(previewResult.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var preview KeyModelTPMChangeOutput
	if err := json.Unmarshal(encoded, &preview); err != nil {
		t.Fatal(err)
	}
	applyResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "apply_team_key_model_tpm_change",
		Arguments: map[string]any{"change_id": preview.ChangeID, "confirm": true},
	})
	if err != nil || applyResult.IsError {
		t.Fatalf("团队 TPM 未配置时 MCP apply 回执应通过输出结构校验: %+v, err=%v", applyResult, err)
	}
	encoded, err = json.Marshal(applyResult.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var applied KeyModelTPMChangeOutput
	if err := json.Unmarshal(encoded, &applied); err != nil || applied.Status != "applied" || applied.Observed == nil ||
		applied.Observed.TeamModelTPMLimit == nil || len(applied.Observed.TeamModelTPMLimit) != 0 {
		t.Fatalf("MCP apply 应返回带空团队 TPM 对象的成功回执: %s, err=%v", encoded, err)
	}
}

type keyModelTPMAPIStub struct {
	mu    sync.Mutex
	calls int
	apply func(string, map[string]int64)
}

func (a *keyModelTPMAPIStub) CheckKeyModelTPMUpdateSupport(context.Context) error { return nil }

func (a *keyModelTPMAPIStub) UpdateKeyModelTPM(_ context.Context, teamID, keyAlias string, limits map[string]int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if teamID != "team-math" || keyAlias != "batch-a" {
		return fmt.Errorf("目标不匹配")
	}
	a.calls++
	a.apply(keyAlias, limits)
	return nil
}

func TestPreviewAndApplyTeamKeyModelTPMSetAndRemove(t *testing.T) {
	keyLimits := map[string]int64{"gpt-6-sol": 1000, "claude-sonnet": 2000}
	teamLimits := map[string]int64{"gpt-6-sol": 5000}
	query := budgetQueryFunc(func(context.Context, string, ...any) (pgx.Rows, error) {
		keyJSON, _ := json.Marshal(keyLimits)
		teamJSON, _ := json.Marshal(teamLimits)
		return &budgetRowsStub{data: [][]any{{"team-math", "math", "batch-a", string(keyJSON), string(teamJSON)}}}, nil
	})
	store, err := newChangeStore(filepath.Join(t.TempDir(), "changes"))
	if err != nil {
		t.Fatal(err)
	}
	api := &keyModelTPMAPIStub{apply: func(_ string, limits map[string]int64) {
		keyLimits = cloneModelTPMLimit(limits)
	}}
	service := &keyModelTPMAdminService{
		query: &keyBudgetService{pool: query}, api: api, store: store,
		owner: "owner", target: "https://litellm.example",
		now: func() time.Time { return time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC) },
	}
	limit := int64(1500)
	_, previewSet, err := service.Preview(context.Background(), nil, PreviewKeyModelTPMInput{
		TeamAlias: "math", KeyAlias: "batch-a", ModelName: "gpt-6-sol", Action: "set", TPMLimit: &limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if previewSet.OldTPMLimit == nil || *previewSet.OldTPMLimit != 1000 || previewSet.NewTPMLimit == nil ||
		*previewSet.NewTPMLimit != 1500 || previewSet.TeamTPMLimit == nil || *previewSet.TeamTPMLimit != 5000 ||
		previewSet.AfterModelTPMLimit["claude-sonnet"] != 2000 {
		t.Fatalf("预览应展示 key/team TPM 并保留其他模型限制: %+v", previewSet)
	}
	if _, _, err := service.Apply(context.Background(), nil, ApplyKeyModelTPMInput{ChangeID: previewSet.ChangeID}); err == nil {
		t.Fatal("未确认时不能提交")
	}
	_, appliedSet, err := service.Apply(context.Background(), nil, ApplyKeyModelTPMInput{ChangeID: previewSet.ChangeID, Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if appliedSet.Status != "applied" || appliedSet.Observed == nil || api.calls != 1 {
		t.Fatalf("设置 TPM 应回读确认: %+v, calls=%d", appliedSet, api.calls)
	}

	_, previewRemove, err := service.Preview(context.Background(), nil, PreviewKeyModelTPMInput{
		TeamAlias: "math", KeyAlias: "batch-a", ModelName: "gpt-6-sol", Action: "remove",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, removedModelRemains := previewRemove.AfterModelTPMLimit["gpt-6-sol"]
	if previewRemove.NewTPMLimit != nil || removedModelRemains ||
		previewRemove.AfterModelTPMLimit["claude-sonnet"] != 2000 || previewRemove.TeamTPMLimit == nil ||
		*previewRemove.TeamTPMLimit != 5000 || !strings.Contains(strings.Join(previewRemove.Warnings, " "), "团队级 TPM") {
		t.Fatalf("清除只能移除目标 key 条目并提示团队限制仍生效: %+v", previewRemove)
	}
	_, appliedRemove, err := service.Apply(context.Background(), nil, ApplyKeyModelTPMInput{ChangeID: previewRemove.ChangeID, Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if appliedRemove.Status != "applied" || api.calls != 2 || keyLimits["claude-sonnet"] != 2000 {
		t.Fatalf("清除后应保留其他模型并回读确认: %+v, limits=%#v calls=%d", appliedRemove, keyLimits, api.calls)
	}
}

func TestApplyKeyModelTPMRejectsChangedSnapshot(t *testing.T) {
	keyLimits := map[string]int64{"gpt-6-sol": 1000}
	teamLimits := map[string]int64{"gpt-6-sol": 5000}
	query := budgetQueryFunc(func(context.Context, string, ...any) (pgx.Rows, error) {
		keyJSON, _ := json.Marshal(keyLimits)
		teamJSON, _ := json.Marshal(teamLimits)
		return &budgetRowsStub{data: [][]any{{"team-math", "math", "batch-a", string(keyJSON), string(teamJSON)}}}, nil
	})
	store, err := newChangeStore(filepath.Join(t.TempDir(), "changes"))
	if err != nil {
		t.Fatal(err)
	}
	api := &keyModelTPMAPIStub{apply: func(_ string, limits map[string]int64) { keyLimits = limits }}
	service := &keyModelTPMAdminService{
		query: &keyBudgetService{pool: query}, api: api, store: store, owner: "owner", target: "https://litellm.example",
		now: func() time.Time { return time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC) },
	}
	limit := int64(1500)
	_, preview, err := service.Preview(context.Background(), nil, PreviewKeyModelTPMInput{
		TeamAlias: "math", KeyAlias: "batch-a", ModelName: "gpt-6-sol", Action: "set", TPMLimit: &limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	teamLimits["gpt-6-sol"] = 6000
	_, result, err := service.Apply(context.Background(), nil, ApplyKeyModelTPMInput{ChangeID: preview.ChangeID, Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "conflict" || api.calls != 0 {
		t.Fatalf("团队 TPM 快照变化时应拒绝提交: %+v, calls=%d", result, api.calls)
	}
}

func TestApplyKeyModelTPMRecoversWithoutRepeatingWrite(t *testing.T) {
	keyLimits := map[string]int64{"gpt-6-sol": 1000}
	teamLimits := map[string]int64{}
	query := budgetQueryFunc(func(context.Context, string, ...any) (pgx.Rows, error) {
		keyJSON, _ := json.Marshal(keyLimits)
		teamJSON, _ := json.Marshal(teamLimits)
		return &budgetRowsStub{data: [][]any{{"team-math", "math", "batch-a", string(keyJSON), string(teamJSON)}}}, nil
	})
	store, err := newChangeStore(filepath.Join(t.TempDir(), "changes"))
	if err != nil {
		t.Fatal(err)
	}
	api := &keyModelTPMAPIStub{apply: func(_ string, limits map[string]int64) { keyLimits = limits }}
	service := &keyModelTPMAdminService{
		query: &keyBudgetService{pool: query}, api: api, store: store, owner: "owner", target: "https://litellm.example",
		now: func() time.Time { return time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC) },
	}
	limit := int64(1500)
	_, preview, err := service.Preview(context.Background(), nil, PreviewKeyModelTPMInput{
		TeamAlias: "math", KeyAlias: "batch-a", ModelName: "gpt-6-sol", Action: "set", TPMLimit: &limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	change, err := store.loadKeyTPM(preview.ChangeID)
	if err != nil {
		t.Fatal(err)
	}
	change.State, change.Status = "executing", "inflight"
	if err := store.saveKeyTPM(change); err != nil {
		t.Fatal(err)
	}
	keyLimits = cloneModelTPMLimit(change.After)
	_, recovered, err := service.Apply(context.Background(), nil, ApplyKeyModelTPMInput{ChangeID: preview.ChangeID, Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != "applied" || !recovered.Confirmed || api.calls != 0 {
		t.Fatalf("应通过回读恢复成功且不重复写入: %+v, calls=%d", recovered, api.calls)
	}
}
