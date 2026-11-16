package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestQueryTeamKeyModelTPMLimitReturnsMapsAndPages(t *testing.T) {
	query := budgetQueryFunc(func(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
		if !strings.Contains(sql, "FROM public.team_key_budget_view") ||
			!strings.Contains(sql, "key_model_tpm_limit::text") ||
			!strings.Contains(sql, "team_model_tpm_limit::text") || strings.Contains(sql, "key_alias = $2") {
			return nil, fmt.Errorf("查询没有按团队读取两种 TPM 映射: %s", sql)
		}
		if len(args) != 3 || args[0] != "math" || args[1] != 2 || args[2] != 0 {
			return nil, fmt.Errorf("团队分页参数错误: %#v", args)
		}
		return &budgetRowsStub{data: [][]any{
			{"team-1", "math", "batch-a", `{"gpt-6-sol":1200}`, `{"gpt-6-sol":5000}`},
			{"team-1", "math", "batch-b", `{}`, `{"gpt-6-sol":5000}`},
		}}, nil
	})
	service := &keyModelTPMQueryService{pool: query}
	_, output, err := service.QueryTeamKeyModelTPMLimit(context.Background(), nil, KeyModelTPMQueryInput{
		TeamAlias: "math", Page: 1, PageSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.Count != 1 || !output.HasMore || output.Page != 1 || output.PageSize != 1 || len(output.Records) != 1 {
		t.Fatalf("分页返回错误: %+v", output)
	}
	record := output.Records[0]
	if record.KeyAlias == nil || *record.KeyAlias != "batch-a" ||
		record.KeyModelTPMLimit["gpt-6-sol"] != 1200 || record.TeamModelTPMLimit["gpt-6-sol"] != 5000 {
		t.Fatalf("应返回 key 与团队的完整 TPM 映射: %+v", record)
	}
}

func TestQueryTeamKeyModelTPMLimitFiltersExactKeyAndPage(t *testing.T) {
	query := budgetQueryFunc(func(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
		if !strings.Contains(sql, "key_alias = $2") || len(args) != 4 ||
			args[0] != "math" || args[1] != "batch-a" || args[2] != 21 || args[3] != 20 {
			return nil, fmt.Errorf("key 精确筛选或分页参数错误: sql=%s args=%#v", sql, args)
		}
		return &budgetRowsStub{data: [][]any{{"team-1", "math", "batch-a", `{}`, `{}`}}}, nil
	})
	service := &keyModelTPMQueryService{pool: query}
	_, output, err := service.QueryTeamKeyModelTPMLimit(context.Background(), nil, KeyModelTPMQueryInput{
		TeamAlias: " math ", KeyAlias: " batch-a ", Page: 2, PageSize: 20,
	})
	if err != nil || output.Count != 1 || output.HasMore {
		t.Fatalf("key 别名筛选查询失败: %+v, err=%v", output, err)
	}
	if len(output.Records[0].KeyModelTPMLimit) != 0 || len(output.Records[0].TeamModelTPMLimit) != 0 {
		t.Fatalf("空配置应返回空映射: %+v", output.Records[0])
	}
}

func TestKeyModelTPMQueryRejectsMissingTeamBeforeDatabase(t *testing.T) {
	calls := 0
	service := &keyModelTPMQueryService{pool: budgetQueryFunc(func(context.Context, string, ...any) (pgx.Rows, error) {
		calls++
		return nil, nil
	})}
	if _, _, err := service.QueryTeamKeyModelTPMLimit(context.Background(), nil, KeyModelTPMQueryInput{KeyAlias: "batch-a"}); err == nil {
		t.Fatal("缺少团队条件时应拒绝查询")
	}
	if calls != 0 {
		t.Fatal("缺少团队条件时不能访问数据库")
	}
}

func TestKeyModelTPMQueryToolIsReadOnly(t *testing.T) {
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(budgetQueryFunc(func(context.Context, string, ...any) (pgx.Rows, error) {
		return nil, fmt.Errorf("列举工具时不应访问数据库")
	})).Connect(context.Background(), serverTransport, nil)
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
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for _, item := range list.Tools {
		if item.Name == "query_team_key_model_tpm_limit" {
			tool = item
		}
	}
	if tool == nil || tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
		t.Fatalf("TPM 查询工具应注册为只读工具: %+v", tool)
	}
	data, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(data, &schema); err != nil || len(schema.Required) != 1 || schema.Required[0] != "team_alias" {
		t.Fatalf("team_alias 应是唯一必填过滤条件: %s, err=%v", data, err)
	}
}
