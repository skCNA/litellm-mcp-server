package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBuildKeyBudgetQueryRequiresTeamAndBindsFilters(t *testing.T) {
	query, args, page, pageSize, err := buildKeyBudgetQuery(KeyBudgetQueryInput{})
	if err == nil || !strings.Contains(err.Error(), "team_alias") || query != "" || args != nil {
		t.Fatalf("缺少团队时应拒绝查询，结果=%q，参数=%#v，错误=%v", query, args, err)
	}

	query, args, page, pageSize, err = buildKeyBudgetQuery(KeyBudgetQueryInput{
		TeamAlias: " math ", KeyAlias: " calc-1 ", Page: 2, PageSize: 20,
	})
	if err != nil {
		t.Fatalf("合法筛选应通过: %v", err)
	}
	if page != 2 || pageSize != 20 || !reflect.DeepEqual(args, []any{"math", "calc-1", 21, 20}) {
		t.Fatalf("归一化参数错误: page=%d page_size=%d args=%#v", page, pageSize, args)
	}
	if !strings.Contains(query, "team_alias = $1 AND key_alias = $2") ||
		!strings.Contains(query, "LIMIT $3 OFFSET $4") ||
		!strings.Contains(query, "public.team_key_budget_view") {
		t.Fatalf("查询条件、分页或视图错误: %s", query)
	}
	for _, arg := range args[:2] {
		if strings.Contains(query, arg.(string)) {
			t.Fatalf("筛选值不应拼接进 SQL: %s", query)
		}
	}
	if strings.Contains(strings.ToLower(query), " token") {
		t.Fatalf("查询不能读取 key token/hash: %s", query)
	}
}

func TestBuildKeyBudgetQueryValidatesPagination(t *testing.T) {
	tests := []struct {
		name  string
		input KeyBudgetQueryInput
		want  string
	}{
		{name: "页码不能为负数", input: KeyBudgetQueryInput{TeamAlias: "math", Page: -1}, want: "page"},
		{name: "页大小不能为负数", input: KeyBudgetQueryInput{TeamAlias: "math", PageSize: -1}, want: "page_size"},
		{name: "页大小有上限", input: KeyBudgetQueryInput{TeamAlias: "math", PageSize: 101}, want: "page_size"},
		{name: "团队别名不能为空", input: KeyBudgetQueryInput{TeamAlias: "  "}, want: "team_alias"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, _, err := buildKeyBudgetQuery(tt.input)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("应拒绝输入并指出 %s，错误=%v", tt.want, err)
			}
		})
	}
	_, args, page, pageSize, err := buildKeyBudgetQuery(KeyBudgetQueryInput{TeamAlias: "math"})
	if err != nil || page != 1 || pageSize != defaultKeyBudgetPageSize ||
		!reflect.DeepEqual(args, []any{"math", defaultKeyBudgetPageSize + 1, 0}) {
		t.Fatalf("省略分页参数应使用默认值: page=%d page_size=%d args=%#v err=%v", page, pageSize, args, err)
	}
}

