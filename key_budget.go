package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	keyBudgetViewName        = "public.team_key_budget_view"
	defaultKeyBudgetPageSize = 50
	maxKeyBudgetPageSize     = 100
	maxKeyBudgetFilterLength = 255
	keyBudgetQueryTimeout    = 10 * time.Second
)

type keyBudgetService struct {
	pool budgetQuerier
}

type KeyBudgetQueryInput struct {
	TeamAlias string `json:"team_alias" jsonschema:"LiteLLM 团队别名，精确匹配，必填"`
	KeyAlias  string `json:"key_alias,omitempty" jsonschema:"可选的 LiteLLM key 别名，精确匹配"`
	Page      int    `json:"page,omitempty" jsonschema:"页码，从 1 开始，默认 1"`
	PageSize  int    `json:"page_size,omitempty" jsonschema:"每页记录数，默认 50，最大 100"`
}

type KeyBudgetRecord struct {
	TeamID               *string    `json:"team_id"`
	TeamAlias            *string    `json:"team_alias"`
	KeyAlias             *string    `json:"key_alias"`
	KeyName              *string    `json:"key_name"`
	UserID               *string    `json:"user_id"`
	Spend                *float64   `json:"spend"`
	KeyMaxBudget         *float64   `json:"key_max_budget"`
	BudgetTableMaxBudget *float64   `json:"budget_table_max_budget"`
	EffectiveMaxBudget   *float64   `json:"effective_max_budget"`
	BudgetDuration       *string    `json:"budget_duration"`
	BudgetResetAt        *time.Time `json:"budget_reset_at"`
	Expires              *time.Time `json:"expires"`
	Blocked              *bool      `json:"blocked"`
	CreatedAt            *time.Time `json:"created_at"`
}

