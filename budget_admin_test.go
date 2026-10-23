package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type safetyFunc func(context.Context, memberSnapshot) error

func (f safetyFunc) Check(ctx context.Context, member memberSnapshot) error { return f(ctx, member) }

type adminFixture struct {
	mu             sync.Mutex
	team           teamInfo
	posts          int
	onWrite        func(*teamMembership) int
	resets         int
	schemaCalls    int
	noReset        bool
	onReset        func(*teamMembership) int
	onResetReceipt func(*spendResetReceipt)
}

func sampleTeam() teamInfo {
	var team teamInfo
	team.Team.ID, team.Team.Alias = "team-a", "backend"
	team.Team.Metadata = map[string]any{}
	for i, id := range []string{"alice", "bob", "admin"} {
		role := "user"
		if id == "admin" {
			role = "admin"
		}
		team.Team.Members = append(team.Team.Members, teamMember{UserID: id, UserEmail: id + "@example.com", Role: role})
		team.Memberships = append(team.Memberships, teamMembership{
			TeamID: "team-a", UserID: id, BudgetID: "budget-" + id,
			Spend: numberPointer(float64(i + 10)), TotalSpend: numberPointer(float64(i + 100)),
			Budget: map[string]any{
				"budget_id": "budget-" + id, "max_budget": float64(100),
				"budget_duration": "1d", "budget_reset_at": "2026-10-24T00:00:00Z",
				"tpm_limit": float64(1234), "rpm_limit": nil, "soft_budget": float64(90),
				"allowed_models": []any{"gpt-test"}, "updated_at": "2026-10-23T00:00:00Z",
			},
		})
	}
	return team
}

