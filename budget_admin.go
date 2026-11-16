package main

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxChangeMembers = 100

type PreviewBudgetInput struct {
	TeamAlias              string  `json:"team_alias" jsonschema:"团队别名，必须精确匹配唯一团队"`
	Scope                  string  `json:"scope" jsonschema:"member 表示单成员，team_members 表示团队现有普通成员"`
	UserEmail              string  `json:"user_email,omitempty" jsonschema:"单成员模式必填，完整邮箱或完整邮箱前缀；多个用户匹配时拒绝执行"`
	MaxBudget              float64 `json:"max_budget" jsonschema:"每位目标成员的新预算上限，必须大于零且不超过管理员配置的上限"`
	ResetCurrentCycleSpend bool    `json:"reset_current_cycle_spend,omitempty" jsonschema:"是否同时清零成员当前周期已消费金额，默认 false；true 时额外调用定向清零接口，不清空累计消费，也不修改周期和重置时间"`
}

type ApplyBudgetInput struct {
	ChangeID string `json:"change_id" jsonschema:"预览返回的变更编号，不可更改预览中的目标和金额"`
	Confirm  bool   `json:"confirm" jsonschema:"必须先向管理员完整展示预览（包括是否清零当前周期消费）并获得明确确认，才能传 true"`
}

type BudgetChangeMember struct {
	UserID               string   `json:"user_id"`
	UserEmail            string   `json:"user_email"`
	OldMaxBudget         *float64 `json:"old_max_budget"`
	NewMaxBudget         float64  `json:"new_max_budget"`
	CurrentSpend         *float64 `json:"current_cycle_spend"`
	BudgetDuration       *string  `json:"budget_duration"`
	BudgetResetAt        *string  `json:"budget_reset_at"`
	Status               string   `json:"status"`
	Detail               string   `json:"detail"`
	BudgetConfirmed      bool     `json:"budget_confirmed"`
	ResetStatus          string   `json:"reset_status"`
	TargetCurrentSpend   *float64 `json:"target_current_cycle_spend"`
	ObservedCurrentSpend *float64 `json:"observed_current_cycle_spend"`
}

type BudgetChangeOutput struct {
	ChangeID               string               `json:"change_id"`
	TeamID                 string               `json:"team_id"`
	TeamAlias              string               `json:"team_alias"`
	Scope                  string               `json:"scope"`
	State                  string               `json:"state"`
	ExpiresAt              time.Time            `json:"expires_at"`
	Members                []BudgetChangeMember `json:"members"`
	Count                  int                  `json:"count"`
	Warnings               []string             `json:"warnings"`
	ResetCurrentCycleSpend bool                 `json:"reset_current_cycle_spend"`
}

type budgetAdminService struct {
	query      *budgetService
	api        budgetAdminAPI
	safety     budgetSafety
	store      *changeStore
	keyBudgets *keyBudgetAdminService
	owner      string
	target     string
	max        float64
	now        func() time.Time
}

