package main

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type keyBudgetAdminAPI interface {
	CheckKeyBudgetUpdateSupport(context.Context) error
	UpdateKeyBudget(context.Context, string, string, float64, string) error
}

const defaultKeyBudgetDuration = "1d"

var keyBudgetDurationPattern = regexp.MustCompile(`^[0-9]+(mo|[smhdw])$`)

type PreviewKeyBudgetInput struct {
	TeamAlias      string  `json:"team_alias" jsonschema:"LiteLLM 团队别名，精确匹配"`
	KeyAlias       string  `json:"key_alias" jsonschema:"LiteLLM key 别名，必须精确匹配唯一 key"`
	MaxBudget      float64 `json:"max_budget" jsonschema:"新的 key 预算上限，必须为正数且不超过管理员配置上限"`
	BudgetDuration string  `json:"budget_duration,omitempty" jsonschema:"预算周期，可选；省略时默认为 1d，例如 7d、30d、1mo、daily、weekly、monthly"`
}

type ApplyKeyBudgetInput struct {
	ChangeID string `json:"change_id" jsonschema:"预览返回的变更编号"`
	Confirm  bool   `json:"confirm" jsonschema:"管理员查看预览并明确确认后才设为 true"`
}

type keyBudgetChange struct {
	ID             string           `json:"change_id"`
	Owner          string           `json:"owner"`
	Target         string           `json:"target"`
	TeamAlias      string           `json:"team_alias"`
	KeyAlias       string           `json:"key_alias"`
	Amount         float64          `json:"max_budget"`
	BudgetDuration string           `json:"budget_duration"`
	Before         KeyBudgetRecord  `json:"before"`
	CreatedAt      time.Time        `json:"created_at"`
	ExpiresAt      time.Time        `json:"expires_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	State          string           `json:"state"`
	Status         string           `json:"status"`
	Detail         string           `json:"detail"`
	Confirmed      bool             `json:"budget_confirmed"`
	Observed       *KeyBudgetRecord `json:"observed,omitempty"`
}

type KeyBudgetChangeOutput struct {
	ChangeID              string           `json:"change_id"`
	TeamID                string           `json:"team_id"`
	TeamAlias             string           `json:"team_alias"`
	KeyAlias              string           `json:"key_alias"`
	State                 string           `json:"state"`
	Status                string           `json:"status"`
	Detail                string           `json:"detail"`
	ExpiresAt             time.Time        `json:"expires_at"`
	Spend                 *float64         `json:"spend"`
	OldKeyMaxBudget       *float64         `json:"old_key_max_budget"`
	NewKeyMaxBudget       float64          `json:"new_key_max_budget"`
	LinkedMaxBudget       *float64         `json:"linked_max_budget"`
	OldEffectiveBudget    *float64         `json:"old_effective_max_budget"`
	NewEffectiveBudget    *float64         `json:"new_effective_max_budget"`
	CurrentBudgetDuration *string          `json:"current_budget_duration"`
	NewBudgetDuration     string           `json:"new_budget_duration"`
	CurrentBudgetResetAt  *time.Time       `json:"current_budget_reset_at"`
	ObservedBudgetResetAt *time.Time       `json:"observed_budget_reset_at,omitempty"`
	BudgetConfirmed       bool             `json:"budget_confirmed"`
	Warnings              []string         `json:"warnings"`
	Observed              *KeyBudgetRecord `json:"observed,omitempty"`
}

type keyBudgetAdminService struct {
	query  *keyBudgetService
	api    keyBudgetAdminAPI
	store  *changeStore
	owner  string
	target string
	max    float64
	now    func() time.Time
}

func validateKeyBudgetPreviewInput(input PreviewKeyBudgetInput, max float64) error {
	if strings.TrimSpace(input.TeamAlias) == "" || len(input.TeamAlias) > maxKeyBudgetFilterLength {
		return fmt.Errorf("team_alias 必须提供且不超过 %d 字节", maxKeyBudgetFilterLength)
	}
	if strings.TrimSpace(input.KeyAlias) == "" || len(input.KeyAlias) > maxKeyBudgetFilterLength {
		return fmt.Errorf("key_alias 必须提供且不超过 %d 字节", maxKeyBudgetFilterLength)
	}
	if math.IsNaN(input.MaxBudget) || math.IsInf(input.MaxBudget, 0) || input.MaxBudget <= 0 || input.MaxBudget > max {
		return fmt.Errorf("max_budget 必须大于 0 且不超过管理员上限 %g；不支持零额度或清空额度", max)
	}
	if _, err := normalizeKeyBudgetDuration(input.BudgetDuration); err != nil {
		return err
	}
	return nil
}

func normalizeKeyBudgetDuration(value string) (string, error) {
	duration := strings.ToLower(strings.TrimSpace(value))
	if duration == "" {
		return defaultKeyBudgetDuration, nil
	}
	switch duration {
	case "hourly", "daily", "weekly", "monthly":
		return duration, nil
	}
	if !keyBudgetDurationPattern.MatchString(duration) {
		return "", fmt.Errorf("budget_duration 格式无效；请使用如 1d、7d、30d、1mo、daily、weekly 或 monthly 的周期")
	}
	unitLength := 1
	if strings.HasSuffix(duration, "mo") {
		unitLength = 2
	}
	amount, err := strconv.ParseUint(duration[:len(duration)-unitLength], 10, 64)
	if err != nil || amount == 0 {
		return "", fmt.Errorf("budget_duration 必须是正数周期")
	}
	return duration, nil
}

func effectiveKeyBudgetAfterChange(keyBudget float64, linkedBudget *float64) *float64 {
	if linkedBudget != nil && *linkedBudget < keyBudget {
		return numberPointer(*linkedBudget)
	}
	return numberPointer(keyBudget)
}

func (s *keyBudgetAdminService) Preview(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input PreviewKeyBudgetInput,
) (*mcp.CallToolResult, KeyBudgetChangeOutput, error) {
	input.TeamAlias = strings.TrimSpace(input.TeamAlias)
	input.KeyAlias = strings.TrimSpace(input.KeyAlias)
	duration, err := normalizeKeyBudgetDuration(input.BudgetDuration)
	if err != nil {
		return nil, KeyBudgetChangeOutput{}, err
	}
	input.BudgetDuration = duration
	if err := validateKeyBudgetPreviewInput(input, s.max); err != nil {
		return nil, KeyBudgetChangeOutput{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.api.CheckKeyBudgetUpdateSupport(ctx); err != nil {
		return nil, KeyBudgetChangeOutput{}, err
	}
	record, err := s.current(ctx, input.TeamAlias, input.KeyAlias)
	if err != nil {
		return nil, KeyBudgetChangeOutput{}, err
	}
	now := s.now().UTC()
	change := &keyBudgetChange{
		Owner: s.owner, Target: s.target, TeamAlias: input.TeamAlias, KeyAlias: input.KeyAlias,
		Amount: input.MaxBudget, BudgetDuration: input.BudgetDuration,
		Before: record, CreatedAt: now, ExpiresAt: now.Add(changeLifetime),
		State: "preview", Status: "pending", Detail: "仅预览，尚未修改 LiteLLM",
	}
	change.ID, err = newChangeID()
	if err != nil {
		return nil, KeyBudgetChangeOutput{}, err
	}
	if err := s.store.saveKey(change); err != nil {
		return nil, KeyBudgetChangeOutput{}, fmt.Errorf("保存 key 预算预览失败，未修改 LiteLLM")
	}
	return nil, keyBudgetChangeOutput(change), nil
}

func (s *keyBudgetAdminService) Apply(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input ApplyKeyBudgetInput,
) (*mcp.CallToolResult, KeyBudgetChangeOutput, error) {
	if !input.Confirm {
		return nil, KeyBudgetChangeOutput{}, fmt.Errorf("请先向管理员展示预览并取得明确确认，再设置 confirm=true")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	unlock, err := s.store.lock(ctx)
	if err != nil {
		return nil, KeyBudgetChangeOutput{}, fmt.Errorf("获取执行锁失败: %w", err)
	}
	defer unlock()
	change, err := s.store.loadKey(input.ChangeID)
	if err != nil {
		return nil, KeyBudgetChangeOutput{}, err
	}
	if change.Owner != s.owner || change.Target != s.target {
		return nil, KeyBudgetChangeOutput{}, fmt.Errorf("变更不属于当前管理凭证或 LiteLLM 部署")
	}
	if change.State == "done" {
		return nil, keyBudgetChangeOutput(change), nil
	}
	if change.State == "executing" {
		return s.recover(ctx, change)
	}
	if change.State != "preview" || !s.now().Before(change.ExpiresAt) {
		return nil, KeyBudgetChangeOutput{}, fmt.Errorf("预览已过期或状态无效，请重新预览")
	}
	if err := validateKeyBudgetPreviewInput(PreviewKeyBudgetInput{
		TeamAlias: change.TeamAlias, KeyAlias: change.KeyAlias, MaxBudget: change.Amount,
		BudgetDuration: change.BudgetDuration,
	}, s.max); err != nil {
		return nil, KeyBudgetChangeOutput{}, fmt.Errorf("预览额度超出当前配置上限，请重新预览")
	}
	if err := s.api.CheckKeyBudgetUpdateSupport(ctx); err != nil {
		return nil, KeyBudgetChangeOutput{}, err
	}
	current, err := s.current(ctx, change.TeamAlias, change.KeyAlias)
	if err != nil {
		return nil, KeyBudgetChangeOutput{}, err
	}
	if !sameKeyBudgetSnapshot(change.Before, current) {
		change.State, change.Status, change.Detail = "done", "conflict", "key 或预算配置已变化；没有提交修改，请重新预览"
		if err := s.store.saveKey(change); err != nil {
			return nil, KeyBudgetChangeOutput{}, fmt.Errorf("保存冲突结果失败")
		}
		return nil, keyBudgetChangeOutput(change), nil
	}
	if sameOptionalFloat(change.Before.KeyMaxBudget, numberPointer(change.Amount)) &&
		stringPointerValue(change.Before.BudgetDuration) == change.BudgetDuration && change.Before.BudgetResetAt != nil {
		change.State, change.Status, change.Detail, change.Confirmed = "done", "unchanged", "key 额度和预算周期已经等于目标值，未提交修改", true
		if err := s.store.saveKey(change); err != nil {
			return nil, KeyBudgetChangeOutput{}, fmt.Errorf("保存结果失败")
		}
		return nil, keyBudgetChangeOutput(change), nil
	}
	change.State, change.Status, change.Detail = "executing", "inflight", "即将提交，只允许回读恢复"
	if err := s.store.saveKey(change); err != nil {
		return nil, KeyBudgetChangeOutput{}, fmt.Errorf("提交前审计保存失败，尚未提交")
	}
	writeErr := s.api.UpdateKeyBudget(ctx, change.Before.TeamIDValue(), change.KeyAlias, change.Amount, change.BudgetDuration)
	verifyCtx, verifyCancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer verifyCancel()
	return s.verify(verifyCtx, change, writeErr)
}

func (s *keyBudgetAdminService) current(ctx context.Context, teamAlias, keyAlias string) (KeyBudgetRecord, error) {
	_, result, err := s.query.QueryTeamKeyBudget(ctx, nil, KeyBudgetQueryInput{
		TeamAlias: teamAlias, KeyAlias: keyAlias, PageSize: 2,
	})
	if err != nil {
		return KeyBudgetRecord{}, err
	}
	if result.HasMore || len(result.Records) != 1 {
		return KeyBudgetRecord{}, fmt.Errorf("团队与 key 别名必须精确对应一个 key；别名重复或目标不存在")
	}
	record := result.Records[0]
	if record.TeamID == nil || *record.TeamID == "" || record.TeamAlias == nil || *record.TeamAlias != teamAlias ||
		record.KeyAlias == nil || *record.KeyAlias != keyAlias {
		return KeyBudgetRecord{}, fmt.Errorf("预算视图返回的团队或 key 标识与请求不一致")
	}
	aliases, err := s.query.QueryKeyAlias(ctx, keyAlias)
	if err != nil {
		return KeyBudgetRecord{}, err
	}
	if len(aliases) != 1 || aliases[0].TeamIDValue() != record.TeamIDValue() {
		return KeyBudgetRecord{}, fmt.Errorf("key_alias 必须在团队 key 预算视图中全局唯一；LiteLLM 更新接口无法安全区分重复别名")
	}
	return record, nil
}

func sameKeyBudgetSnapshot(before, after KeyBudgetRecord) bool {
	return sameKeyBudgetSettings(before, after) && sameOptionalFloat(before.KeyMaxBudget, after.KeyMaxBudget) &&
		reflect.DeepEqual(before.BudgetDuration, after.BudgetDuration) && reflect.DeepEqual(before.BudgetResetAt, after.BudgetResetAt)
}

func sameKeyBudgetSettings(before, after KeyBudgetRecord) bool {
	return before.TeamIDValue() == after.TeamIDValue() && reflect.DeepEqual(before.TeamAlias, after.TeamAlias) &&
		reflect.DeepEqual(before.KeyAlias, after.KeyAlias) && reflect.DeepEqual(before.KeyName, after.KeyName) &&
		reflect.DeepEqual(before.UserID, after.UserID) &&
		sameOptionalFloat(before.BudgetTableMaxBudget, after.BudgetTableMaxBudget) &&
		reflect.DeepEqual(before.Expires, after.Expires) && reflect.DeepEqual(before.Blocked, after.Blocked) &&
		reflect.DeepEqual(before.CreatedAt, after.CreatedAt)
}

func (record KeyBudgetRecord) TeamIDValue() string {
	if record.TeamID == nil {
		return ""
	}
	return *record.TeamID
}

func stringPointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func sameOptionalFloat(left, right *float64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func spendDidNotDecrease(before, after *float64) bool {
	return before == nil || after != nil && *after >= *before
}

func (s *keyBudgetAdminService) recover(ctx context.Context, change *keyBudgetChange) (*mcp.CallToolResult, KeyBudgetChangeOutput, error) {
	current, err := s.current(ctx, change.TeamAlias, change.KeyAlias)
	if err == nil && keyBudgetUpdateMatches(change, current) && spendDidNotDecrease(change.Before.Spend, current.Spend) {
		change.Status, change.Detail, change.Confirmed = "applied", "回读确认预算额度已生效", true
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
	if err := s.store.saveKey(change); err != nil {
		return nil, KeyBudgetChangeOutput{}, fmt.Errorf("保存恢复结果失败")
	}
	return nil, keyBudgetChangeOutput(change), nil
}

func (s *keyBudgetAdminService) verify(ctx context.Context, change *keyBudgetChange, writeErr error) (*mcp.CallToolResult, KeyBudgetChangeOutput, error) {
	current, err := s.current(ctx, change.TeamAlias, change.KeyAlias)
	if err == nil && keyBudgetUpdateMatches(change, current) && spendDidNotDecrease(change.Before.Spend, current.Spend) {
		change.Status, change.Detail, change.Confirmed = "applied", "回读确认 key 自身额度已生效；关联预算与周期保持不变", true
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
	if err := s.store.saveKey(change); err != nil {
		return nil, KeyBudgetChangeOutput{}, fmt.Errorf("提交后结果保存失败，可能已生效；请使用同一 change_id 回读")
	}
	return nil, keyBudgetChangeOutput(change), nil
}

func keyBudgetUpdateMatches(change *keyBudgetChange, current KeyBudgetRecord) bool {
	return sameKeyBudgetSettings(change.Before, current) &&
		sameOptionalFloat(current.KeyMaxBudget, numberPointer(change.Amount)) &&
		stringPointerValue(current.BudgetDuration) == change.BudgetDuration && current.BudgetResetAt != nil
}

func keyBudgetChangeOutput(change *keyBudgetChange) KeyBudgetChangeOutput {
	warnings := make([]string, 0, 2)
	var observedBudgetResetAt *time.Time
	if change.Observed != nil {
		observedBudgetResetAt = change.Observed.BudgetResetAt
	}
	if change.Before.Spend != nil && change.Amount <= *change.Before.Spend {
		warnings = append(warnings, "新预算不高于 key 当前周期消费，key 可能已经达到或超过新上限")
	}
	if change.Before.BudgetTableMaxBudget != nil && *change.Before.BudgetTableMaxBudget <= change.Amount {
		warnings = append(warnings, "关联预算不高于新 key 额度，最终生效额度仍受关联预算限制")
	}
	if change.Before.BudgetDuration != nil && *change.Before.BudgetDuration != change.BudgetDuration {
		warnings = append(warnings, "预算周期将被修改；当前消费会保留，不会因修改周期而清零")
	}
	return KeyBudgetChangeOutput{
		ChangeID: change.ID, TeamID: change.Before.TeamIDValue(), TeamAlias: change.TeamAlias,
		KeyAlias: change.KeyAlias, State: change.State, Status: change.Status, Detail: change.Detail,
		ExpiresAt: change.ExpiresAt, Spend: change.Before.Spend, OldKeyMaxBudget: change.Before.KeyMaxBudget,
		NewKeyMaxBudget: change.Amount, LinkedMaxBudget: change.Before.BudgetTableMaxBudget,
		OldEffectiveBudget:    change.Before.EffectiveMaxBudget,
		NewEffectiveBudget:    effectiveKeyBudgetAfterChange(change.Amount, change.Before.BudgetTableMaxBudget),
		CurrentBudgetDuration: change.Before.BudgetDuration, NewBudgetDuration: change.BudgetDuration,
		CurrentBudgetResetAt: change.Before.BudgetResetAt, ObservedBudgetResetAt: observedBudgetResetAt,
		BudgetConfirmed: change.Confirmed, Warnings: warnings, Observed: change.Observed,
	}
}

func validKeyBudgetChange(change *keyBudgetChange) bool {
	duration, durationErr := normalizeKeyBudgetDuration(change.BudgetDuration)
	if change.Owner == "" || change.Target == "" || change.TeamAlias == "" || change.KeyAlias == "" ||
		change.Before.TeamIDValue() == "" || change.Before.TeamAlias == nil || *change.Before.TeamAlias != change.TeamAlias ||
		change.Before.KeyAlias == nil || *change.Before.KeyAlias != change.KeyAlias ||
		change.Amount <= 0 || math.IsNaN(change.Amount) || math.IsInf(change.Amount, 0) ||
		change.CreatedAt.IsZero() || !change.ExpiresAt.After(change.CreatedAt) ||
		durationErr != nil || duration != change.BudgetDuration {
		return false
	}
	switch change.State {
	case "preview":
		return change.Status == "pending" && change.Observed == nil && !change.Confirmed
	case "executing":
		return change.Status == "inflight" && change.Observed == nil && !change.Confirmed
	case "done":
		switch change.Status {
		case "applied":
			return change.Confirmed && change.Observed != nil
		case "unchanged":
			return change.Confirmed && change.Observed == nil
		case "conflict", "unconfirmed":
			return !change.Confirmed
		default:
			return false
		}
	default:
		return false
	}
}