func (f *adminFixture) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer secret-test-key" {
			t.Error("管理请求缺少凭证")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/openapi.json":
			f.schemaCalls++
			paths := map[string]any{}
			if !f.noReset {
				paths[memberSpendResetRoute] = map[string]any{"post": map[string]any{}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"paths": paths})
		case r.Method == http.MethodGet && r.URL.Path == "/team/info":
			if r.URL.Query().Get("team_id") != f.team.Team.ID {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(f.team)
		case r.Method == http.MethodPost && r.URL.Path == "/team/member_update":
			f.posts++
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 3 ||
				body["team_id"] != f.team.Team.ID || body["max_budget_in_team"] == nil || body["user_id"] == nil {
				t.Errorf("请求必须且只能包含团队、用户和额度: %#v", body)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			for i := range f.team.Memberships {
				member := &f.team.Memberships[i]
				if member.UserID == body["user_id"] {
					member.Budget["max_budget"] = body["max_budget_in_team"]
					member.Budget["updated_at"] = "2026-10-23T10:00:00Z"
					if f.onWrite != nil {
						if code := f.onWrite(member); code != 0 {
							http.Error(w, "上游测试异常，包含不可返回的敏感内容", code)
							return
						}
					}
					_, _ = w.Write([]byte(`{}`))
					return
				}
			}
			http.Error(w, "not found", http.StatusNotFound)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reset_spend"):
			f.resets++
			if f.noReset {
				http.Error(w, "接口不存在", http.StatusNotFound)
				return
			}
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || body["reset_to"] != float64(0) {
				t.Errorf("清零请求只能携带 reset_to=0: %#v", body)
				http.Error(w, "清零参数错误", http.StatusBadRequest)
				return
			}
			for i := range f.team.Memberships {
				member := &f.team.Memberships[i]
				if r.URL.Path == "/team/"+f.team.Team.ID+"/member/"+member.UserID+"/reset_spend" {
					previous := *member.Spend
					member.Spend = numberPointer(float64(0))
					if f.onReset != nil {
						if code := f.onReset(member); code != 0 {
							http.Error(w, "清零响应异常", code)
							return
						}
					}
					receipt := spendResetReceipt{
						TeamID: member.TeamID, UserID: member.UserID,
						Spend: numberPointer(float64(0)), PreviousSpend: &previous,
						MaxBudget: numberPointer(member.Budget["max_budget"]),
					}
					if f.onResetReceipt != nil {
						f.onResetReceipt(&receipt)
					}
					_ = json.NewEncoder(w).Encode(receipt)
					return
				}
			}
			http.Error(w, "成员不存在", http.StatusNotFound)
		default:
			t.Errorf("非预期的 API 调用: %s %s", r.Method, r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
}

func newAdminTestService(t *testing.T) (*budgetAdminService, *adminFixture) {
	t.Helper()
	fixture := &adminFixture{team: sampleTeam()}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	api, err := newAdminClient(server.URL, "secret-test-key")
	if err != nil {
		t.Fatal(err)
	}
	store, err := newChangeStore(filepath.Join(t.TempDir(), "changes"))
	if err != nil {
		t.Fatal(err)
	}
	pool := budgetQueryFunc(func(_ context.Context, query string, args ...any) (pgx.Rows, error) {
		if len(args) != 1 || args[0] != "backend" || strings.Contains(query, "split_part") {
			return nil, fmt.Errorf("写入解析必须先按整个团队别名查询")
		}
		return &budgetRowsStub{data: [][]any{
			{"team-a", "backend", "alice", "alice@example.com", 10.0, 100.0, 100.0, "member_budget"},
			{"team-a", "backend", "bob", "bob@example.com", 11.0, 101.0, 100.0, "member_budget"},
			{"team-a", "backend", "admin", "admin@example.com", 12.0, 102.0, 100.0, "member_budget"},
		}}, nil
	})
	return &budgetAdminService{
		query: &budgetService{pool: pool}, api: api, safety: safetyFunc(func(context.Context, memberSnapshot) error { return nil }),
		store: store, owner: "test-owner", target: server.URL, max: 1000, now: time.Now,
	}, fixture
}

func previewForTest(t *testing.T, service *budgetAdminService, scope string) BudgetChangeOutput {
	t.Helper()
	input := PreviewBudgetInput{TeamAlias: "backend", Scope: scope, MaxBudget: 50}
	if scope == "member" {
		input.UserEmail = "bob@different.example"
	}
	_, output, err := service.Preview(context.Background(), nil, input)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func TestAdminPreviewApplyAndIdempotency(t *testing.T) {
	service, fixture := newAdminTestService(t)
	preview := previewForTest(t, service, "team_members")
	if preview.Count != 2 || fixture.posts != 0 || preview.State != "preview" {
		t.Fatalf("预览应排除管理员且不写入: %+v", preview)
	}
	for _, item := range preview.Members {
		if *item.OldMaxBudget != 100 || *item.BudgetDuration != "1d" || *item.BudgetResetAt != "2026-10-24T00:00:00Z" {
			t.Fatalf("预览缺少旧配置: %+v", item)
		}
	}
	fixture.mu.Lock()
	*fixture.team.Memberships[0].Spend += 2
	fixture.mu.Unlock()
	input := ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true}
	_, applied, err := service.Apply(context.Background(), nil, input)
	if err != nil || applied.State != "done" {
		t.Fatalf("执行失败: %+v %v", applied, err)
	}
	for _, item := range applied.Members {
		if item.Status != "applied" {
			t.Fatalf("消费自然增长不应阻止执行: %+v", applied)
		}
	}
	// 重新建立服务对象模拟重启，重复提交只读取已持久化结果。
	restarted := *service
	restarted.store, err = newChangeStore(service.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	_, repeated, err := restarted.Apply(context.Background(), nil, input)
	if err != nil || repeated.Members[0].Status != "applied" || fixture.posts != 2 {
		t.Fatalf("重启后不应重复写入: %+v %v 次数=%d", repeated, err, fixture.posts)
	}
	if fixture.team.Memberships[2].Budget["max_budget"] != float64(100) {
		t.Fatal("不得修改管理员预算")
	}
	info, err := os.Stat(filepath.Join(service.store.dir, preview.ChangeID+".json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("审计记录权限应为 0600: %v", err)
	}
}

func TestAdminConcurrentApply(t *testing.T) {
	service, fixture := newAdminTestService(t)
	preview := previewForTest(t, service, "member")
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			_, output, err := service.Apply(context.Background(), nil, ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true})
			if err != nil || output.Members[0].Status != "applied" {
				t.Errorf("并发执行失败: %+v %v", output, err)
			}
		})
	}
	wg.Wait()
	if fixture.posts != 1 {
		t.Fatalf("并发调用只能提交一次: %d", fixture.posts)
	}
}

