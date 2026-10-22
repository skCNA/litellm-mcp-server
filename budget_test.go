package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestBuildBudgetQueryRequiresFilter(t *testing.T) {
	for _, input := range []BudgetQueryInput{
		{},
		{TeamAlias: " \t ", UserEmail: "\n "},
	} {
		query, args, err := buildBudgetQuery(input)
		if err == nil || !strings.Contains(err.Error(), "至少提供") {
			t.Fatalf("缺少有效条件时应返回明确错误，实际为 %v", err)
		}
		if query != "" || args != nil {
			t.Fatal("无效条件不应生成查询")
		}
	}
}

func TestBuildBudgetQueryUsesBoundParameters(t *testing.T) {
	tests := []struct {
		name      string
		input     BudgetQueryInput
		wantWhere string
		wantArgs  []any
	}{
		{
			name:      "只按团队查询",
			input:     BudgetQueryInput{TeamAlias: "  研发团队 ", UserEmail: " \t"},
			wantWhere: "team_alias = $1",
			wantArgs:  []any{"研发团队"},
		},
		{
			name:      "backend 团队使用 public 视图",
			input:     BudgetQueryInput{TeamAlias: "backend"},
			wantWhere: "team_alias = $1",
			wantArgs:  []any{"backend"},
		},
		{
			name:      "只按邮箱查询",
			input:     BudgetQueryInput{TeamAlias: "\n ", UserEmail: " bob@example.com "},
			wantWhere: "split_part(user_email, '@', 1) = $1",
			wantArgs:  []any{"bob"},
		},
		{
			name:      "其他域名使用相同前缀",
			input:     BudgetQueryInput{UserEmail: "bob@company.example"},
			wantWhere: "split_part(user_email, '@', 1) = $1",
			wantArgs:  []any{"bob"},
		},
		{
			name:      "只传邮箱前缀",
			input:     BudgetQueryInput{UserEmail: " bob "},
			wantWhere: "split_part(user_email, '@', 1) = $1",
			wantArgs:  []any{"bob"},
		},
		{
			name:      "数字后缀作为前缀的一部分",
			input:     BudgetQueryInput{UserEmail: "bob1@example.com"},
			wantWhere: "split_part(user_email, '@', 1) = $1",
			wantArgs:  []any{"bob1"},
		},
		{
			name:      "保留前缀大小写",
			input:     BudgetQueryInput{UserEmail: "Bob@example.com"},
			wantWhere: "split_part(user_email, '@', 1) = $1",
			wantArgs:  []any{"Bob"},
		},
		{
			name:      "通配符和加号按原值绑定",
			input:     BudgetQueryInput{UserEmail: "bob_%+tag@example.com"},
			wantWhere: "split_part(user_email, '@', 1) = $1",
			wantArgs:  []any{"bob_%+tag"},
		},
		{
			name:      "团队和邮箱共同筛选",
			input:     BudgetQueryInput{TeamAlias: " 研发团队 ", UserEmail: " bob@example.com "},
			wantWhere: "team_alias = $1 AND split_part(user_email, '@', 1) = $2",
			wantArgs:  []any{"研发团队", "bob"},
		},
		{
			name:      "团队和裸前缀共同筛选",
			input:     BudgetQueryInput{TeamAlias: "研发团队", UserEmail: "bob1"},
			wantWhere: "team_alias = $1 AND split_part(user_email, '@', 1) = $2",
			wantArgs:  []any{"研发团队", "bob1"},
		},
		{
			name:      "特殊字符仍作为绑定参数",
			input:     BudgetQueryInput{TeamAlias: "' OR 1=1 --", UserEmail: "x'@example.com"},
			wantWhere: "team_alias = $1 AND split_part(user_email, '@', 1) = $2",
			wantArgs:  []any{"' OR 1=1 --", "x'"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, args, err := buildBudgetQuery(tt.input)
			if err != nil {
				t.Fatalf("构造查询失败: %v", err)
			}
			wantQuery := "SELECT team_id, team_alias, user_id, user_email, member_current_cycle_spend, " +
				"member_total_spend, member_max_budget, budget_source FROM public.team_member_budget_view WHERE " +
				tt.wantWhere + " ORDER BY team_alias, user_email, team_id, user_id"
			if strings.Join(strings.Fields(query), " ") != wantQuery {
				t.Fatalf("查询错误: %s", query)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Fatalf("绑定参数 = %#v，期望 %#v", args, tt.wantArgs)
			}
			for _, arg := range args {
				if strings.Contains(query, arg.(string)) {
					t.Fatal("筛选值不应拼接进 SQL")
				}
			}
		})
	}
}

