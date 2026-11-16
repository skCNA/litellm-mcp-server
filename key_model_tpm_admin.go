package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type keyModelTPMAdminAPI interface {
	CheckKeyModelTPMUpdateSupport(context.Context) error
	UpdateKeyModelTPM(context.Context, string, string, map[string]int64) error
}

type PreviewKeyModelTPMInput struct {
	TeamAlias string `json:"team_alias" jsonschema:"LiteLLM 团队别名，精确匹配"`
	KeyAlias  string `json:"key_alias" jsonschema:"LiteLLM key 别名，精确匹配且唯一"`
	ModelName string `json:"model_name" jsonschema:"LiteLLM 配置中的模型名，精确匹配"`
	Action    string `json:"action" jsonschema:"set 设置或更新限制；remove 只清除该模型在此 key 上的限制"`
	TPMLimit  *int64 `json:"tpm_limit,omitempty" jsonschema:"设置时必填的正整数 TPM 上限；清除时禁止提供"`
}

type ApplyKeyModelTPMInput struct {
	ChangeID string `json:"change_id" jsonschema:"预览返回的变更编号"`
	Confirm  bool   `json:"confirm" jsonschema:"管理员查看预览并明确确认后才设为 true"`
}

type keyModelTPMSnapshot struct {
	TeamID            string           `json:"team_id"`
	TeamAlias         string           `json:"team_alias"`
	KeyAlias          string           `json:"key_alias"`
	KeyModelTPMLimit  map[string]int64 `json:"key_model_tpm_limit"`
	TeamModelTPMLimit map[string]int64 `json:"team_model_tpm_limit"`
}

type keyModelTPMChange struct {
	ID        string               `json:"change_id"`
	Owner     string               `json:"owner"`
	Target    string               `json:"target"`
	Action    string               `json:"action"`
	ModelName string               `json:"model_name"`
	TPMLimit  *int64               `json:"tpm_limit,omitempty"`
	Before    keyModelTPMSnapshot  `json:"before"`
	After     map[string]int64     `json:"after_model_tpm_limit"`
	CreatedAt time.Time            `json:"created_at"`
	ExpiresAt time.Time            `json:"expires_at"`
	UpdatedAt time.Time            `json:"updated_at"`
	State     string               `json:"state"`
	Status    string               `json:"status"`
	Detail    string               `json:"detail"`
	Confirmed bool                 `json:"confirmed"`
	Observed  *keyModelTPMSnapshot `json:"observed,omitempty"`
}

type KeyModelTPMChangeOutput struct {
	ChangeID            string               `json:"change_id"`
	TeamID              string               `json:"team_id"`
	TeamAlias           string               `json:"team_alias"`
	KeyAlias            string               `json:"key_alias"`
	ModelName           string               `json:"model_name"`
	Action              string               `json:"action"`
	OldTPMLimit         *int64               `json:"old_tpm_limit"`
	NewTPMLimit         *int64               `json:"new_tpm_limit"`
	TeamTPMLimit        *int64               `json:"team_tpm_limit"`
	BeforeModelTPMLimit map[string]int64     `json:"before_model_tpm_limit"`
	AfterModelTPMLimit  map[string]int64     `json:"after_model_tpm_limit"`
	State               string               `json:"state"`
	Status              string               `json:"status"`
	Detail              string               `json:"detail"`
	ExpiresAt           time.Time            `json:"expires_at"`
	Confirmed           bool                 `json:"confirmed"`
	Warnings            []string             `json:"warnings"`
	Observed            *keyModelTPMSnapshot `json:"observed,omitempty"`
}

type keyModelTPMAdminService struct {
	query  *keyBudgetService
	api    keyModelTPMAdminAPI
	store  *changeStore
	owner  string
	target string
	now    func() time.Time
}