func (s *budgetAdminService) Preview(ctx context.Context, _ *mcp.CallToolRequest, input PreviewBudgetInput) (*mcp.CallToolResult, BudgetChangeOutput, error) {
	if err := validatePreview(input, s.max); err != nil {
		return nil, BudgetChangeOutput{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if input.ResetCurrentCycleSpend {
		if err := s.api.CheckResetSupport(ctx); err != nil {
			return nil, BudgetChangeOutput{}, err
		}
	}
	input.TeamAlias = strings.TrimSpace(input.TeamAlias)
	// 先查整个别名，不能让邮箱筛选掩盖同名团队的歧义。
	_, records, err := s.query.QueryTeamMemberBudget(ctx, nil, BudgetQueryInput{TeamAlias: input.TeamAlias})
	if err != nil {
		return nil, BudgetChangeOutput{}, err
	}
	teamID, users, err := resolveTeam(records.Records, input)
	if err != nil {
		return nil, BudgetChangeOutput{}, err
	}
	team, err := s.api.Team(ctx, teamID)
	if err != nil {
		return nil, BudgetChangeOutput{}, err
	}
	if team.Team.Alias != input.TeamAlias {
		return nil, BudgetChangeOutput{}, fmt.Errorf("数据库与 LiteLLM API 的团队别名不一致")
	}
	var selected []string
	for _, member := range team.Team.Members {
		if member.Role != "user" || member.UserID == "default_user_id" {
			continue
		}
		if input.Scope == "member" && !users[member.UserID] {
			continue
		}
		if !users[member.UserID] {
			return nil, BudgetChangeOutput{}, fmt.Errorf("团队成员列表与预算视图不一致，拒绝生成不完整批量计划")
		}
		selected = append(selected, member.UserID)
	}
	if len(selected) == 0 || len(selected) > maxChangeMembers {
		return nil, BudgetChangeOutput{}, fmt.Errorf("目标普通成员数量必须在 1 到 %d 之间", maxChangeMembers)
	}
	sort.Strings(selected)
	change := &budgetChange{
		Owner: s.owner, Target: s.target, TeamAlias: input.TeamAlias, Scope: input.Scope,
		Amount: input.MaxBudget, CreatedAt: s.now().UTC(), State: "preview",
		ResetCurrentCycleSpend: input.ResetCurrentCycleSpend,
	}
	change.ExpiresAt = change.CreatedAt.Add(changeLifetime)
	for _, id := range selected {
		snapshot, err := snapshotMember(team, id)
		if err != nil {
			return nil, BudgetChangeOutput{}, err
		}
		if input.Scope == "member" {
			prefix, _, _ := strings.Cut(strings.TrimSpace(input.UserEmail), "@")
			actual, _, _ := strings.Cut(snapshot.Email, "@")
			if snapshot.Email != "" && prefix != actual {
				return nil, BudgetChangeOutput{}, fmt.Errorf("数据库与 API 的成员邮箱前缀不一致，拒绝选择目标")
			}
		}
		if snapshot.Email == "" {
			for _, record := range records.Records {
				if record.UserID != nil && *record.UserID == id && record.UserEmail != nil {
					snapshot.Email = *record.UserEmail
					break
				}
			}
		}
		if err := s.safety.Check(ctx, snapshot); err != nil {
			return nil, BudgetChangeOutput{}, fmt.Errorf("成员 %s: %w", id, err)
		}
		item := changeItem{Before: snapshot, Status: "pending"}
		if input.ResetCurrentCycleSpend {
			item.ResetStatus = "pending"
		}
		change.Items = append(change.Items, item)
	}
	change.ID, err = newChangeID()
	if err != nil {
		return nil, BudgetChangeOutput{}, err
	}
	if err := s.store.save(change); err != nil {
		return nil, BudgetChangeOutput{}, fmt.Errorf("保存预览记录失败，未修改预算: %w", err)
	}
	return nil, changeOutput(change), nil
}

func validatePreview(input PreviewBudgetInput, max float64) error {
	if strings.TrimSpace(input.TeamAlias) == "" {
		return fmt.Errorf("必须提供 team_alias")
	}
	if _, _, err := buildBudgetQuery(BudgetQueryInput{TeamAlias: input.TeamAlias, UserEmail: input.UserEmail}); err != nil {
		return err
	}
	if input.Scope != "member" && input.Scope != "team_members" {
		return fmt.Errorf("scope 必须为 member 或 team_members")
	}
	if input.Scope == "member" && strings.TrimSpace(input.UserEmail) == "" {
		return fmt.Errorf("单成员模式必须提供 user_email")
	}
	if input.Scope == "team_members" && input.UserEmail != "" {
		return fmt.Errorf("团队批量模式不能同时提供 user_email")
	}
	if math.IsNaN(input.MaxBudget) || math.IsInf(input.MaxBudget, 0) || input.MaxBudget <= 0 || input.MaxBudget > max {
		return fmt.Errorf("max_budget 必须大于 0 且不超过管理员上限 %g；不支持清空或无限额", max)
	}
	return nil
}

func resolveTeam(records []BudgetRecord, input PreviewBudgetInput) (string, map[string]bool, error) {
	teamID := ""
	users := map[string]bool{}
	prefix, _, _ := strings.Cut(strings.TrimSpace(input.UserEmail), "@")
	for _, record := range records {
		if record.TeamID == nil || *record.TeamID == "" || (teamID != "" && teamID != *record.TeamID) {
			return "", nil, fmt.Errorf("team_alias 未对应唯一有效团队，请先修正团队别名")
		}
		teamID = *record.TeamID
		if record.UserID == nil || *record.UserID == "" {
			continue
		}
		if input.Scope == "member" {
			if record.UserEmail == nil {
				continue
			}
			local, _, _ := strings.Cut(*record.UserEmail, "@")
			if local != prefix {
				continue
			}
		}
		users[*record.UserID] = true
	}
	if teamID == "" || len(users) == 0 {
		return "", nil, fmt.Errorf("没有找到目标团队或成员")
	}
	if input.Scope == "member" && len(users) != 1 {
		return "", nil, fmt.Errorf("邮箱前缀匹配多个用户，拒绝自动选择；请先消除身份歧义")
	}
	return teamID, users, nil
}

func snapshotMember(team teamInfo, userID string) (memberSnapshot, error) {
	var member teamMember
	count := 0
	for _, m := range team.Team.Members {
		if m.UserID == userID {
			member = m
			count++
		}
	}
	if count != 1 || member.Role != "user" || userID == "" || userID == "default_user_id" {
		return memberSnapshot{}, fmt.Errorf("目标不是唯一的现有普通成员")
	}
	var membership teamMembership
	count = 0
	for _, m := range team.Memberships {
		if m.UserID == userID {
			membership = m
			count++
		}
	}
	if count != 1 || membership.TeamID != team.Team.ID || membership.BudgetID == "" || membership.Budget == nil {
		return memberSnapshot{}, fmt.Errorf("成员 %s 缺少独立预算或成员关系异常，不自动创建预算", userID)
	}
	if team.Team.Metadata["team_member_budget_id"] == membership.BudgetID {
		return memberSnapshot{}, fmt.Errorf("成员 %s 使用团队默认共享预算，拒绝修改", userID)
	}
	if membership.Budget["budget_id"] != membership.BudgetID {
		return memberSnapshot{}, fmt.Errorf("成员预算标识不一致")
	}
	for _, key := range []string{"max_budget", "budget_duration", "budget_reset_at"} {
		if _, ok := membership.Budget[key]; !ok {
			return memberSnapshot{}, fmt.Errorf("预算配置缺少字段 %s，无法保证保留原配置", key)
		}
	}
	if value := membership.Budget["max_budget"]; value != nil {
		if number, ok := value.(float64); !ok || math.IsInf(number, 0) || math.IsNaN(number) {
			return memberSnapshot{}, fmt.Errorf("原额度格式无效")
		}
	}
	for _, key := range []string{"budget_duration", "budget_reset_at"} {
		if value := membership.Budget[key]; value != nil {
			if _, ok := value.(string); !ok {
				return memberSnapshot{}, fmt.Errorf("原周期配置格式无效")
			}
		}
	}
	for _, value := range []*float64{membership.Spend, membership.TotalSpend} {
		if value == nil || !validSpend(*value) {
			return memberSnapshot{}, fmt.Errorf("缺少有效的成员消费计数，无法核验")
		}
	}
	return memberSnapshot{
		TeamID: team.Team.ID, UserID: userID, Email: member.UserEmail,
		BudgetID: membership.BudgetID, Budget: membership.Budget,
		Spend: membership.Spend, TotalSpend: membership.TotalSpend,
	}, nil
}

func budgetSettings(budget map[string]any) map[string]any {
	result := make(map[string]any, len(budget))
	for key, value := range budget {
		switch key {
		case "created_at", "updated_at", "created_by", "updated_by":
			continue
		}
		if key == "budget_reset_at" && value != nil {
			if text, ok := value.(string); ok {
				// Prisma 的无时区 timestamp 按 UTC 存储，API 可能补上 Z。
				for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999"} {
					if timestamp, err := time.Parse(layout, text); err == nil {
						value = timestamp.UTC().Format(time.RFC3339Nano)
						break
					}
				}
			}
		}
		result[key] = value
	}
	return result
}

func sameConfiguration(before, after memberSnapshot) bool {
	return before.TeamID == after.TeamID && before.UserID == after.UserID &&
		before.BudgetID == after.BudgetID &&
		reflect.DeepEqual(budgetSettings(before.Budget), budgetSettings(after.Budget))
}

func (s *budgetAdminService) current(ctx context.Context, before memberSnapshot, alias string) (memberSnapshot, error) {
	team, err := s.api.Team(ctx, before.TeamID)
	if err != nil {
		return memberSnapshot{}, err
	}
	if team.Team.Alias != alias {
		return memberSnapshot{}, fmt.Errorf("团队别名已变化，请重新预览")
	}
	now, err := snapshotMember(team, before.UserID)
	if err != nil {
		return memberSnapshot{}, err
	}
	if err := s.safety.Check(ctx, now); err != nil {
		return memberSnapshot{}, err
	}
	return now, nil
}

func (s *budgetAdminService) Apply(ctx context.Context, _ *mcp.CallToolRequest, input ApplyBudgetInput) (*mcp.CallToolResult, BudgetChangeOutput, error) {
	if !input.Confirm {
		return nil, BudgetChangeOutput{}, fmt.Errorf("请先向管理员展示预览并取得明确确认，再设置 confirm=true")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	unlock, err := s.store.lock(ctx)
	if err != nil {
		return nil, BudgetChangeOutput{}, fmt.Errorf("获取执行锁失败: %w", err)
	}
	defer unlock()
	change, err := s.store.load(input.ChangeID)
	if err != nil {
		return nil, BudgetChangeOutput{}, err
	}
	if change.Owner != s.owner || change.Target != s.target {
		return nil, BudgetChangeOutput{}, fmt.Errorf("变更不属于当前管理凭证或 LiteLLM 部署")
	}
	if change.State == "done" {
		return nil, changeOutput(change), nil
	}
	if change.State == "executing" {
		// 崩溃或落盘失败后的记录只能回读，不重发可能已成功的请求，也不继续未开始的成员。
		for i := range change.Items {
			item := &change.Items[i]
			switch item.Status {
			case "inflight":
				if change.ResetCurrentCycleSpend && item.ResetStatus == "inflight" {
					s.verifyReset(ctx, change, item)
				} else {
					s.verifyBudget(ctx, change, item, nil)
					if change.ResetCurrentCycleSpend {
						item.ResetStatus = "skipped"
						if item.BudgetConfirmed {
							item.Status, item.Detail = "partial", "额度已确认，但上次执行中断，未提交清零；需人工核查后重新预览"
						}
					}
				}
			case "pending":
				item.Status, item.Detail = "skipped", "上次执行中断，未提交；需重新预览"
				if change.ResetCurrentCycleSpend {
					item.ResetStatus = "skipped"
				}
			}
		}
		return s.finish(change)
	}
	if change.State != "preview" {
		return nil, BudgetChangeOutput{}, fmt.Errorf("变更状态无效")
	}
	if !s.now().Before(change.ExpiresAt) {
		return nil, BudgetChangeOutput{}, fmt.Errorf("预览已过期，请重新预览")
	}
	if change.Amount <= 0 || change.Amount > s.max {
		return nil, BudgetChangeOutput{}, fmt.Errorf("预览额度超出当前管理员上限，请重新预览")
	}
	if change.ResetCurrentCycleSpend {
		// 执行前再次检查，防止预览后上游回退到不支持清零的版本。
		if err := s.api.CheckResetSupport(ctx); err != nil {
			return nil, BudgetChangeOutput{}, err
		}
	}
	started := s.now().UTC()
	change.State, change.StartedAt = "executing", &started
	if err := s.store.save(change); err != nil {
		return nil, BudgetChangeOutput{}, fmt.Errorf("执行状态保存失败，尚未提交: %w", err)
	}
	stop := false
	for i := range change.Items {
		item := &change.Items[i]
		if stop || ctx.Err() != nil {
			item.Status, item.Detail = "skipped", "前项未确认或调用已取消，未提交；需重新预览"
			if change.ResetCurrentCycleSpend {
				item.ResetStatus = "skipped"
			}
			continue
		}
		current, err := s.current(ctx, item.Before, change.TeamAlias)
		if err != nil || !sameConfiguration(item.Before, current) {
			item.Status, item.Detail = "conflict", "角色、关联或预算配置已变化或无法核验，请重新预览"
			if err != nil {
				item.Detail = err.Error()
			}
			if change.ResetCurrentCycleSpend {
				item.ResetStatus = "skipped"
			}
			stop = true
			continue
		}
		old := numberPointer(current.Budget["max_budget"])
		budgetUnchanged := old != nil && *old == change.Amount
		if budgetUnchanged && !change.ResetCurrentCycleSpend {
			item.Status, item.Detail = "unchanged", "原额度已等于目标额度，未发送更新"
			item.BudgetConfirmed = true
			item.ObservedSpend = current.Spend
			continue
		}
		// 记录提交前最新计数；正常消费增长不构成配置冲突。
		item.Before.Spend, item.Before.TotalSpend = current.Spend, current.TotalSpend
		item.Status, item.Detail = "inflight", "即将提交，仅允许回读恢复"
		if err := s.store.save(change); err != nil {
			return nil, BudgetChangeOutput{}, fmt.Errorf("提交前审计保存失败，停止后续修改: %w", err)
		}
		var writeErr error
		if !budgetUnchanged {
			writeErr = s.api.Update(ctx, current.TeamID, current.UserID, change.Amount)
		}
		// 即使调用方取消或 POST 超时，也单独回读一次；不能把网络错误当作未写入。
		verifyCtx, verifyCancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
		s.verifyBudget(verifyCtx, change, item, writeErr)
		verifyCancel()
		if change.ResetCurrentCycleSpend {
			if item.Status == "applied" {
				if err := s.resetCurrentCycleSpend(ctx, change, item); err != nil {
					return nil, BudgetChangeOutput{}, err
				}
			} else {
				item.ResetStatus = "skipped"
				item.Detail += "；未提交清零"
			}
		}
		if err := s.store.save(change); err != nil {
			return nil, BudgetChangeOutput{}, fmt.Errorf("提交后结果保存失败，可能已生效；请使用同一 change_id 回读恢复")
		}
		stop = item.Status == "unconfirmed" || item.Status == "partial"
	}
	return s.finish(change)
}

func (s *budgetAdminService) verifyBudget(ctx context.Context, change *budgetChange, item *changeItem, writeErr error) {
	current, err := s.current(ctx, item.Before, change.TeamAlias)
	item.Status, item.Detail = "unconfirmed", "无法确认最终状态；禁止盲目重试，请管理员核查"
	if err != nil {
		item.Detail += "：" + err.Error()
		return
	}
	item.ObservedSpend = current.Spend
	expected := expectedBudgetSnapshot(change, item)
	if sameConfiguration(expected, current) &&
		*current.Spend >= *item.Before.Spend && *current.TotalSpend >= *item.Before.TotalSpend {
		item.Status, item.Detail = "applied", "回读确认额度生效，其他预算配置未变化，消费计数未下降"
		item.BudgetConfirmed = true
		if writeErr != nil {
			item.Detail += "；提交响应异常但回读已确认"
		}
		return
	}
	if sameConfiguration(item.Before, current) && writeErr != nil {
		// 请求可能仍在上游处理，原值不等于明确失败；不自动重试。
		item.Detail = "提交返回异常且回读仍为原值，结果未确认；请管理员稍后核查"
	}
}

func (s *budgetAdminService) finish(change *budgetChange) (*mcp.CallToolResult, BudgetChangeOutput, error) {
	change.State = "done"
	if err := s.store.save(change); err != nil {
		return nil, BudgetChangeOutput{}, fmt.Errorf("保存最终审计结果失败，请使用同一 change_id 恢复")
	}
	return nil, changeOutput(change), nil
}

func changeOutput(change *budgetChange) BudgetChangeOutput {
	output := BudgetChangeOutput{
		ChangeID: change.ID, TeamID: change.Items[0].Before.TeamID, TeamAlias: change.TeamAlias,
		Scope: change.Scope, State: change.State, ExpiresAt: change.ExpiresAt,
		Members: make([]BudgetChangeMember, 0, len(change.Items)), Count: len(change.Items),
		Warnings:               []string{"仅修改现有普通成员的独立预算；不调整团队总预算或未来成员默认值。", "批量更新不是事务，可能部分成功；不会自动回滚或重试。"},
		ResetCurrentCycleSpend: change.ResetCurrentCycleSpend,
	}
	if change.ResetCurrentCycleSpend {
		output.Warnings = append(output.Warnings, "本次将清零目标成员的当前周期已消费金额；不清空累计消费，不重设周期或下次重置时间。额度更新和清零不是原子操作，可能只完成额度更新。")
	}
	for _, item := range change.Items {
		resetStatus := "not_requested"
		var targetSpend *float64
		if change.ResetCurrentCycleSpend {
			resetStatus = item.ResetStatus
			targetSpend = numberPointer(float64(0))
		}
		output.Members = append(output.Members, BudgetChangeMember{
			UserID: item.Before.UserID, UserEmail: item.Before.Email,
			OldMaxBudget: numberPointer(item.Before.Budget["max_budget"]), NewMaxBudget: change.Amount,
			CurrentSpend: item.Before.Spend, BudgetDuration: stringPointer(item.Before.Budget["budget_duration"]),
			BudgetResetAt: stringPointer(item.Before.Budget["budget_reset_at"]), Status: item.Status, Detail: item.Detail,
			BudgetConfirmed: item.BudgetConfirmed, ResetStatus: resetStatus,
			TargetCurrentSpend: targetSpend, ObservedCurrentSpend: item.ObservedSpend,
		})
		if item.Before.Spend != nil && *item.Before.Spend >= change.Amount {
			warning := fmt.Sprintf("成员 %s 本周期费用已达到或超过新额度，生效后可能立即被限制。", item.Before.UserID)
			if change.ResetCurrentCycleSpend {
				warning = fmt.Sprintf("成员 %s 原周期费用已达到或超过新额度；清零完成前可能被限制，清零失败时额度仍可能已经更新。", item.Before.UserID)
			}
			output.Warnings = append(output.Warnings, warning)
		}
	}
	return output
}

func validSpend(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func numberPointer(value any) *float64 {
	if number, ok := value.(float64); ok {
		return &number
	}
	return nil
}

func stringPointer(value any) *string {
	if text, ok := value.(string); ok {
		return &text
	}
	return nil
}
