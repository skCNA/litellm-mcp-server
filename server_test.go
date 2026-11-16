package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBudgetToolInputSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	wantRows := [][]any{
		{"team-alpha", "研发团队", "user-bob", "bob@example.com", 38.20, 438.20, nil, "user"},
	}
	queryCount := 0
	store := budgetQueryFunc(func(_ context.Context, query string, args ...any) (pgx.Rows, error) {
		queryCount++
		if len(args) != 1 || args[0] != "bob" {
			return nil, fmt.Errorf("MCP 调用未正确传递筛选参数: %#v", args)
		}
		if !strings.Contains(query, "split_part(user_email, '@', 1) = $1") {
			return nil, fmt.Errorf("MCP 调用未使用邮箱前缀等值条件")
		}
		if !strings.Contains(query, "FROM public.team_member_budget_view\n") {
			return nil, fmt.Errorf("MCP 调用未查询 public 下的预算视图")
		}
		return &budgetRowsStub{data: wantRows}, nil
	})
	serverSession, err := newMCPServer(store).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("连接测试服务失败: %v", err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("连接测试客户端失败: %v", err)
	}
	defer session.Close()
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("获取工具定义失败: %v", err)
	}
	if len(result.Tools) != 2 {
		t.Fatalf("工具列表不符合预期: %+v", result.Tools)
	}
	var memberTool *mcp.Tool
	for i := range result.Tools {
		if result.Tools[i].Name == "query_team_member_budget" {
			memberTool = result.Tools[i]
		}
	}
	if memberTool == nil {
		t.Fatalf("缺少成员预算查询工具: %+v", result.Tools)
	}
	if !strings.Contains(memberTool.Description, "public.team_member_budget_view") {
		t.Fatalf("工具描述应与实际查询的 public 视图一致: %s", memberTool.Description)
	}
	data, err := json.Marshal(memberTool.InputSchema)
	if err != nil {
		t.Fatalf("序列化入参定义失败: %v", err)
	}
	var schema struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("解析入参定义失败: %v", err)
	}
	if len(schema.Properties) != 2 ||
		schema.Properties["team_alias"].Type != "string" ||
		schema.Properties["user_email"].Type != "string" {
		t.Fatalf("入参应仅包含两个字符串字段: %s", data)
	}
	if len(schema.Required) != 0 {
		t.Fatalf("两个字段均允许单独省略: %s", data)
	}
	if !strings.Contains(schema.Properties["user_email"].Description, "前缀") {
		t.Fatalf("邮箱入参描述应说明前缀匹配规则: %s", data)
	}

	outputSchema, err := json.Marshal(memberTool.OutputSchema)
	if err != nil {
		t.Fatalf("序列化返回结构失败: %v", err)
	}
	var outputDefinition struct {
		Properties map[string]struct {
			Items struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(outputSchema, &outputDefinition); err != nil {
		t.Fatalf("解析返回结构失败: %v", err)
	}
	fields := outputDefinition.Properties["records"].Items.Properties
	if len(fields) != 8 {
		t.Fatalf("返回结构应包含视图的完整八列: %s", outputSchema)
	}
	for _, field := range []string{"team_id", "team_alias", "user_id", "user_email",
		"member_current_cycle_spend", "member_total_spend", "member_max_budget", "budget_source"} {
		if _, ok := fields[field]; !ok {
			t.Fatalf("返回结构缺少 %s: %s", field, outputSchema)
		}
	}

	// 空条件应在访问数据库前被拒绝，避免意外查询所有用户。
	callResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "query_team_member_budget", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("调用工具失败: %v", err)
	}
	if !callResult.IsError {
		t.Fatal("缺少筛选条件时应返回工具错误")
	}
	if queryCount != 0 {
		t.Fatal("空条件不应访问数据库")
	}
	callResult, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "query_team_member_budget", Arguments: map[string]any{"team_alias": "研发团队", "user_email": "@example.com"},
	})
	if err != nil || !callResult.IsError || queryCount != 0 {
		t.Fatalf("无效前缀应在查询前被拒绝: 结果=%+v，错误=%v", callResult, err)
	}
	for i, email := range []string{"bob@example.com", "bob@other.example", "bob"} {
		callResult, err = session.CallTool(ctx, &mcp.CallToolParams{
			Name: "query_team_member_budget", Arguments: map[string]any{"user_email": email},
		})
		if err != nil {
			t.Fatalf("调用工具失败: %v", err)
		}
		if callResult.IsError || queryCount != i+1 {
			t.Fatalf("每次查询应成功执行一次: %+v", callResult)
		}
		assertBudgetOutput(t, callResult.StructuredContent, wantRows)
	}
}

func TestBearerAuth(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	called := false
	handler := bearerAuth(token, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	testCases := []struct {
		name       string
		authHeader string
		wantStatus int
		wantCalled bool
	}{
		{name: "缺少凭证", wantStatus: http.StatusUnauthorized},
		{name: "错误凭证", authHeader: "Bearer wrong", wantStatus: http.StatusUnauthorized},
		{name: "正确凭证", authHeader: "Bearer " + token, wantStatus: http.StatusNoContent, wantCalled: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			called = false
			request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			request.Header.Set("Authorization", testCase.authHeader)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != testCase.wantStatus {
				t.Fatalf("状态码 = %d，期望 %d", response.Code, testCase.wantStatus)
			}
			if called != testCase.wantCalled {
				t.Fatalf("下游处理器调用状态 = %t，期望 %t", called, testCase.wantCalled)
			}
			if !testCase.wantCalled && response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("未授权响应应提供 Bearer 挑战头")
			}
		})
	}
}
