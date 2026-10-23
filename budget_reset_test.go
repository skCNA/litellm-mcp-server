package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func previewResetForTest(t *testing.T, service *budgetAdminService, scope string, amount float64) BudgetChangeOutput {
	t.Helper()
	input := PreviewBudgetInput{TeamAlias: "backend", Scope: scope, MaxBudget: amount, ResetCurrentCycleSpend: true}
	if scope == "member" {
		input.UserEmail = "bob@other.example"
	}
	_, preview, err := service.Preview(context.Background(), nil, input)
	if err != nil {
		t.Fatal(err)
	}
	return preview
}

func TestResetSpendDefaultOff(t *testing.T) {
	service, fixture := newAdminTestService(t)
	fixture.noReset = true
	preview := previewForTest(t, service, "member")
	_, result, err := service.Apply(context.Background(), nil, ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true})
	if err != nil || result.ResetCurrentCycleSpend || result.Members[0].ResetStatus != "not_requested" ||
		fixture.resets != 0 || fixture.schemaCalls != 0 || *fixture.team.Memberships[1].Spend != 11 {
		t.Fatalf("缺省不能检查或调用清零接口: %+v %v", result, err)
	}
}

func TestResetSpendPreviewApplyAndReplay(t *testing.T) {
	service, fixture := newAdminTestService(t)
	preview := previewResetForTest(t, service, "member", 50)
	if !preview.ResetCurrentCycleSpend || preview.Members[0].ResetStatus != "pending" ||
		preview.Members[0].TargetCurrentSpend == nil || *preview.Members[0].TargetCurrentSpend != 0 ||
		fixture.posts != 0 || fixture.resets != 0 {
		t.Fatalf("预览必须明确清零目标且不写入: %+v", preview)
	}
	change, err := service.store.load(preview.ChangeID)
	if err != nil || !change.ResetCurrentCycleSpend {
		t.Fatalf("清零意图必须持久化: %v", err)
	}
	input := ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true}
	_, result, err := service.Apply(context.Background(), nil, input)
	if err != nil || result.Members[0].Status != "applied" || result.Members[0].ResetStatus != "confirmed" ||
		!result.Members[0].BudgetConfirmed || *result.Members[0].ObservedCurrentSpend != 0 {
		t.Fatalf("额度与清零应同时完成: %+v %v", result, err)
	}
	member := fixture.team.Memberships[1]
	if *member.Spend != 0 || *member.TotalSpend != 101 || member.Budget["max_budget"] != float64(50) ||
		member.Budget["budget_duration"] != "1d" || member.Budget["budget_reset_at"] != "2026-10-24T00:00:00Z" ||
		fixture.posts != 1 || fixture.resets != 1 || fixture.schemaCalls != 2 {
		t.Fatal("只能修改额度及当前消费，不能改变累计消费或周期")
	}
	// 重复执行时必须保留第一次清零之后产生的新消费。
	fixture.team.Memberships[1].Spend = numberPointer(float64(7))
	restarted := *service
	restarted.store, err = newChangeStore(service.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	_, result, err = restarted.Apply(context.Background(), nil, input)
	if err != nil || fixture.resets != 1 || fixture.posts != 1 || *fixture.team.Memberships[1].Spend != 7 {
		t.Fatalf("重复调用或重启不得再次清零: %+v %v", result, err)
	}
}

func TestResetSpendWithSameBudget(t *testing.T) {
	service, fixture := newAdminTestService(t)
	preview := previewResetForTest(t, service, "member", 100)
	_, result, err := service.Apply(context.Background(), nil, ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true})
	if err != nil || result.Members[0].Status != "applied" || fixture.posts != 0 || fixture.resets != 1 {
		t.Fatalf("额度相同仍需执行明确请求的清零: %+v %v", result, err)
	}
}

func TestResetSpendTeamBatch(t *testing.T) {
	service, fixture := newAdminTestService(t)
	preview := previewResetForTest(t, service, "team_members", 50)
	_, result, err := service.Apply(context.Background(), nil, ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true})
	if err != nil || len(result.Members) != 2 || fixture.posts != 2 || fixture.resets != 2 {
		t.Fatalf("批量清零失败: %+v %v", result, err)
	}
	for _, member := range result.Members {
		if member.Status != "applied" || member.ResetStatus != "confirmed" {
			t.Fatalf("应逐项返回确认状态: %+v", member)
		}
	}
	if *fixture.team.Memberships[2].Spend != 12 || fixture.team.Memberships[2].Budget["max_budget"] != float64(100) {
		t.Fatal("批量不能清零或更新管理员")
	}
}

