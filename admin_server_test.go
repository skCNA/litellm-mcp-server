package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type tokenTransport struct {
	token     string
	mu        sync.Mutex
	sessionID string
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	request := req.Clone(req.Context())
	request.Header.Set("Authorization", "Bearer "+t.token)
	response, err := http.DefaultTransport.RoundTrip(request)
	if err == nil && response.Header.Get("Mcp-Session-Id") != "" {
		t.mu.Lock()
		t.sessionID = response.Header.Get("Mcp-Session-Id")
		t.mu.Unlock()
	}
	return response, err
}

func TestAdminMCPHTTPIsolation(t *testing.T) {
	service, fixture := newAdminTestService(t)
	readToken, writeToken := strings.Repeat("r", 32), strings.Repeat("w", 32)
	server := httptest.NewServer(newAuthenticatedMCPHandler(readToken, writeToken, service.query.pool, service))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var writeSessionID string
	for _, token := range []string{writeToken, readToken} {
		transport := &tokenTransport{token: token}
		client := mcp.NewClient(&mcp.Implementation{Name: "budget-admin-test", Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: server.URL, HTTPClient: &http.Client{Transport: transport},
			DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		list, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token == writeToken {
			if len(list.Tools) != 3 {
				t.Fatalf("管理令牌应看到三个工具: %+v", list.Tools)
			}
			for _, tool := range list.Tools {
				if tool.Name != "preview_member_budget_change" {
					continue
				}
				data, err := json.Marshal(tool.InputSchema)
				if err != nil {
					t.Fatal(err)
				}
				var schema struct {
					Properties map[string]struct {
						Type string `json:"type"`
					} `json:"properties"`
					Required []string `json:"required"`
				}
				if json.Unmarshal(data, &schema) != nil || schema.Properties["reset_current_cycle_spend"].Type != "boolean" {
					t.Fatalf("清零参数必须是布尔类型: %s", data)
				}
				for _, required := range schema.Required {
					if required == "reset_current_cycle_spend" {
						t.Fatal("清零开关必须可省略")
					}
				}
			}
			transport.mu.Lock()
			writeSessionID = transport.sessionID
			transport.mu.Unlock()
			if writeSessionID == "" {
				t.Fatal("缺少管理员会话编号")
			}
			result, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "preview_member_budget_change",
				Arguments: map[string]any{"team_alias": "backend", "scope": "member", "user_email": "bob", "max_budget": 50, "reset_current_cycle_spend": "true"},
			})
			if err == nil && !result.IsError {
				t.Fatal("清零开关不接受字符串")
			}
			result, err = session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "preview_member_budget_change",
				Arguments: map[string]any{"team_alias": "backend", "scope": "member", "user_email": "bob", "max_budget": 50, "reset_current_cycle_spend": true},
			})
			if err != nil || result.IsError {
				t.Fatalf("MCP 预览失败: %+v %v", result, err)
			}
			data, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var preview BudgetChangeOutput
			if err := json.Unmarshal(data, &preview); err != nil || preview.Count != 1 || !preview.ResetCurrentCycleSpend {
				t.Fatalf("预览返回结构异常: %s %v", data, err)
			}
			result, err = session.CallTool(ctx, &mcp.CallToolParams{
				Name: "apply_member_budget_change", Arguments: map[string]any{"change_id": preview.ChangeID, "confirm": true, "reset_current_cycle_spend": false},
			})
			if err == nil && !result.IsError {
				t.Fatal("执行时不能覆盖预览中的清零选择")
			}
			if fixture.posts != 0 || fixture.resets != 0 {
				t.Fatal("无效执行参数不能提交写入")
			}
			result, err = session.CallTool(ctx, &mcp.CallToolParams{
				Name: "apply_member_budget_change", Arguments: map[string]any{"change_id": preview.ChangeID, "confirm": true},
			})
			if err != nil || result.IsError {
				t.Fatalf("MCP 执行失败: %+v %v", result, err)
			}
		} else {
			if len(list.Tools) != 1 || list.Tools[0].Name != "query_team_member_budget" {
				t.Fatalf("只读令牌不能发现管理工具: %+v", list.Tools)
			}
			for _, name := range []string{"preview_member_budget_change", "apply_member_budget_change"} {
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
				if err == nil && !result.IsError {
					t.Fatalf("只读令牌不应直接调用工具 %s", name)
				}
			}
			body := `{"jsonrpc":"2.0","id":55,"method":"tools/call","params":{"name":"apply_member_budget_change","arguments":{"change_id":"test","confirm":true}}}`
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+readToken)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Mcp-Session-Id", writeSessionID)
			req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("只读凭证不得借用管理会话，应返回404: %d", response.StatusCode)
			}
		}
	}
	if fixture.posts != 1 || fixture.resets != 1 {
		t.Fatalf("仅管理员调用应各执行一次额度更新和清零: 额度=%d 清零=%d", fixture.posts, fixture.resets)
	}
}