func TestBuildBudgetQueryValidatesFilterLength(t *testing.T) {
	tests := []struct {
		name    string
		input   BudgetQueryInput
		wantErr string
	}{
		{
			name:  "团队别名长度上限",
			input: BudgetQueryInput{TeamAlias: " " + strings.Repeat("a", 255) + " "},
		},
		{
			name:  "邮箱长度上限",
			input: BudgetQueryInput{UserEmail: " " + strings.Repeat("a", 243) + "@example.com "},
		},
		{
			name:  "裸前缀长度上限",
			input: BudgetQueryInput{UserEmail: strings.Repeat("a", 255)},
		},
		{
			name:    "团队别名超长",
			input:   BudgetQueryInput{TeamAlias: strings.Repeat("a", 256), UserEmail: "bob@example.com"},
			wantErr: "team_alias",
		},
		{
			name:    "邮箱超长",
			input:   BudgetQueryInput{TeamAlias: "研发团队", UserEmail: strings.Repeat("a", 244) + "@example.com"},
			wantErr: "user_email",
		},
		{
			name:    "前缀合法但原始邮箱超长",
			input:   BudgetQueryInput{UserEmail: "bob@" + strings.Repeat("a", 252)},
			wantErr: "user_email",
		},
		{
			name:    "裸前缀超长",
			input:   BudgetQueryInput{UserEmail: strings.Repeat("a", 256)},
			wantErr: "user_email",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := buildBudgetQuery(tt.input)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("合法长度应通过校验: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("应返回 %s 长度错误，实际为 %v", tt.wantErr, err)
			}
		})
	}
}

func TestBuildBudgetQueryRejectsInvalidEmailPrefix(t *testing.T) {
	for _, email := range []string{
		"@", "@example.com", "bob@", "bob@@example.com", "bob@example@com",
		"bob smith@example.com", "bob@ example.com", "bob\tname", "bob\nname",
		"bob\u3000name@example.com", "bob\x00@example.com",
	} {
		t.Run(email, func(t *testing.T) {
			for _, teamAlias := range []string{"", "研发团队"} {
				query, args, err := buildBudgetQuery(BudgetQueryInput{TeamAlias: teamAlias, UserEmail: email})
				if err == nil || !strings.Contains(err.Error(), "user_email") {
					t.Fatalf("应拒绝无效邮箱前缀，实际为 %v", err)
				}
				if query != "" || args != nil {
					t.Fatal("邮箱无效时不能退化为按团队查询")
				}
			}
		})
	}
}

type budgetQueryFunc func(context.Context, string, ...any) (pgx.Rows, error)

func (f budgetQueryFunc) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	return f(ctx, query, args...)
}

// 仅实现查询服务实际使用的行读取方法，其余 pgx.Rows 方法不应被调用。
type budgetRowsStub struct {
	pgx.Rows
	data    [][]any
	index   int
	closed  bool
	scanErr error
	readErr error
}

func (r *budgetRowsStub) Next() bool {
	if r.index >= len(r.data) {
		return false
	}
	r.index++
	return true
}

func (r *budgetRowsStub) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	values := r.data[r.index-1]
	if len(dest) != len(values) {
		return fmt.Errorf("扫描列数 = %d，期望 %d", len(dest), len(values))
	}
	for i, target := range dest {
		scanner, ok := target.(interface{ Scan(any) error })
		if !ok {
			return fmt.Errorf("第 %d 列不支持扫描", i)
		}
		if err := scanner.Scan(values[i]); err != nil {
			return err
		}
	}
	return nil
}

func (r *budgetRowsStub) Err() error { return r.readErr }
func (r *budgetRowsStub) Close()     { r.closed = true }