func TestAdminValidation(t *testing.T) {
	for name, input := range map[string]PreviewBudgetInput{
		"缺少团队":  {Scope: "team_members", MaxBudget: 10},
		"缺少模式":  {TeamAlias: "backend", MaxBudget: 10},
		"缺少邮箱":  {TeamAlias: "backend", Scope: "member", MaxBudget: 10},
		"邮箱无效":  {TeamAlias: "backend", Scope: "member", UserEmail: "bob@", MaxBudget: 10},
		"批量带邮箱": {TeamAlias: "backend", Scope: "team_members", UserEmail: "bob", MaxBudget: 10},
		"额度为零":  {TeamAlias: "backend", Scope: "team_members"},
		"负数":    {TeamAlias: "backend", Scope: "team_members", MaxBudget: -1},
		"超过上限":  {TeamAlias: "backend", Scope: "team_members", MaxBudget: 101},
		"非数字":   {TeamAlias: "backend", Scope: "team_members", MaxBudget: math.NaN()},
		"无穷":    {TeamAlias: "backend", Scope: "team_members", MaxBudget: math.Inf(1)},
	} {
		t.Run(name, func(t *testing.T) {
			if validatePreview(input, 100) == nil {
				t.Fatal("无效输入必须拒绝")
			}
		})
	}
}

func TestAdminPreviewRejectsUnsafeTargets(t *testing.T) {
	for _, name := range []string{"共享预算", "没有预算", "身份重复", "预算字段缺失", "共享引用", "管理员", "同名前缀", "同名团队"} {
		t.Run(name, func(t *testing.T) {
			service, fixture := newAdminTestService(t)
			input := PreviewBudgetInput{TeamAlias: "backend", Scope: "member", UserEmail: "bob", MaxBudget: 50}
			switch name {
			case "共享预算":
				fixture.team.Team.Metadata["team_member_budget_id"] = "budget-bob"
			case "没有预算":
				fixture.team.Memberships[1].BudgetID = ""
			case "身份重复":
				fixture.team.Team.Members = append(fixture.team.Team.Members, fixture.team.Team.Members[1])
			case "预算字段缺失":
				delete(fixture.team.Memberships[1].Budget, "budget_reset_at")
			case "共享引用":
				service.safety = safetyFunc(func(context.Context, memberSnapshot) error { return errors.New("共享引用") })
			case "管理员":
				input.UserEmail = "admin"
			case "同名前缀", "同名团队":
				otherTeam := "team-a"
				if name == "同名团队" {
					otherTeam = "team-b"
				}
				service.query.pool = budgetQueryFunc(func(context.Context, string, ...any) (pgx.Rows, error) {
					return &budgetRowsStub{data: [][]any{
						{"team-a", "backend", "bob", "bob@a.com", 1.0, 1.0, 100.0, "member_budget"},
						{otherTeam, "backend", "bob2", "bob@b.com", 1.0, 1.0, 100.0, "member_budget"},
					}}, nil
				})
			}
			if _, _, err := service.Preview(context.Background(), nil, input); err == nil || fixture.posts != 0 {
				t.Fatalf("不安全预览必须拒绝且不能写入: %v", err)
			}
		})
	}
}

func TestAdminApplyGuards(t *testing.T) {
	for _, name := range []string{"未确认", "过期", "凭证变化", "目标变化", "金额上限降低", "配置变化", "角色变化", "共享检查失败"} {
		t.Run(name, func(t *testing.T) {
			service, fixture := newAdminTestService(t)
			preview := previewForTest(t, service, "member")
			input := ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true}
			wantConflict := false
			switch name {
			case "未确认":
				input.Confirm = false
			case "过期":
				service.now = func() time.Time { return preview.ExpiresAt }
			case "凭证变化":
				service.owner = "another"
			case "目标变化":
				service.target = "another"
			case "金额上限降低":
				service.max = 10
			case "配置变化":
				fixture.team.Memberships[1].Budget["budget_duration"] = "7d"
				wantConflict = true
			case "角色变化":
				fixture.team.Team.Members[1].Role = "admin"
				wantConflict = true
			case "共享检查失败":
				service.safety = safetyFunc(func(context.Context, memberSnapshot) error { return errors.New("无法验证") })
				wantConflict = true
			}
			_, output, err := service.Apply(context.Background(), nil, input)
			if wantConflict {
				if err != nil || output.Members[0].Status != "conflict" {
					t.Fatalf("应返回冲突: %+v %v", output, err)
				}
			} else if err == nil {
				t.Fatal("应拒绝执行")
			}
			if fixture.posts != 0 {
				t.Fatal("校验失败不得写入")
			}
		})
	}
}

