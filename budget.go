package main

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	budgetViewName        = "public.team_member_budget_view"
	maxBudgetFilterLength = 255
	budgetQueryTimeout    = 10 * time.Second
)

type budgetQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type budgetService struct {
	pool budgetQuerier
}

type BudgetQueryInput struct {
	TeamAlias string `json:"team_alias,omitempty" jsonschema:"LiteLLM 团队别名，精确匹配；单独提供时返回该团队所有成员的完整费用与预算数据；与 user_email 至少提供一个"`
	UserEmail string `json:"user_email,omitempty" jsonschema:"用户邮箱或邮箱前缀，按 @ 前的完整内容区分大小写精确匹配，忽略域名；例如 bob@a.com 或 bob 均匹配 bob@b.com，但不匹配 bob1@b.com；与 team_alias 同时提供时共同筛选"`
}

type BudgetRecord struct {
	TeamID                  *string  `json:"team_id"`
	TeamAlias               *string  `json:"team_alias"`
	UserID                  *string  `json:"user_id"`
	UserEmail               *string  `json:"user_email"`
	MemberCurrentCycleSpend *float64 `json:"member_current_cycle_spend"`
	MemberTotalSpend        *float64 `json:"member_total_spend"`
	MemberMaxBudget         *float64 `json:"member_max_budget"`
	BudgetSource            *string  `json:"budget_source"`
}

type BudgetQueryOutput struct {
	Records []BudgetRecord `json:"records"`
	Count   int            `json:"count"`
}

func (s *budgetService) QueryTeamMemberBudget(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input BudgetQueryInput,
) (*mcp.CallToolResult, BudgetQueryOutput, error) {
	query, args, err := buildBudgetQuery(input)
	if err != nil {
		return nil, BudgetQueryOutput{}, err
	}

	queryCtx, cancel := context.WithTimeout(ctx, budgetQueryTimeout)
	defer cancel()

	rows, err := s.pool.Query(queryCtx, query, args...)
	if err != nil {
		return nil, BudgetQueryOutput{}, fmt.Errorf("查询 LiteLLM 团队成员预算视图 %s 失败: %w", budgetViewName, err)
	}
	defer rows.Close()

	records := make([]BudgetRecord, 0)
	for rows.Next() {
		record, err := scanBudgetRecord(rows)
		if err != nil {
			return nil, BudgetQueryOutput{}, fmt.Errorf("读取团队成员预算记录失败: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, BudgetQueryOutput{}, fmt.Errorf("遍历团队成员预算记录失败: %w", err)
	}

	return nil, BudgetQueryOutput{
		Records: records,
		Count:   len(records),
	}, nil
}

func buildBudgetQuery(input BudgetQueryInput) (string, []any, error) {
	input.TeamAlias = strings.TrimSpace(input.TeamAlias)
	input.UserEmail = strings.TrimSpace(input.UserEmail)
	if input.TeamAlias == "" && input.UserEmail == "" {
		return "", nil, fmt.Errorf("必须至少提供 team_alias 或 user_email")
	}
	if len(input.TeamAlias) > maxBudgetFilterLength {
		return "", nil, fmt.Errorf("team_alias 长度超出限制")
	}
	if len(input.UserEmail) > maxBudgetFilterLength {
		return "", nil, fmt.Errorf("user_email 长度超出限制")
	}
	if input.UserEmail != "" {
		prefix, domain, hasDomain := strings.Cut(input.UserEmail, "@")
		if prefix == "" || (hasDomain && (domain == "" || strings.Contains(domain, "@"))) ||
			strings.IndexFunc(input.UserEmail, func(r rune) bool {
				return unicode.IsSpace(r) || unicode.IsControl(r)
			}) >= 0 {
			return "", nil, fmt.Errorf("user_email 必须是非空邮箱前缀或完整邮箱，不能包含内部空白、控制字符或多个 @")
		}
		input.UserEmail = prefix
	}

	// 只拼接固定列名和占位符，所有筛选值均通过绑定参数传递。
	conditions := make([]string, 0, 2)
	args := make([]any, 0, 2)
	if input.TeamAlias != "" {
		args = append(args, input.TeamAlias)
		conditions = append(conditions, fmt.Sprintf("team_alias = $%d", len(args)))
	}
	if input.UserEmail != "" {
		args = append(args, input.UserEmail)
		// 比较完整邮箱前缀，忽略域名，但不将 bob 和 bob1 视为同一用户。
		conditions = append(conditions, fmt.Sprintf("split_part(user_email, '@', 1) = $%d", len(args)))
	}
	query := fmt.Sprintf(`
SELECT
    team_id,
    team_alias,
    user_id,
    user_email,
    member_current_cycle_spend,
    member_total_spend,
    member_max_budget,
    budget_source
FROM %s
WHERE %s
ORDER BY team_alias, user_email, team_id, user_id`, budgetViewName, strings.Join(conditions, " AND "))

	return query, args, nil
}

func scanBudgetRecord(rows pgx.Rows) (BudgetRecord, error) {
	var (
		teamID                  pgtype.Text
		teamAlias               pgtype.Text
		userID                  pgtype.Text
		userEmail               pgtype.Text
		memberCurrentCycleSpend pgtype.Float8
		memberTotalSpend        pgtype.Float8
		memberMaxBudget         pgtype.Float8
		budgetSource            pgtype.Text
	)
	if err := rows.Scan(
		&teamID,
		&teamAlias,
		&userID,
		&userEmail,
		&memberCurrentCycleSpend,
		&memberTotalSpend,
		&memberMaxBudget,
		&budgetSource,
	); err != nil {
		return BudgetRecord{}, err
	}

	return BudgetRecord{
		TeamID:                  textPointer(teamID),
		TeamAlias:               textPointer(teamAlias),
		UserID:                  textPointer(userID),
		UserEmail:               textPointer(userEmail),
		MemberCurrentCycleSpend: floatPointer(memberCurrentCycleSpend),
		MemberTotalSpend:        floatPointer(memberTotalSpend),
		MemberMaxBudget:         floatPointer(memberMaxBudget),
		BudgetSource:            textPointer(budgetSource),
	}, nil
}

func textPointer(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

// 保留数据库中的 NULL，避免将未知预算或费用误报成零。
func floatPointer(value pgtype.Float8) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}
