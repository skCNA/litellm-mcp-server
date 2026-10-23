package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5"
)

type budgetSafety interface {
	Check(context.Context, memberSnapshot) error
}

type postgresBudgetSafety struct{ pool budgetQuerier }

// 发现所有 LiteLLM budget_id 列及指向预算表的外键，避免仅检查团队成员而漏掉密钥、客户等引用。
const budgetReferencesSQL = `
SELECT DISTINCT ns.nspname, c.relname, a.attname, c.relrowsecurity
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace ns ON ns.oid = c.relnamespace
JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
WHERE c.relkind IN ('r', 'p') AND (
 (ns.nspname = 'public' AND c.relname LIKE 'LiteLLM_%' AND a.attname = 'budget_id')
 OR EXISTS (
   SELECT 1 FROM pg_catalog.pg_constraint f
   WHERE f.contype = 'f' AND f.confrelid = 'public."LiteLLM_BudgetTable"'::regclass
     AND f.conrelid = c.oid AND a.attnum = ANY(f.conkey)
 ))
ORDER BY ns.nspname, c.relname, a.attname`

func (s *postgresBudgetSafety) Check(ctx context.Context, target memberSnapshot) error {
	ctx, cancel := context.WithTimeout(ctx, budgetQueryTimeout)
	defer cancel()
	rows, err := s.pool.Query(ctx, budgetReferencesSQL)
	if err != nil {
		return fmt.Errorf("无法发现预算引用结构，拒绝写入")
	}
	type reference struct{ schema, table, column string }
	var refs []reference
	found := map[string]bool{}
	for rows.Next() {
		var r reference
		var rls bool
		if err = rows.Scan(&r.schema, &r.table, &r.column, &rls); err != nil || rls {
			rows.Close()
			return fmt.Errorf("预算引用结构不可验证或启用了行级安全，拒绝写入")
		}
		refs = append(refs, r)
		if r.schema == "public" {
			found[r.table] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("读取预算引用结构失败")
	}
	// 这些表构成已核对版本的最低结构；缺表不能作为没有引用的证据。
	for _, name := range []string{"LiteLLM_BudgetTable", "LiteLLM_TeamMembership",
		"LiteLLM_VerificationToken", "LiteLLM_EndUserTable", "LiteLLM_TagTable",
		"LiteLLM_OrganizationTable", "LiteLLM_OrganizationMembership", "LiteLLM_ProjectTable"} {
		if !found[name] {
			return fmt.Errorf("缺少预算安全检查所需表 %s，拒绝写入", name)
		}
	}
	for _, r := range refs {
		if r.schema == "public" && r.table == "LiteLLM_BudgetTable" && r.column == "budget_id" {
			continue
		}
		table := pgx.Identifier{r.schema, r.table}.Sanitize()
		column := pgx.Identifier{r.column}.Sanitize()
		query := "SELECT count(*) FROM " + table + " WHERE " + column + " = $1"
		args := []any{target.BudgetID}
		if r.schema == "public" && r.table == "LiteLLM_TeamMembership" && r.column == "budget_id" {
			query += " AND (team_id IS DISTINCT FROM $2 OR user_id IS DISTINCT FROM $3)"
			args = append(args, target.TeamID, target.UserID)
		}
		count, err := s.count(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("无法读取 %s 的预算引用，请检查只读权限", table)
		}
		if count != 0 {
			return fmt.Errorf("预算被其他对象共享，拒绝修改")
		}
	}
	count, err := s.count(ctx, `
SELECT count(*) FROM pg_catalog.pg_class
WHERE oid = 'public."LiteLLM_TeamTable"'::regclass AND relrowsecurity`)
	if err != nil || count != 0 {
		return fmt.Errorf("无法完整检查团队默认预算引用")
	}
	count, err = s.count(ctx, `
SELECT count(*) FROM public."LiteLLM_TeamTable"
WHERE metadata->>'team_member_budget_id' = $1`, target.BudgetID)
	if err != nil || count != 0 {
		return fmt.Errorf("预算属于团队共享默认值或无法读取默认引用，拒绝修改")
	}
	// 核对 API 与数据库中的实际关联、成员身份和预算配置，防止两端误指不同部署。
	rows, err = s.pool.Query(ctx, `
SELECT to_jsonb(b), t.members_with_roles
FROM public."LiteLLM_TeamMembership" m
JOIN public."LiteLLM_BudgetTable" b ON b.budget_id = m.budget_id
JOIN public."LiteLLM_TeamTable" t ON t.team_id = m.team_id
WHERE m.team_id = $1 AND m.user_id = $2 AND m.budget_id = $3`,
		target.TeamID, target.UserID, target.BudgetID)
	if err != nil {
		return fmt.Errorf("无法核验数据库中的预算关联及配置")
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var budgetJSON, membersJSON []byte
		if err := rows.Scan(&budgetJSON, &membersJSON); err != nil {
			return fmt.Errorf("读取数据库预算关联失败")
		}
		var budget map[string]any
		var members []teamMember
		if json.Unmarshal(budgetJSON, &budget) != nil || json.Unmarshal(membersJSON, &members) != nil ||
			!reflect.DeepEqual(budgetSettings(budget), budgetSettings(target.Budget)) {
			return fmt.Errorf("数据库与 API 预算配置不一致，拒绝写入")
		}
		matches := 0
		for _, member := range members {
			if member.UserID == target.UserID && member.Role == "user" {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("数据库成员身份不是唯一普通成员")
		}
		n++
	}
	if rows.Err() != nil || n != 1 {
		return fmt.Errorf("数据库成员预算关联不唯一或不存在")
	}
	return nil
}

func (s *postgresBudgetSafety) count(ctx context.Context, query string, args ...any) (int64, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var count int64
	if !rows.Next() {
		return 0, fmt.Errorf("缺少引用计数")
	}
	if err := rows.Scan(&count); err != nil {
		return 0, err
	}
	if rows.Next() {
		return 0, fmt.Errorf("引用计数不唯一")
	}
	return count, rows.Err()
}
