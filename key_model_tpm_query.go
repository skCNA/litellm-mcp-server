package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type KeyModelTPMQueryInput struct {
	TeamAlias string `json:"team_alias" jsonschema:"LiteLLM 团队别名，精确匹配，必填"`
	KeyAlias  string `json:"key_alias,omitempty" jsonschema:"可选的 LiteLLM key 别名，精确匹配"`
	Page      int    `json:"page,omitempty" jsonschema:"页码，从 1 开始，默认 1"`
	PageSize  int    `json:"page_size,omitempty" jsonschema:"每页记录数，默认 50，最大 100"`
}

type KeyModelTPMRecord struct {
	TeamID            *string          `json:"team_id"`
	TeamAlias         *string          `json:"team_alias"`
	KeyAlias          *string          `json:"key_alias"`
	KeyModelTPMLimit  map[string]int64 `json:"key_model_tpm_limit"`
	TeamModelTPMLimit map[string]int64 `json:"team_model_tpm_limit"`
}

type KeyModelTPMQueryOutput struct {
	Records  []KeyModelTPMRecord `json:"records"`
	Count    int                 `json:"count"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
	HasMore  bool                `json:"has_more"`
}

type keyModelTPMQueryService struct {
	pool budgetQuerier
}

func (s *keyModelTPMQueryService) QueryTeamKeyModelTPMLimit(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input KeyModelTPMQueryInput,
) (*mcp.CallToolResult, KeyModelTPMQueryOutput, error) {
	input.TeamAlias = strings.TrimSpace(input.TeamAlias)
	input.KeyAlias = strings.TrimSpace(input.KeyAlias)
	_, args, page, pageSize, err := buildKeyBudgetQuery(KeyBudgetQueryInput{
		TeamAlias: input.TeamAlias, KeyAlias: input.KeyAlias, Page: input.Page, PageSize: input.PageSize,
	})
	if err != nil {
		return nil, KeyModelTPMQueryOutput{}, err
	}
	conditions := []string{"team_alias = $1"}
	if input.KeyAlias != "" {
		conditions = append(conditions, "key_alias = $2")
	}
	limitIndex, offsetIndex := len(args)-1, len(args)
	query := fmt.Sprintf(`
SELECT team_id, team_alias, key_alias,
       COALESCE(key_model_tpm_limit::text, '{}'),
       COALESCE(team_model_tpm_limit::text, '{}')
FROM %s
WHERE %s
ORDER BY team_id, key_alias NULLS LAST
LIMIT $%d OFFSET $%d`, keyBudgetViewName, strings.Join(conditions, " AND "), limitIndex, offsetIndex)

	queryCtx, cancel := context.WithTimeout(ctx, keyBudgetQueryTimeout)
	defer cancel()
	rows, err := s.pool.Query(queryCtx, query, args...)
	if err != nil {
		return nil, KeyModelTPMQueryOutput{}, fmt.Errorf("查询 LiteLLM key 模型 TPM 视图 %s 失败: %w", keyBudgetViewName, err)
	}
	defer rows.Close()

	records := make([]KeyModelTPMRecord, 0, pageSize)
	hasMore := false
	for rows.Next() {
		if len(records) == pageSize {
			hasMore = true
			break
		}
		record, err := scanKeyModelTPMRecord(rows)
		if err != nil {
			return nil, KeyModelTPMQueryOutput{}, fmt.Errorf("读取 key 模型 TPM 记录失败: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, KeyModelTPMQueryOutput{}, fmt.Errorf("遍历 key 模型 TPM 记录失败: %w", err)
	}
	return nil, KeyModelTPMQueryOutput{
		Records: records, Count: len(records), Page: page, PageSize: pageSize, HasMore: hasMore,
	}, nil
}

func scanKeyModelTPMRecord(rows interface{ Scan(...any) error }) (KeyModelTPMRecord, error) {
	var teamID, teamAlias, keyAlias, keyLimitsJSON, teamLimitsJSON pgtype.Text
	if err := rows.Scan(&teamID, &teamAlias, &keyAlias, &keyLimitsJSON, &teamLimitsJSON); err != nil {
		return KeyModelTPMRecord{}, err
	}
	keyLimits, err := decodeModelTPMLimitMap(keyLimitsJSON)
	if err != nil {
		return KeyModelTPMRecord{}, fmt.Errorf("key model_tpm_limit 数据结构无效")
	}
	teamLimits, err := decodeModelTPMLimitMap(teamLimitsJSON)
	if err != nil {
		return KeyModelTPMRecord{}, fmt.Errorf("team model_tpm_limit 数据结构无效")
	}
	return KeyModelTPMRecord{
		TeamID: textPointer(teamID), TeamAlias: textPointer(teamAlias), KeyAlias: textPointer(keyAlias),
		KeyModelTPMLimit: cloneModelTPMLimit(keyLimits), TeamModelTPMLimit: cloneModelTPMLimit(teamLimits),
	}, nil
}