func TestResetSpendUnsupportedBeforeAnyWrite(t *testing.T) {
	service, fixture := newAdminTestService(t)
	fixture.noReset = true
	_, _, err := service.Preview(context.Background(), nil, PreviewBudgetInput{
		TeamAlias: "backend", Scope: "team_members", MaxBudget: 50, ResetCurrentCycleSpend: true,
	})
	if err == nil || !strings.Contains(err.Error(), "reset_spend") || fixture.posts != 0 || fixture.resets != 0 {
		t.Fatalf("缺少接口应在预览阶段拒绝: %v", err)
	}
	fixture.noReset = false
	preview := previewResetForTest(t, service, "member", 50)
	fixture.noReset = true
	_, _, err = service.Apply(context.Background(), nil, ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true})
	if err == nil || fixture.posts != 0 || fixture.resets != 0 {
		t.Fatalf("执行前能力消失不能先修改额度: %v", err)
	}
}

func TestResetSpendBudgetFailureSkipsReset(t *testing.T) {
	service, fixture := newAdminTestService(t)
	preview := previewResetForTest(t, service, "member", 50)
	fixture.onWrite = func(member *teamMembership) int {
		member.Budget["max_budget"] = float64(100)
		return http.StatusBadGateway
	}
	_, result, err := service.Apply(context.Background(), nil, ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true})
	if err != nil || result.Members[0].Status != "unconfirmed" || result.Members[0].ResetStatus != "skipped" ||
		fixture.resets != 0 || *fixture.team.Memberships[1].Spend != 11 {
		t.Fatalf("额度尚未确认时不得清零: %+v %v", result, err)
	}
}

func TestResetSpendReadbackBoundaries(t *testing.T) {
	for _, scenario := range []string{"正常新增消费", "失败未清零", "异常响应已清零", "累计消费被清空", "周期被修改", "无效回执且消费非零"} {
		t.Run(scenario, func(t *testing.T) {
			service, fixture := newAdminTestService(t)
			preview := previewResetForTest(t, service, "team_members", 50)
			fixture.onReset = func(member *teamMembership) int {
				switch scenario {
				case "正常新增消费":
					member.Spend = numberPointer(float64(120))
					member.TotalSpend = numberPointer(*member.TotalSpend + 120)
				case "失败未清零":
					member.Spend = numberPointer(float64(10))
					return http.StatusBadGateway
				case "异常响应已清零":
					return http.StatusGatewayTimeout
				case "累计消费被清空":
					member.TotalSpend = numberPointer(float64(0))
				case "周期被修改":
					member.Budget["budget_duration"] = "7d"
				case "无效回执且消费非零":
					member.Spend = numberPointer(float64(5))
				}
				return 0
			}
			if scenario == "无效回执且消费非零" {
				fixture.onResetReceipt = func(receipt *spendResetReceipt) { receipt.UserID = "other" }
			}
			input := ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true}
			_, result, err := service.Apply(context.Background(), nil, input)
			if err != nil {
				t.Fatal(err)
			}
			success := scenario == "正常新增消费" || scenario == "异常响应已清零"
			if success {
				if result.Members[0].Status != "applied" || result.Members[1].Status != "applied" || fixture.resets != 2 {
					t.Fatalf("有效回执加配置回读或观察到零，应确认完成: %+v", result)
				}
			} else if result.Members[0].Status != "unconfirmed" || result.Members[0].ResetStatus != "unconfirmed" ||
				!result.Members[0].BudgetConfirmed || result.Members[1].Status != "skipped" || fixture.resets != 1 || fixture.posts != 1 {
				t.Fatalf("清零无法确认应停止后续成员，但不能隐瞒额度已更新: %+v", result)
			}
			resets, posts := fixture.resets, fixture.posts
			_, _, err = service.Apply(context.Background(), nil, input)
			if err != nil || fixture.resets != resets || fixture.posts != posts {
				t.Fatal("完成或未确认状态均不能重复请求")
			}
		})
	}
}

func TestResetSpendCancelledAfterBudget(t *testing.T) {
	service, fixture := newAdminTestService(t)
	preview := previewResetForTest(t, service, "member", 50)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.api.(*adminClient).client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(req)
		if err == nil && req.URL.Path == "/team/member_update" {
			response.Body.Close()
			cancel()
			return nil, context.DeadlineExceeded
		}
		return response, err
	})
	_, result, err := service.Apply(ctx, nil, ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true})
	if err != nil || result.Members[0].Status != "partial" || !result.Members[0].BudgetConfirmed ||
		result.Members[0].ResetStatus != "skipped" || fixture.resets != 0 {
		t.Fatalf("取消后可以回读额度，但不能再开始清零: %+v %v", result, err)
	}
}