func validateKeyModelTPMInput(input PreviewKeyModelTPMInput) error {
	if strings.TrimSpace(input.TeamAlias) == "" || len(input.TeamAlias) > maxKeyBudgetFilterLength {
		return fmt.Errorf("team_alias 必须提供且不超过 %d 字节", maxKeyBudgetFilterLength)
	}
	if strings.TrimSpace(input.KeyAlias) == "" || len(input.KeyAlias) > maxKeyBudgetFilterLength {
		return fmt.Errorf("key_alias 必须提供且不超过 %d 字节", maxKeyBudgetFilterLength)
	}
	modelName := strings.TrimSpace(input.ModelName)
	if modelName == "" || len(modelName) > maxKeyBudgetFilterLength || strings.IndexFunc(modelName, unicode.IsControl) >= 0 {
		return fmt.Errorf("model_name 必须提供有效的 LiteLLM 模型名且不超过 %d 字节", maxKeyBudgetFilterLength)
	}
	switch input.Action {
	case "set":
		if input.TPMLimit == nil || *input.TPMLimit <= 0 {
			return fmt.Errorf("action=set 时 tpm_limit 必须为正整数")
		}
	case "remove":
		if input.TPMLimit != nil {
			return fmt.Errorf("action=remove 时不能提供 tpm_limit；移除模型条目才表示清除，不使用零值")
		}
	default:
		return fmt.Errorf("action 必须为 set 或 remove")
	}
	return nil
}

func changeModelTPMLimit(current map[string]int64, modelName, action string, limit *int64) (map[string]int64, error) {
	if strings.TrimSpace(modelName) == "" || modelName != strings.TrimSpace(modelName) {
		return nil, fmt.Errorf("model_name 必须是非空精确名称")
	}
	updated := cloneModelTPMLimit(current)
	switch action {
	case "set":
		if limit == nil || *limit <= 0 {
			return nil, fmt.Errorf("设置 TPM 时必须提供正整数")
		}
		updated[modelName] = *limit
	case "remove":
		if limit != nil {
			return nil, fmt.Errorf("清除模型限制时不能提供 tpm_limit")
		}
		delete(updated, modelName)
	default:
		return nil, fmt.Errorf("action 必须为 set 或 remove")
	}
	if len(updated) == 0 {
		return nil, nil
	}
	return updated, nil
}

func cloneModelTPMLimit(current map[string]int64) map[string]int64 {
	if len(current) == 0 {
		return map[string]int64{}
	}
	updated := make(map[string]int64, len(current)+1)
	for model, limit := range current {
		updated[model] = limit
	}
	return updated
}

func equalModelTPMLimit(left, right map[string]int64) bool {
	if len(left) != len(right) {
		return false
	}
	for model, limit := range left {
		if other, ok := right[model]; !ok || other != limit {
			return false
		}
	}
	return true
}

