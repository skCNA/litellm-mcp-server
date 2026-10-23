package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type valueRows struct {
	pgx.Rows
	values [][]any
	index  int
	err    error
}

func (r *valueRows) Close() {}
func (r *valueRows) Next() bool {
	if r.index >= len(r.values) {
		return false
	}
	r.index++
	return true
}
func (r *valueRows) Err() error { return r.err }
func (r *valueRows) Scan(dest ...any) error {
	if r.index == 0 || len(dest) != len(r.values[r.index-1]) {
		return fmt.Errorf("测试记录列数不匹配")
	}
	for i, value := range r.values[r.index-1] {
		target := reflect.ValueOf(dest[i]).Elem()
		source := reflect.ValueOf(value)
		if !source.Type().AssignableTo(target.Type()) {
			return fmt.Errorf("测试列类型不匹配: %T 到 %T", value, dest[i])
		}
		target.Set(source)
	}
	return nil
}

func referenceRows() [][]any {
	var rows [][]any
	for _, name := range []string{"LiteLLM_BudgetTable", "LiteLLM_TeamMembership",
		"LiteLLM_VerificationToken", "LiteLLM_EndUserTable", "LiteLLM_TagTable",
		"LiteLLM_OrganizationTable", "LiteLLM_OrganizationMembership", "LiteLLM_ProjectTable"} {
		rows = append(rows, []any{"public", name, "budget_id", false})
	}
	return rows
}

func TestPostgresBudgetSafety(t *testing.T) {
	for _, scenario := range []string{"独立预算", "共享成员", "共享密钥", "共享客户", "共享标签", "共享组织", "共享组织成员", "共享项目",
		"共享团队默认值", "新增外键引用", "缺少表", "行级安全", "团队行级安全", "缺少只读权限", "预算配置不一致",
		"管理员身份", "缺少成员关系", "预算游标错误"} {
		t.Run(scenario, func(t *testing.T) {
			team := sampleTeam()
			snapshot, err := snapshotMember(team, "bob")
			if err != nil {
				t.Fatal(err)
			}
			memberReferences := map[string]string{
				"共享成员": "LiteLLM_TeamMembership", "共享密钥": "LiteLLM_VerificationToken",
				"共享客户": "LiteLLM_EndUserTable", "共享标签": "LiteLLM_TagTable",
				"共享组织": "LiteLLM_OrganizationTable", "共享组织成员": "LiteLLM_OrganizationMembership",
				"共享项目": "LiteLLM_ProjectTable", "新增外键引用": "custom_refs",
			}
			var queries []string
			pool := budgetQueryFunc(func(_ context.Context, query string, args ...any) (pgx.Rows, error) {
				queries = append(queries, query)
				if query == budgetReferencesSQL {
					refs := referenceRows()
					switch scenario {
					case "缺少表":
						refs = refs[:len(refs)-1]
					case "行级安全":
						refs[1][3] = true
					case "新增外键引用":
						refs = append(refs, []any{"other", "custom_refs", "member_budget", false})
					}
					return &valueRows{values: refs}, nil
				}
				if strings.Contains(query, "SELECT to_jsonb(b)") {
					if scenario == "缺少成员关系" {
						return &valueRows{}, nil
					}
					budget := budgetSettings(snapshot.Budget)
					// PostgreSQL 与 API 可用不同但等价的时区格式。
					budget["budget_reset_at"] = "2026-10-24T00:00:00"
					if scenario == "预算配置不一致" {
						budget["max_budget"] = float64(200)
					}
					members := append([]teamMember(nil), team.Team.Members...)
					if scenario == "管理员身份" {
						members[1].Role = "admin"
					}
					budgetJSON, _ := json.Marshal(budget)
					membersJSON, _ := json.Marshal(members)
					rows := &valueRows{values: [][]any{{budgetJSON, membersJSON}}}
					if scenario == "预算游标错误" {
						rows.err = errors.New("游标失败")
					}
					return rows, nil
				}
				if strings.HasPrefix(query, "SELECT count(*) FROM") || strings.HasPrefix(query, "\nSELECT count(*) FROM") {
					count := int64(0)
					if ref, ok := memberReferences[scenario]; ok && strings.Contains(query, `"`+ref+`"`) {
						count = 1
					}
					if scenario == "共享团队默认值" && strings.Contains(query, "metadata->>") {
						count = 1
					}
					if scenario == "团队行级安全" && strings.Contains(query, "pg_catalog.pg_class") {
						count = 1
					}
					if strings.Contains(query, `FROM "public"."LiteLLM_VerificationToken"`) && scenario == "缺少只读权限" {
						return nil, errors.New("权限不足")
					}
					if strings.Contains(query, `FROM "public"."LiteLLM_TeamMembership"`) {
						if len(args) != 3 || args[0] != snapshot.BudgetID || args[1] != snapshot.TeamID || args[2] != snapshot.UserID ||
							!strings.Contains(query, "IS DISTINCT FROM") {
							return nil, errors.New("必须仅排除当前目标成员")
						}
					}
					return &valueRows{values: [][]any{{count}}}, nil
				}
				return nil, fmt.Errorf("非预期安全查询: %s", query)
			})
			err = (&postgresBudgetSafety{pool: pool}).Check(context.Background(), snapshot)
			if scenario == "独立预算" {
				if err != nil {
					t.Fatal(err)
				}
				if len(queries) != 11 {
					t.Fatalf("必须检查完整引用集合: %d", len(queries))
				}
			} else if err == nil {
				t.Fatal("不安全或无法核验时必须拒绝")
			}
		})
	}
}