func TestResetSpendLostResponse(t *testing.T) {
	for _, newSpend := range []float64{0, 5} {
		t.Run(fmt.Sprintf("新增消费_%g", newSpend), func(t *testing.T) {
			service, fixture := newAdminTestService(t)
			preview := previewResetForTest(t, service, "member", 50)
			fixture.onReset = func(member *teamMembership) int {
				member.Spend = numberPointer(newSpend)
				member.TotalSpend = numberPointer(*member.TotalSpend + newSpend)
				return 0
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			service.api.(*adminClient).client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				response, err := http.DefaultTransport.RoundTrip(req)
				if err == nil && strings.HasSuffix(req.URL.Path, "/reset_spend") {
					response.Body.Close()
					cancel()
					return nil, context.DeadlineExceeded
				}
				return response, err
			})
			input := ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true}
			_, result, err := service.Apply(ctx, nil, input)
			want := "applied"
			if newSpend > 0 {
				want = "unconfirmed"
			}
			if err != nil || result.Members[0].Status != want || fixture.resets != 1 {
				t.Fatalf("丢失回执后只能按明确证据确认: %+v %v", result, err)
			}
			_, _, err = service.Apply(context.Background(), nil, input)
			if err != nil || fixture.resets != 1 {
				t.Fatal("不能重置清零后新增的消费")
			}
		})
	}
}

func TestResetSpendCrashRecovery(t *testing.T) {
	for _, scenario := range []string{"清零尚未开始", "已清零回执丢失", "已清零且有新消费", "无回执且消费非零"} {
		t.Run(scenario, func(t *testing.T) {
			service, fixture := newAdminTestService(t)
			preview := previewResetForTest(t, service, "team_members", 50)
			change, err := service.store.load(preview.ChangeID)
			if err != nil {
				t.Fatal(err)
			}
			change.State, change.Items[0].Status = "executing", "inflight"
			item := &change.Items[0]
			fixture.team.Memberships[0].Budget["max_budget"] = float64(50)
			if scenario != "清零尚未开始" {
				item.BudgetConfirmed = true
				item.ResetStatus = "inflight"
				item.ResetBeforeSpend = numberPointer(float64(10))
				item.ResetBeforeTotalSpend = numberPointer(float64(100))
				fixture.team.Memberships[0].Spend = numberPointer(float64(0))
			}
			if scenario == "已清零且有新消费" || scenario == "无回执且消费非零" {
				fixture.team.Memberships[0].Spend = numberPointer(float64(8))
				if scenario == "已清零且有新消费" {
					item.ResetReceipt = &spendResetReceipt{TeamID: "team-a", UserID: "alice", Spend: numberPointer(float64(0)), PreviousSpend: item.ResetBeforeSpend, MaxBudget: numberPointer(float64(50))}
				}
			}
			if err := service.store.save(change); err != nil {
				t.Fatal(err)
			}
			_, result, err := service.Apply(context.Background(), nil, ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true})
			want := "applied"
			if scenario == "清零尚未开始" {
				want = "partial"
			} else if scenario == "无回执且消费非零" {
				want = "unconfirmed"
			}
			if err != nil || result.Members[0].Status != want || result.Members[1].Status != "skipped" ||
				result.Members[1].ResetStatus != "skipped" || fixture.resets != 0 || fixture.posts != 0 {
				t.Fatalf("中断恢复只能读取，不能续写或重复清零: %+v %v", result, err)
			}
		})
	}
}

func TestResetSpendAuditFailures(t *testing.T) {
	for _, phase := range []string{"清零前", "清零回执保存时"} {
		t.Run(phase, func(t *testing.T) {
			service, fixture := newAdminTestService(t)
			preview := previewResetForTest(t, service, "member", 50)
			defer os.Chmod(service.store.dir, 0700)
			wantResets := 0
			if phase == "清零前" {
				checks := 0
				service.safety = safetyFunc(func(context.Context, memberSnapshot) error {
					checks++
					if checks == 3 {
						return os.Chmod(service.store.dir, 0500)
					}
					return nil
				})
			} else {
				wantResets = 1
				fixture.onReset = func(*teamMembership) int {
					if err := os.Chmod(service.store.dir, 0500); err != nil {
						t.Error(err)
					}
					return 0
				}
			}
			input := ApplyBudgetInput{ChangeID: preview.ChangeID, Confirm: true}
			if _, _, err := service.Apply(context.Background(), nil, input); err == nil ||
				fixture.posts != 1 || fixture.resets != wantResets {
				t.Fatalf("审计落盘失败必须停止: 错误=%v 额度请求=%d 清零请求=%d", err, fixture.posts, fixture.resets)
			}
			if err := os.Chmod(service.store.dir, 0700); err != nil {
				t.Fatal(err)
			}
			_, result, err := service.Apply(context.Background(), nil, input)
			wantStatus := "partial"
			if phase == "清零回执保存时" {
				wantStatus = "applied"
			}
			if err != nil || result.Members[0].Status != wantStatus || fixture.resets != wantResets || fixture.posts != 1 {
				t.Fatalf("恢复只能回读，不能重新提交: %+v %v", result, err)
			}
		})
	}
}

func TestResetClientEscapesIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/team/team%2Fa/member/user%2Fb%3F%23/reset_spend" || r.URL.RawQuery != "" {
			t.Errorf("标识必须作为独立路径参数编码: %s", r.URL.String())
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := newAdminClient(server.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ResetSpend(context.Background(), "team/a", "user/b?#"); err != nil {
		t.Fatal(err)
	}
}