type KeyBudgetQueryOutput struct {
	Records  []KeyBudgetRecord `json:"records"`
	Count    int               `json:"count"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
	HasMore  bool              `json:"has_more"`
}

func (s *keyBudgetService) QueryTeamKeyBudget(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input KeyBudgetQueryInput,
) (*mcp.CallToolResult, KeyBudgetQueryOutput, error) {
	query, args, page, pageSize, err := buildKeyBudgetQuery(input)
	if err != nil {
		return nil, KeyBudgetQueryOutput{}, err
	}

	queryCtx, cancel := context.WithTimeout(ctx, keyBudgetQueryTimeout)
	defer cancel()

	rows, err := s.pool.Query(queryCtx, query, args...)
	if err != nil {
		return nil, KeyBudgetQueryOutput{}, fmt.Errorf("查询 LiteLLM key 预算视图 %s 失败: %w", keyBudgetViewName, err)
	}
	defer rows.Close()

	records := make([]KeyBudgetRecord, 0, pageSize)
	hasMore := false
	for rows.Next() {
		if len(records) == pageSize {
			hasMore = true
			break
		}
		record, err := scanKeyBudgetRecord(rows)
		if err != nil {
			return nil, KeyBudgetQueryOutput{}, fmt.Errorf("读取 key 预算记录失败: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, KeyBudgetQueryOutput{}, fmt.Errorf("遍历 key 预算记录失败: %w", err)
	}

	return nil, KeyBudgetQueryOutput{
		Records: records, Count: len(records), Page: page, PageSize: pageSize, HasMore: hasMore,
	}, nil
}

func (s *keyBudgetService) QueryKeyAlias(ctx context.Context, keyAlias string) ([]KeyBudgetRecord, error) {
	keyAlias = strings.TrimSpace(keyAlias)
	if keyAlias == "" || len(keyAlias) > maxKeyBudgetFilterLength {
		return nil, fmt.Errorf("key_alias 必须提供且不超过 %d 字节", maxKeyBudgetFilterLength)
	}
	queryCtx, cancel := context.WithTimeout(ctx, keyBudgetQueryTimeout)
	defer cancel()
	query := fmt.Sprintf(`
SELECT
    team_id,
    team_alias,
    key_alias,
    key_name,
    user_id,
    spend,
    key_max_budget,
    budget_table_max_budget,
    effective_max_budget,
    budget_duration,
    budget_reset_at,
    expires,
    blocked,
    created_at
FROM %s
WHERE key_alias = $1
ORDER BY team_id NULLS LAST, key_alias
LIMIT 2`, keyBudgetViewName)
	rows, err := s.pool.Query(queryCtx, query, keyAlias)
	if err != nil {
		return nil, fmt.Errorf("检查 LiteLLM key 别名唯一性失败: %w", err)
	}
	defer rows.Close()
	records := make([]KeyBudgetRecord, 0, 2)
	for rows.Next() {
		record, err := scanKeyBudgetRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("读取 key 别名检查结果失败: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 key 别名检查结果失败: %w", err)
	}
	return records, nil
}

func buildKeyBudgetQuery(input KeyBudgetQueryInput) (string, []any, int, int, error) {
	input.TeamAlias = strings.TrimSpace(input.TeamAlias)
	input.KeyAlias = strings.TrimSpace(input.KeyAlias)
	if input.TeamAlias == "" {
		return "", nil, 0, 0, fmt.Errorf("必须提供 team_alias")
	}
	if len(input.TeamAlias) > maxKeyBudgetFilterLength {
		return "", nil, 0, 0, fmt.Errorf("team_alias 长度超出限制")
	}
	if len(input.KeyAlias) > maxKeyBudgetFilterLength {
		return "", nil, 0, 0, fmt.Errorf("key_alias 长度超出限制")
	}
	if input.Page < 0 {
		return "", nil, 0, 0, fmt.Errorf("page 必须大于等于 1")
	}
	if input.Page == 0 {
		input.Page = 1
	}
	if input.PageSize < 0 || input.PageSize > maxKeyBudgetPageSize {
		return "", nil, 0, 0, fmt.Errorf("page_size 必须介于 1 和 %d 之间", maxKeyBudgetPageSize)
	}
	if input.PageSize == 0 {
		input.PageSize = defaultKeyBudgetPageSize
	}
	if input.Page-1 > int(^uint(0)>>1)/input.PageSize {
		return "", nil, 0, 0, fmt.Errorf("page 与 page_size 组合超出分页范围")
	}

	conditions := []string{"team_alias = $1"}
	args := []any{input.TeamAlias}
	if input.KeyAlias != "" {
		args = append(args, input.KeyAlias)
		conditions = append(conditions, fmt.Sprintf("key_alias = $%d", len(args)))
	}
	args = append(args, input.PageSize+1, (input.Page-1)*input.PageSize)
	limitIndex := len(args) - 1
	offsetIndex := len(args)
	query := fmt.Sprintf(`
SELECT
    team_id,
    team_alias,
    key_alias,
    key_name,
    user_id,
    spend,
    key_max_budget,
    budget_table_max_budget,
    effective_max_budget,
    budget_duration,
    budget_reset_at,
    expires,
    blocked,
    created_at
FROM %s
WHERE %s
ORDER BY team_id, key_alias NULLS LAST, key_name NULLS LAST, user_id NULLS LAST,
    created_at NULLS LAST, spend, key_max_budget, budget_table_max_budget,
    effective_max_budget, budget_duration, budget_reset_at, expires, blocked
LIMIT $%d OFFSET $%d`, keyBudgetViewName, strings.Join(conditions, " AND "), limitIndex, offsetIndex)
	return query, args, input.Page, input.PageSize, nil
}

func scanKeyBudgetRecord(rows pgx.Rows) (KeyBudgetRecord, error) {
	var (
		teamID               pgtype.Text
		teamAlias            pgtype.Text
		keyAlias             pgtype.Text
		keyName              pgtype.Text
		userID               pgtype.Text
		spend                pgtype.Float8
		keyMaxBudget         pgtype.Float8
		budgetTableMaxBudget pgtype.Float8
		effectiveMaxBudget   pgtype.Float8
		budgetDuration       pgtype.Text
		budgetResetAt        pgtype.Timestamptz
		expires              pgtype.Timestamptz
		blocked              pgtype.Bool
		createdAt            pgtype.Timestamptz
	)
	if err := rows.Scan(
		&teamID, &teamAlias, &keyAlias, &keyName, &userID, &spend,
		&keyMaxBudget, &budgetTableMaxBudget, &effectiveMaxBudget, &budgetDuration,
		&budgetResetAt, &expires, &blocked, &createdAt,
	); err != nil {
		return KeyBudgetRecord{}, err
	}
	return KeyBudgetRecord{
		TeamID: textPointer(teamID), TeamAlias: textPointer(teamAlias),
		KeyAlias: textPointer(keyAlias), KeyName: textPointer(keyName), UserID: textPointer(userID),
		Spend: floatPointer(spend), KeyMaxBudget: floatPointer(keyMaxBudget),
		BudgetTableMaxBudget: floatPointer(budgetTableMaxBudget),
		EffectiveMaxBudget:   floatPointer(effectiveMaxBudget), BudgetDuration: textPointer(budgetDuration),
		BudgetResetAt: timestampPointer(budgetResetAt), Expires: timestampPointer(expires),
		Blocked: nullableBoolPointer(blocked), CreatedAt: timestampPointer(createdAt),
	}, nil
}

func timestampPointer(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

func nullableBoolPointer(value pgtype.Bool) *bool {
	if !value.Valid {
		return nil
	}
	return &value.Bool
}