func TestAdminReadbackAndPartialResults(t *testing.T) {
	for _, name := range []string{"响应异常但成功", "周期被修改", "消费被清零", "额度未生效", "第二项配置冲突"} {
		t.Run(name, func(t *testing.T) {
			service, fixture := newAdminTestService(t)
			preview := previewForTest(t, service, "team_members")
			fixture.onWrite = func(member *teamMembership) int {
				switch name {
				case "响应异常但成功":
					return http.StatusGatewayTimeout
				case "周期被修改":
					member.Budget["budget_reset_at"] = "2026-10-25T00:00:00Z"
				case "消费被清零":
					member.Spend = numberPointer(float64(0))
				case "额度未生效":
					member.Budget["max_budget"] = float64(100)
					return http.StatusBadGateway
				case "第二项配置冲突":
					fixture.team.Memberships[1].Budget["rpm_limit"] = float64(50)
				}
				return 0
			}
			input := ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true}
			_, output, err := service.Apply(context.Background(), nil, input)
			if err != nil {
				t.Fatal(err)
			}
			if name == "响应异常但成功" {
				if output.Members[0].Status != "applied" || output.Members[1].Status != "applied" {
					t.Fatalf("应通过回读确认: %+v", output)
				}
			} else if name == "第二项配置冲突" {
				if output.Members[0].Status != "applied" || output.Members[1].Status != "conflict" {
					t.Fatalf("应明确展示部分成功: %+v", output)
				}
			} else if output.Members[0].Status != "unconfirmed" || output.Members[1].Status != "skipped" {
				t.Fatalf("无法核实时必须停止后续项: %+v", output)
			}
			posts := fixture.posts
			_, _, err = service.Apply(context.Background(), nil, input)
			if err != nil || fixture.posts != posts {
				t.Fatal("不确定结果不得重复提交")
			}
		})
	}
}

func TestAdminInterruptedRecovery(t *testing.T) {
	service, fixture := newAdminTestService(t)
	preview := previewForTest(t, service, "team_members")
	change, err := service.store.load(preview.ChangeID)
	if err != nil {
		t.Fatal(err)
	}
	change.State, change.Items[0].Status = "executing", "inflight"
	if err := service.store.save(change); err != nil {
		t.Fatal(err)
	}
	fixture.team.Memberships[0].Budget["max_budget"] = float64(50)
	_, output, err := service.Apply(context.Background(), nil, ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true})
	if err != nil || output.Members[0].Status != "applied" || output.Members[1].Status != "skipped" || fixture.posts != 0 {
		t.Fatalf("中断恢复只能回读，不提交剩余成员: %+v %v", output, err)
	}
}

func TestAdminOverlappingPlansConflict(t *testing.T) {
	service, fixture := newAdminTestService(t)
	first := previewForTest(t, service, "member")
	second := previewForTest(t, service, "member")
	_, _, err := service.Apply(context.Background(), nil, ApplyBudgetInput{ChangeID: first.ChangeID, Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := service.Apply(context.Background(), nil, ApplyBudgetInput{ChangeID: second.ChangeID, Confirm: true})
	if err != nil || result.Members[0].Status != "conflict" || fixture.posts != 1 {
		t.Fatalf("不同计划不能覆盖预览后的变更: %+v %v", result, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestAdminLostResponseAndCancelledCallerReadback(t *testing.T) {
	service, fixture := newAdminTestService(t)
	preview := previewForTest(t, service, "member")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := service.api.(*adminClient)
	client.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(req)
		if err == nil && req.Method == http.MethodPost {
			response.Body.Close()
			// 上游已处理但调用方丢失响应；回读不能继承已经取消的上下文。
			cancel()
			return nil, context.DeadlineExceeded
		}
		return response, err
	})
	_, result, err := service.Apply(ctx, nil, ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true})
	if err != nil || result.Members[0].Status != "applied" || fixture.posts != 1 {
		t.Fatalf("响应丢失后应独立回读确认，不重复提交: %+v %v", result, err)
	}
}

func TestAdminEmailMissingFromTeamMember(t *testing.T) {
	service, fixture := newAdminTestService(t)
	fixture.team.Team.Members[1].UserEmail = ""
	preview := previewForTest(t, service, "member")
	if preview.Members[0].UserEmail != "bob@example.com" {
		t.Fatal("团队 API 未填邮箱时，预览应保留视图解析出的邮箱")
	}
	_, result, err := service.Apply(context.Background(), nil, ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true})
	if err != nil || result.Members[0].Status != "applied" {
		t.Fatalf("已确定用户 ID 时，不应要求 API 填充可选邮箱: %+v %v", result, err)
	}
}

func TestAdminCorruptRecordRejected(t *testing.T) {
	service, fixture := newAdminTestService(t)
	preview := previewForTest(t, service, "member")
	change, err := service.store.load(preview.ChangeID)
	if err != nil {
		t.Fatal(err)
	}
	change.Items[0].Before.Spend = nil
	if err := service.store.save(change); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Apply(context.Background(), nil, ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true}); err == nil || fixture.posts != 0 {
		t.Fatalf("损坏记录必须拒绝执行: %v", err)
	}
}