func TestQueryTeamKeyBudgetReturnsOnlyRequestedPage(t *testing.T) {
	createdAt := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	data := [][]any{
		{"team-math", "math", "solver", "Solver bot", "user-1", 12.5, 100.0, 80.0, 80.0, "30d", createdAt, nil, false, createdAt},
		{"team-math", "math", "integral", "Integral bot", nil, 5.0, nil, 50.0, 50.0, nil, nil, nil, false, createdAt},
	}
	var gotQuery string
	var gotArgs []any
	rows := &budgetRowsStub{data: data}
	service := &keyBudgetService{pool: budgetQueryFunc(func(_ context.Context, query string, args ...any) (pgx.Rows, error) {
		gotQuery, gotArgs = query, args
		return rows, nil
	})}
	_, output, err := service.QueryTeamKeyBudget(context.Background(), nil, KeyBudgetQueryInput{
		TeamAlias: "math", PageSize: 1,
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(output.Records) != 1 || output.Count != 1 || output.Page != 1 ||
		output.PageSize != 1 || !output.HasMore {
		t.Fatalf("分页结果错误: %+v", output)
	}
	if output.Records[0].EffectiveMaxBudget == nil || *output.Records[0].EffectiveMaxBudget != 80 ||
		output.Records[0].KeyMaxBudget == nil || *output.Records[0].KeyMaxBudget != 100 ||
		output.Records[0].BudgetTableMaxBudget == nil || *output.Records[0].BudgetTableMaxBudget != 80 {
		t.Fatalf("额度字段未完整返回: %+v", output.Records[0])
	}
	if !rows.closed || !strings.Contains(gotQuery, "LIMIT $2 OFFSET $3") ||
		!reflect.DeepEqual(gotArgs, []any{"math", 2, 0}) {
		t.Fatalf("查询未正确分页或关闭: query=%s args=%#v closed=%t", gotQuery, gotArgs, rows.closed)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token", "tpm_limit", "rpm_limit"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("响应包含禁止字段 %q: %s", forbidden, encoded)
		}
	}
}

func TestQueryTeamKeyBudgetErrorsDoNotReturnPartialRecords(t *testing.T) {
	failure := errors.New("模拟 key 预算查询失败")
	called := false
	service := &keyBudgetService{pool: budgetQueryFunc(func(context.Context, string, ...any) (pgx.Rows, error) {
		called = true
		return nil, failure
	})}
	_, output, err := service.QueryTeamKeyBudget(context.Background(), nil, KeyBudgetQueryInput{TeamAlias: "math"})
	if !called || !errors.Is(err, failure) || output.Records != nil || output.Count != 0 {
		t.Fatalf("查询失败应传播错误且不返回部分数据: output=%+v err=%v", output, err)
	}
}

func TestKeyBudgetQueryRowsPreserveNulls(t *testing.T) {
	row := []any{"team-math", "math", nil, nil, nil, 0.0, nil, nil, nil, nil, nil, nil, nil, nil}
	service := &keyBudgetService{pool: budgetQueryFunc(func(context.Context, string, ...any) (pgx.Rows, error) {
		return &budgetRowsStub{data: [][]any{row}}, nil
	})}
	_, output, err := service.QueryTeamKeyBudget(context.Background(), nil, KeyBudgetQueryInput{TeamAlias: "math"})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if output.Records[0].KeyAlias != nil || output.Records[0].KeyName != nil ||
		output.Records[0].KeyMaxBudget != nil || output.Records[0].BudgetTableMaxBudget != nil ||
		output.Records[0].EffectiveMaxBudget != nil {
		t.Fatalf("数据库 NULL 应保留为 nil: %+v", output.Records[0])
	}
}

func TestKeyBudgetQueryOutputHasStableEmptyPage(t *testing.T) {
	service := &keyBudgetService{pool: budgetQueryFunc(func(context.Context, string, ...any) (pgx.Rows, error) {
		return &budgetRowsStub{}, nil
	})}
	_, output, err := service.QueryTeamKeyBudget(context.Background(), nil, KeyBudgetQueryInput{TeamAlias: "math"})
	if err != nil || output.Records == nil || len(output.Records) != 0 || output.Count != 0 || output.HasMore {
		t.Fatalf("无结果应返回稳定空页: %+v err=%v", output, err)
	}
}

func TestKeyBudgetQueryScanFailureIsReported(t *testing.T) {
	failure := fmt.Errorf("invalid database row")
	service := &keyBudgetService{pool: budgetQueryFunc(func(context.Context, string, ...any) (pgx.Rows, error) {
		return &budgetRowsStub{data: [][]any{{}}, scanErr: failure}, nil
	})}
	_, output, err := service.QueryTeamKeyBudget(context.Background(), nil, KeyBudgetQueryInput{TeamAlias: "math"})
	if err == nil || output.Count != 0 {
		t.Fatalf("扫描异常不应成功或返回记录: %+v err=%v", output, err)
	}
}

func TestKeyBudgetMCPToolIsReadOnlyAndUsesBoundedPage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	createdAt := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	store := budgetQueryFunc(func(_ context.Context, query string, args ...any) (pgx.Rows, error) {
		if !strings.Contains(query, "FROM public.team_key_budget_view") ||
			!strings.Contains(query, "LIMIT $2 OFFSET $3") ||
			strings.Contains(query, "token") {
			return nil, fmt.Errorf("工具查询超出 key 预算视图范围: %s", query)
		}
		if !reflect.DeepEqual(args, []any{"math", 2, 0}) {
			return nil, fmt.Errorf("MCP 调用未正确传递团队和分页参数: %#v", args)
		}
		row := []any{"team-math", "math", "solver", "Solver bot", "user-1", 12.5, 100.0, 80.0, 80.0, "30d", createdAt, nil, false, createdAt}
		return &budgetRowsStub{data: [][]any{row}}, nil
	})
	serverSession, err := newMCPServer(store).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("连接测试服务失败: %v", err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("连接测试客户端失败: %v", err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("获取工具列表失败: %v", err)
	}
	var keyTool *mcp.Tool
	for _, tool := range listed.Tools {
		if tool.Name == "query_team_key_budget" {
			keyTool = tool
		}
	}
	if keyTool == nil || keyTool.Annotations == nil || !keyTool.Annotations.ReadOnlyHint ||
		strings.Contains(strings.ToLower(keyTool.Description), "tpm") ||
		strings.Contains(strings.ToLower(keyTool.Description), "rpm") {
		t.Fatalf("只读 MCP 应提供不含速率限制的 key 预算工具: %+v", keyTool)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	data, err := json.Marshal(keyTool.InputSchema)
	if err != nil || json.Unmarshal(data, &schema) != nil {
		t.Fatalf("解析工具入参失败: %s %v", data, err)
	}
	if _, ok := schema.Properties["team_alias"]; !ok || len(schema.Required) != 1 || schema.Required[0] != "team_alias" {
		t.Fatalf("team_alias 应为唯一必填条件: %s", data)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "query_team_key_budget", Arguments: map[string]any{"team_alias": "math", "page_size": 1},
	})
	if err != nil || result.IsError {
		t.Fatalf("MCP key 查询失败: %+v %v", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output KeyBudgetQueryOutput
	if err := json.Unmarshal(encoded, &output); err != nil || output.Count != 1 ||
		output.Records[0].KeyAlias == nil || *output.Records[0].KeyAlias != "solver" {
		t.Fatalf("MCP key 查询返回结构错误: %s %v", encoded, err)
	}
	var outputSchema struct {
		Properties map[string]struct {
			Items struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"items"`
		} `json:"properties"`
	}
	data, err = json.Marshal(keyTool.OutputSchema)
	if err != nil || json.Unmarshal(data, &outputSchema) != nil {
		t.Fatalf("解析工具返回结构失败: %s %v", data, err)
	}
	fields := outputSchema.Properties["records"].Items.Properties
	if _, ok := fields["tpm_limit"]; ok {
		t.Fatalf("首版不应公开 TPM 限制: %s", data)
	}
	if _, ok := fields["rpm_limit"]; ok {
		t.Fatalf("首版不应公开 RPM 限制: %s", data)
	}
	if _, ok := fields["token"]; ok {
		t.Fatalf("返回结构不应公开 token: %s", data)
	}
}

func TestTeamKeyBudgetViewDoesNotExposeKeyToken(t *testing.T) {
	data, err := os.ReadFile("sql/team_key_budget_view.sql")
	if err != nil {
		t.Fatalf("读取 key 预算视图定义失败: %v", err)
	}
	sql := strings.ToLower(string(data))
	if !strings.Contains(sql, "public.team_key_budget_view") ||
		!strings.Contains(sql, "grant select on public.team_key_budget_view to mcp_readonly") {
		t.Fatalf("视图应可由只读账号查询: %s", sql)
	}
	if strings.Contains(sql, "vt.token") || strings.Contains(sql, "tpm_limit") || strings.Contains(sql, "rpm_limit") {
		t.Fatalf("视图不应读取 key token 或包含首版之外的速率限制: %s", sql)
	}
}