func (s *keyModelTPMAdminService) Preview(ctx context.Context, _ *mcp.CallToolRequest, input PreviewKeyModelTPMInput) (*mcp.CallToolResult, KeyModelTPMChangeOutput, error) {
	input.TeamAlias = strings.TrimSpace(input.TeamAlias)
	input.KeyAlias = strings.TrimSpace(input.KeyAlias)
	input.ModelName = strings.TrimSpace(input.ModelName)
	if err := validateKeyModelTPMInput(input); err != nil {
		return nil, KeyModelTPMChangeOutput{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.api.CheckKeyModelTPMUpdateSupport(ctx); err != nil {
		return nil, KeyModelTPMChangeOutput{}, err
	}
	before, err := s.current(ctx, input.TeamAlias, input.KeyAlias)
	if err != nil {
		return nil, KeyModelTPMChangeOutput{}, err
	}
	after, err := changeModelTPMLimit(before.KeyModelTPMLimit, input.ModelName, input.Action, input.TPMLimit)
	if err != nil {
		return nil, KeyModelTPMChangeOutput{}, err
	}
	now := s.now().UTC()
	change := &keyModelTPMChange{
		Owner: s.owner, Target: s.target, Action: input.Action, ModelName: input.ModelName,
		TPMLimit: input.TPMLimit, Before: before, After: after, CreatedAt: now,
		ExpiresAt: now.Add(changeLifetime), State: "preview", Status: "pending",
		Detail: "仅预览，尚未修改 LiteLLM",
	}
	change.ID, err = newChangeID()
	if err != nil {
		return nil, KeyModelTPMChangeOutput{}, err
	}
	if err := s.store.saveKeyTPM(change); err != nil {
		return nil, KeyModelTPMChangeOutput{}, fmt.Errorf("保存 TPM 预览失败，未修改 LiteLLM")
	}
	return nil, keyModelTPMChangeOutput(change), nil
}

func (s *keyModelTPMAdminService) Apply(ctx context.Context, _ *mcp.CallToolRequest, input ApplyKeyModelTPMInput) (*mcp.CallToolResult, KeyModelTPMChangeOutput, error) {
	if !input.Confirm {
		return nil, KeyModelTPMChangeOutput{}, fmt.Errorf("请先向管理员完整展示预览并取得明确确认，再设置 confirm=true")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	unlock, err := s.store.lock(ctx)
	if err != nil {
		return nil, KeyModelTPMChangeOutput{}, fmt.Errorf("获取执行锁失败")
	}
	defer unlock()
	change, err := s.store.loadKeyTPM(input.ChangeID)
	if err != nil {
		return nil, KeyModelTPMChangeOutput{}, err
	}
	if change.Owner != s.owner || change.Target != s.target {
		return nil, KeyModelTPMChangeOutput{}, fmt.Errorf("变更不属于当前管理凭证或 LiteLLM 部署")
	}
	if change.State == "done" {
		return nil, keyModelTPMChangeOutput(change), nil
	}
	if change.State == "executing" {
		return s.recover(ctx, change)
	}
	if change.State != "preview" || !s.now().Before(change.ExpiresAt) {
		return nil, KeyModelTPMChangeOutput{}, fmt.Errorf("预览已过期或状态无效，请重新预览")
	}
	if err := s.api.CheckKeyModelTPMUpdateSupport(ctx); err != nil {
		return nil, KeyModelTPMChangeOutput{}, err
	}
	current, err := s.current(ctx, change.Before.TeamAlias, change.Before.KeyAlias)
	if err != nil {
		return nil, KeyModelTPMChangeOutput{}, err
	}
	if !sameKeyModelTPMSnapshot(change.Before, current) {
		change.State, change.Status, change.Detail = "done", "conflict", "key 的 TPM 配置或团队 TPM 配置已变化；没有提交修改，请重新预览"
		if err := s.store.saveKeyTPM(change); err != nil {
			return nil, KeyModelTPMChangeOutput{}, fmt.Errorf("保存冲突结果失败")
		}
		return nil, keyModelTPMChangeOutput(change), nil
	}
	if equalModelTPMLimit(current.KeyModelTPMLimit, change.After) {
		change.State, change.Status, change.Detail, change.Confirmed = "done", "unchanged", "key 的 model_tpm_limit 已等于目标值，未提交修改", true
		if err := s.store.saveKeyTPM(change); err != nil {
			return nil, KeyModelTPMChangeOutput{}, fmt.Errorf("保存执行结果失败")
		}
		return nil, keyModelTPMChangeOutput(change), nil
	}
	change.State, change.Status, change.Detail = "executing", "inflight", "即将提交，只允许回读恢复"
	if err := s.store.saveKeyTPM(change); err != nil {
		return nil, KeyModelTPMChangeOutput{}, fmt.Errorf("提交前审计保存失败，尚未提交")
	}
	writeErr := s.api.UpdateKeyModelTPM(ctx, change.Before.TeamID, change.Before.KeyAlias, change.After)
	verifyCtx, verifyCancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer verifyCancel()
	return s.verify(verifyCtx, change, writeErr)
}

func (s *keyModelTPMAdminService) current(ctx context.Context, teamAlias, keyAlias string) (keyModelTPMSnapshot, error) {
	if strings.TrimSpace(teamAlias) == "" || strings.TrimSpace(keyAlias) == "" {
		return keyModelTPMSnapshot{}, fmt.Errorf("必须提供团队别名和 key 别名")
	}
	queryCtx, cancel := context.WithTimeout(ctx, keyBudgetQueryTimeout)
	defer cancel()
	rows, err := s.query.pool.Query(queryCtx, fmt.Sprintf(`
SELECT team_id, team_alias, key_alias,
       COALESCE(key_model_tpm_limit::text, '{}'),
       COALESCE(team_model_tpm_limit::text, '{}')
FROM %s
WHERE key_alias = $1
ORDER BY team_id NULLS LAST
LIMIT 2`, keyBudgetViewName), keyAlias)
	if err != nil {
		return keyModelTPMSnapshot{}, fmt.Errorf("读取 key TPM 配置失败")
	}
	defer rows.Close()
	var found *keyModelTPMSnapshot
	for rows.Next() {
		if found != nil {
			return keyModelTPMSnapshot{}, fmt.Errorf("key_alias 必须在所有团队中唯一，无法安全更新")
		}
		var teamID, team, alias, keyLimitsJSON, teamLimitsJSON pgtype.Text
		if err := rows.Scan(&teamID, &team, &alias, &keyLimitsJSON, &teamLimitsJSON); err != nil {
			return keyModelTPMSnapshot{}, fmt.Errorf("读取 key TPM 配置失败")
		}
		if !teamID.Valid || teamID.String == "" || !team.Valid || !alias.Valid ||
			team.String != teamAlias || alias.String != keyAlias {
			return keyModelTPMSnapshot{}, fmt.Errorf("key TPM 视图返回的团队或 key 标识与请求不一致")
		}
		keyLimits, err := decodeModelTPMLimitMap(keyLimitsJSON)
		if err != nil {
			return keyModelTPMSnapshot{}, fmt.Errorf("key model_tpm_limit 数据结构无效")
		}
		teamLimits, err := decodeModelTPMLimitMap(teamLimitsJSON)
		if err != nil {
			return keyModelTPMSnapshot{}, fmt.Errorf("team model_tpm_limit 数据结构无效")
		}
		found = &keyModelTPMSnapshot{
			TeamID: teamID.String, TeamAlias: team.String, KeyAlias: alias.String,
			KeyModelTPMLimit: keyLimits, TeamModelTPMLimit: teamLimits,
		}
	}
	if err := rows.Err(); err != nil {
		return keyModelTPMSnapshot{}, fmt.Errorf("读取 key TPM 配置失败")
	}
	if found == nil {
		return keyModelTPMSnapshot{}, fmt.Errorf("团队下不存在指定 key")
	}
	return *found, nil
}

func decodeModelTPMLimitMap(value pgtype.Text) (map[string]int64, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" || strings.TrimSpace(value.String) == "null" {
		return nil, nil
	}
	var limits map[string]int64
	if err := json.Unmarshal([]byte(value.String), &limits); err != nil {
		return nil, err
	}
	if len(limits) == 0 {
		return nil, nil
	}
	for model := range limits {
		if strings.TrimSpace(model) == "" {
			return nil, fmt.Errorf("model name 不能为空")
		}
	}
	return limits, nil
}

func sameKeyModelTPMSnapshot(before, after keyModelTPMSnapshot) bool {
	return before.TeamID == after.TeamID && before.TeamAlias == after.TeamAlias &&
		before.KeyAlias == after.KeyAlias && equalModelTPMLimit(before.KeyModelTPMLimit, after.KeyModelTPMLimit) &&
		equalModelTPMLimit(before.TeamModelTPMLimit, after.TeamModelTPMLimit)
}

func (s *keyModelTPMAdminService) recover(ctx context.Context, change *keyModelTPMChange) (*mcp.CallToolResult, KeyModelTPMChangeOutput, error) {
	current, err := s.current(ctx, change.Before.TeamAlias, change.Before.KeyAlias)
	if err == nil && keyModelTPMUpdateMatches(change, current) {
		change.Status, change.Detail, change.Confirmed = "applied", "回读确认 key 的 model_tpm_limit 已生效", true
		change.Observed = &current
	} else {
		change.Status, change.Detail = "unconfirmed", "上次执行中断，无法确认最终状态；禁止自动重试，请人工核查"
		if err == nil {
			change.Observed = &current
		} else {
			change.Detail += "：" + err.Error()
		}
	}
	change.State = "done"
	if err := s.store.saveKeyTPM(change); err != nil {
		return nil, KeyModelTPMChangeOutput{}, fmt.Errorf("保存恢复结果失败")
	}
	return nil, keyModelTPMChangeOutput(change), nil
}

func (s *keyModelTPMAdminService) verify(ctx context.Context, change *keyModelTPMChange, writeErr error) (*mcp.CallToolResult, KeyModelTPMChangeOutput, error) {
	current, err := s.current(ctx, change.Before.TeamAlias, change.Before.KeyAlias)
	if err == nil && keyModelTPMUpdateMatches(change, current) {
		change.Status, change.Detail, change.Confirmed = "applied", "回读确认 key 的 model_tpm_limit 已生效；团队 TPM 配置未改变", true
		if writeErr != nil {
			change.Detail += "；提交响应异常，但回读已确认"
		}
		change.Observed = &current
	} else {
		change.Status, change.Detail = "unconfirmed", "无法确认最终状态；禁止盲目重试，请人工核查"
		if err != nil {
			change.Detail += "：" + err.Error()
		} else {
			change.Observed = &current
		}
	}
	change.State = "done"
	if err := s.store.saveKeyTPM(change); err != nil {
		return nil, KeyModelTPMChangeOutput{}, fmt.Errorf("提交后结果保存失败，可能已生效；请使用同一 change_id 回读")
	}
	return nil, keyModelTPMChangeOutput(change), nil
}

func keyModelTPMUpdateMatches(change *keyModelTPMChange, current keyModelTPMSnapshot) bool {
	return current.TeamID == change.Before.TeamID && current.TeamAlias == change.Before.TeamAlias &&
		current.KeyAlias == change.Before.KeyAlias && equalModelTPMLimit(current.KeyModelTPMLimit, change.After) &&
		equalModelTPMLimit(current.TeamModelTPMLimit, change.Before.TeamModelTPMLimit)
}

func keyModelTPMChangeOutput(change *keyModelTPMChange) KeyModelTPMChangeOutput {
	oldLimit := optionalModelTPMLimit(change.Before.KeyModelTPMLimit, change.ModelName)
	newLimit := optionalModelTPMLimit(change.After, change.ModelName)
	warnings := make([]string, 0, 1)
	teamLimit := optionalModelTPMLimit(change.Before.TeamModelTPMLimit, change.ModelName)
	if change.Action == "remove" && teamLimit != nil {
		warnings = append(warnings, "已清除该模型的 key 级 TPM；团队级 TPM 限制仍独立生效")
	}
	if change.Action == "remove" && teamLimit == nil {
		warnings = append(warnings, "已清除该模型的 key 级 TPM；未读取到该模型的团队级 TPM 限制")
	}
	return KeyModelTPMChangeOutput{
		ChangeID: change.ID, TeamID: change.Before.TeamID, TeamAlias: change.Before.TeamAlias,
		KeyAlias: change.Before.KeyAlias, ModelName: change.ModelName, Action: change.Action,
		OldTPMLimit: oldLimit, NewTPMLimit: newLimit, TeamTPMLimit: teamLimit,
		BeforeModelTPMLimit: cloneModelTPMLimit(change.Before.KeyModelTPMLimit),
		AfterModelTPMLimit:  cloneModelTPMLimit(change.After), State: change.State,
		Status: change.Status, Detail: change.Detail, ExpiresAt: change.ExpiresAt,
		Confirmed: change.Confirmed, Warnings: warnings, Observed: change.Observed,
	}
}

func optionalModelTPMLimit(limits map[string]int64, model string) *int64 {
	limit, ok := limits[model]
	if !ok {
		return nil
	}
	return &limit
}

func validKeyModelTPMChange(change *keyModelTPMChange) bool {
	if change.ID == "" || change.Owner == "" || change.Target == "" || change.Before.TeamID == "" ||
		change.Before.TeamAlias == "" || change.Before.KeyAlias == "" || change.ModelName == "" ||
		!change.CreatedAt.Before(change.ExpiresAt) ||
		(change.State != "preview" && change.State != "executing" && change.State != "done") {
		return false
	}
	if change.Action != "set" && change.Action != "remove" {
		return false
	}
	if (change.Action == "set" && (change.TPMLimit == nil || *change.TPMLimit <= 0)) ||
		(change.Action == "remove" && change.TPMLimit != nil) {
		return false
	}
	expected, err := changeModelTPMLimit(change.Before.KeyModelTPMLimit, change.ModelName, change.Action, change.TPMLimit)
	if err != nil || !equalModelTPMLimit(expected, change.After) {
		return false
	}
	switch change.Status {
	case "pending", "inflight", "applied", "unchanged", "conflict", "unconfirmed":
	default:
		return false
	}
	if (change.State == "preview" && change.Status != "pending") ||
		(change.State == "executing" && change.Status != "inflight") ||
		(change.State == "done" && (change.Status == "pending" || change.Status == "inflight")) {
		return false
	}
	return true
}