func TestQueryTeamMemberBudgetReturnsCompleteData(t *testing.T) {
	alice := []any{"team-alpha", "研发团队", "user-alice", "alice@example.com", 12.75, 125.50, 200.0, "team"}
	bob := []any{"team-alpha", "研发团队", "user-bob", "bob@example.com", 38.20, 438.20, nil, "user"}
	bobOtherTeam := []any{"team-beta", "测试团队", "user-bob", "bob@company.example", 0.0, 0.0, 0.0, "team"}
	manyRows := make([][]any, 201)
	for i := range manyRows {
		manyRows[i] = []any{"team-alpha", "研发团队", fmt.Sprintf("user-%d", i), fmt.Sprintf("user-%d@example.com", i), 1.0, 2.0, 3.0, "team"}
	}
	tests := []struct {
		name  string
		input BudgetQueryInput
		rows  [][]any
	}{
		{name: "团队返回所有成员完整数据", input: BudgetQueryInput{TeamAlias: "研发团队"}, rows: [][]any{alice, bob}},
		{name: "邮箱返回跨团队完整数据", input: BudgetQueryInput{UserEmail: "bob@example.com"}, rows: [][]any{bob, bobOtherTeam}},
		{name: "其他域名返回完整邮箱不改写", input: BudgetQueryInput{UserEmail: "bob@other.example"}, rows: [][]any{bob, bobOtherTeam}},
		{name: "裸前缀返回跨域名完整数据", input: BudgetQueryInput{UserEmail: "bob"}, rows: [][]any{bob, bobOtherTeam}},
		{name: "双条件返回匹配用户完整数据", input: BudgetQueryInput{TeamAlias: "研发团队", UserEmail: "bob@example.com"}, rows: [][]any{bob}},
		{name: "超过两百条不截断", input: BudgetQueryInput{TeamAlias: "研发团队"}, rows: manyRows},
		{name: "没有匹配记录", input: BudgetQueryInput{UserEmail: "missing@example.com"}},
		{name: "保留所有空值", input: BudgetQueryInput{TeamAlias: "研发团队"}, rows: [][]any{{nil, "研发团队", nil, nil, nil, nil, nil, nil}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := &budgetRowsStub{data: tt.rows}
			var queryContext context.Context
			service := &budgetService{pool: budgetQueryFunc(func(ctx context.Context, _ string, _ ...any) (pgx.Rows, error) {
				queryContext = ctx
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > budgetQueryTimeout {
					t.Fatal("数据库查询应具有有效的超时限制")
				}
				return rows, nil
			})}
			_, output, err := service.QueryTeamMemberBudget(context.Background(), nil, tt.input)
			if err != nil {
				t.Fatalf("查询失败: %v", err)
			}
			assertBudgetOutput(t, output, tt.rows)
			if !rows.closed {
				t.Fatal("查询完成后应关闭结果集")
			}
			if !errors.Is(queryContext.Err(), context.Canceled) {
				t.Fatal("查询完成后应释放超时上下文")
			}
		})
	}
}

func assertBudgetOutput(t *testing.T, output any, wantRows [][]any) {
	t.Helper()
	data, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("返回数据无法序列化: %v", err)
	}
	var result struct {
		Records []map[string]any `json:"records"`
		Count   int              `json:"count"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("解析返回数据失败: %v", err)
	}
	if result.Count != len(wantRows) || len(result.Records) != len(wantRows) || result.Records == nil {
		t.Fatalf("返回记录数量或空数组格式错误: %s", data)
	}
	if strings.Contains(string(data), `"truncated"`) {
		t.Fatal("完整结果不应继续包含截断标记")
	}
	fields := []string{
		"team_id", "team_alias", "user_id", "user_email",
		"member_current_cycle_spend", "member_total_spend", "member_max_budget", "budget_source",
	}
	for i, record := range result.Records {
		if len(record) != len(fields) {
			t.Fatalf("第 %d 条记录应包含完整的八列: %#v", i, record)
		}
		for j, field := range fields {
			value, ok := record[field]
			if !ok || !reflect.DeepEqual(value, wantRows[i][j]) {
				t.Fatalf("第 %d 条记录的 %s = %#v，期望 %#v", i, field, value, wantRows[i][j])
			}
		}
	}
}

func TestQueryTeamMemberBudgetErrors(t *testing.T) {
	failure := errors.New("模拟数据库故障")
	validRow := []any{"team-alpha", "研发团队", "user-bob", "bob@example.com", 38.20, 438.20, nil, "user"}
	tests := []struct {
		name     string
		input    BudgetQueryInput
		queryErr error
		rows     *budgetRowsStub
		wantErr  error
	}{
		{name: "拒绝空条件"},
		{name: "无效邮箱不能退化为团队查询", input: BudgetQueryInput{TeamAlias: "研发团队", UserEmail: "@example.com"}},
		{name: "查询失败", input: BudgetQueryInput{TeamAlias: "研发团队"}, queryErr: failure, wantErr: failure},
		{name: "查询超时", input: BudgetQueryInput{UserEmail: "bob@example.com"}, queryErr: context.DeadlineExceeded, wantErr: context.DeadlineExceeded},
		{name: "扫描失败", input: BudgetQueryInput{TeamAlias: "研发团队"}, rows: &budgetRowsStub{data: [][]any{validRow}, scanErr: failure}, wantErr: failure},
		{name: "读取中断不返回部分数据", input: BudgetQueryInput{TeamAlias: "研发团队"}, rows: &budgetRowsStub{data: [][]any{validRow}, readErr: failure}, wantErr: failure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			service := &budgetService{pool: budgetQueryFunc(func(context.Context, string, ...any) (pgx.Rows, error) {
				called = true
				return tt.rows, tt.queryErr
			})}
			_, output, err := service.QueryTeamMemberBudget(context.Background(), nil, tt.input)
			if err == nil || (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) {
				t.Fatalf("错误未正确传播: %v", err)
			}
			if tt.wantErr == nil && called {
				t.Fatal("空条件不应访问数据库")
			}
			if len(output.Records) != 0 || output.Count != 0 {
				t.Fatal("失败时不应返回部分数据")
			}
			if tt.rows != nil && !tt.rows.closed {
				t.Fatal("失败后也应关闭结果集")
			}
		})
	}
}