func TestAdminUnchangedAndStoreFailure(t *testing.T) {
	t.Run("额度相同不更新", func(t *testing.T) {
		service, fixture := newAdminTestService(t)
		_, preview, err := service.Preview(context.Background(), nil, PreviewBudgetInput{TeamAlias: "backend", Scope: "team_members", MaxBudget: 100})
		if err != nil {
			t.Fatal(err)
		}
		_, result, err := service.Apply(context.Background(), nil, ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true})
		if err != nil || result.Members[0].Status != "unchanged" || fixture.posts != 0 {
			t.Fatalf("相同额度不应更新: %+v %v", result, err)
		}
	})
	t.Run("提交前落盘失败不更新", func(t *testing.T) {
		service, fixture := newAdminTestService(t)
		preview := previewForTest(t, service, "member")
		service.safety = safetyFunc(func(context.Context, memberSnapshot) error {
			return os.Chmod(service.store.dir, 0500)
		})
		defer os.Chmod(service.store.dir, 0700)
		_, _, err := service.Apply(context.Background(), nil, ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true})
		if err == nil || fixture.posts != 0 {
			t.Fatalf("审计保存失败应阻止提交: %v", err)
		}
	})
}

func TestAdminClientRejectsRedirectAndSecrets(t *testing.T) {
	hit := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := newAdminClient(server.URL, "secret-test-key")
	if err != nil {
		t.Fatal(err)
	}
	err = client.Update(context.Background(), "team-a", "bob", 50)
	if err == nil || hit || strings.Contains(err.Error(), "secret-test-key") {
		t.Fatalf("不应跟随重定向或暴露密钥: %v", err)
	}
	for _, address := range []string{"file:///tmp/test", "https://user:password@example.com", "https://example.com?a=b", "https://example.com/#part"} {
		if _, err := newAdminClient(address, "key"); err == nil {
			t.Fatalf("不应接受地址 %s", address)
		}
	}
}

func TestChangeStoreInvalidIDAndLockCancellation(t *testing.T) {
	store, err := newChangeStore(filepath.Join(t.TempDir(), "changes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../secret", "", strings.Repeat("g", 48)} {
		if _, err := store.load(id); err == nil {
			t.Fatal("非法路径不应读取成功")
		}
	}
	unlock, err := store.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := store.lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("锁等待必须能取消: %v", err)
	}
}

func TestAdminConfig(t *testing.T) {
	names := []string{"MCP_WRITE_AUTH_TOKEN", "LITELLM_URL", "LITELLM_MASTER_KEY", "MCP_CHANGE_DIR", "MCP_MAX_MEMBER_BUDGET"}
	for _, name := range names {
		t.Setenv(name, "")
	}
	read := strings.Repeat("r", 32)
	if config, err := loadAdminConfig(read); err != nil || config != nil {
		t.Fatal("未配置应保持只读")
	}
	t.Setenv("LITELLM_URL", "http://localhost:4000")
	if _, err := loadAdminConfig(read); err == nil {
		t.Fatal("配置不完整不能静默忽略")
	}
	t.Setenv("LITELLM_MASTER_KEY", "key")
	t.Setenv("MCP_CHANGE_DIR", filepath.Join(t.TempDir(), "changes"))
	t.Setenv("MCP_MAX_MEMBER_BUDGET", "1000")
	t.Setenv("MCP_WRITE_AUTH_TOKEN", strings.Repeat("w", 32))
	config, err := loadAdminConfig(read)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.service(nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_WRITE_AUTH_TOKEN", read)
	if _, err := loadAdminConfig(read); err == nil {
		t.Fatal("读写令牌不能相同")
	}
	t.Setenv("MCP_WRITE_AUTH_TOKEN", strings.Repeat("w", 32))
	for _, max := range []string{"0", "-1", "NaN", "Inf", "bad"} {
		t.Setenv("MCP_MAX_MEMBER_BUDGET", max)
		if _, err := loadAdminConfig(read); err == nil {
			t.Fatalf("配置额度无效时必须拒绝: %s", max)
		}
	}
}
