package main

import (
	"context"
	"fmt"
	"time"
)

func expectedBudgetSnapshot(change *budgetChange, item *changeItem) memberSnapshot {
	expected := item.Before
	expected.Budget = budgetSettings(item.Before.Budget)
	expected.Budget["max_budget"] = change.Amount
	return expected
}

func validResetReceipt(change *budgetChange, item *changeItem) bool {
	receipt := item.ResetReceipt
	return receipt != nil && receipt.TeamID == item.Before.TeamID && receipt.UserID == item.Before.UserID &&
		receipt.Spend != nil && *receipt.Spend == 0 &&
		receipt.PreviousSpend != nil && validSpend(*receipt.PreviousSpend) &&
		receipt.MaxBudget != nil && *receipt.MaxBudget == change.Amount
}

func (s *budgetAdminService) resetCurrentCycleSpend(ctx context.Context, change *budgetChange, item *changeItem) error {
	item.Status, item.ResetStatus = "partial", "skipped"
	item.Detail = "额度已确认，但清零前检查未完成，尚未提交清零"
	if ctx.Err() != nil {
		item.Detail = "额度已确认，但调用已取消，未提交清零"
		return nil
	}
	current, err := s.current(ctx, item.Before, change.TeamAlias)
	if err != nil {
		item.Detail += "：" + err.Error()
		return nil
	}
	if !sameConfiguration(expectedBudgetSnapshot(change, item), current) ||
		*current.Spend < *item.Before.Spend || *current.TotalSpend < *item.Before.TotalSpend {
		item.Status, item.Detail = "unconfirmed", "清零前预算配置或消费计数发生非预期变化，未提交清零，请人工核查"
		return nil
	}
	item.ResetBeforeSpend, item.ResetBeforeTotalSpend = current.Spend, current.TotalSpend
	item.ObservedSpend = current.Spend
	item.Status, item.ResetStatus = "inflight", "inflight"
	item.Detail = "额度已确认，即将提交当前周期消费清零"
	// 清零会删除当前计数，必须在请求前持久化独立意图，恢复时绝不能重发。
	if err := s.store.save(change); err != nil {
		return fmt.Errorf("额度可能已生效，但清零前审计保存失败，尚未提交清零；请使用同一 change_id 恢复")
	}
	receipt, resetErr := s.api.ResetSpend(ctx, current.TeamID, current.UserID)
	item.ResetReceipt = receipt
	if !validResetReceipt(change, item) {
		item.ResetReceipt = nil
	} else if err := s.store.save(change); err != nil {
		return fmt.Errorf("清零响应已确认，但审计保存失败；请使用同一 change_id 回读，禁止重新清零")
	}
	verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
	defer cancel()
	s.verifyReset(verifyCtx, change, item)
	if resetErr != nil {
		if item.Status == "applied" {
			item.Detail += "；清零请求响应异常，但回读已确认消费为零"
		} else {
			item.Detail += "；清零请求响应异常，不代表未生效"
		}
	}
	return nil
}

func (s *budgetAdminService) verifyReset(ctx context.Context, change *budgetChange, item *changeItem) {
	item.Status, item.ResetStatus = "unconfirmed", "unconfirmed"
	item.Detail = "额度此前已确认，但清零结果尚未确认；禁止重复清零，请管理员核查"
	current, err := s.current(ctx, item.Before, change.TeamAlias)
	if err != nil {
		item.Detail += "：" + err.Error()
		return
	}
	item.ObservedSpend = current.Spend
	if !sameConfiguration(expectedBudgetSnapshot(change, item), current) ||
		item.ResetBeforeTotalSpend == nil || *current.TotalSpend < *item.ResetBeforeTotalSpend {
		item.Detail = "清零后预算配置或累计消费出现非预期变化，请人工核查；不会自动回滚或再次清零"
		return
	}
	item.BudgetConfirmed = true
	// 成功回执证明请求执行时为零；随后新消费可以增长。丢失回执时只接受直接观察到零。
	if !validResetReceipt(change, item) && *current.Spend != 0 {
		item.Detail = "额度已确认，但未取得有效清零回执，且当前消费不为零，不能区分未清零与清零后新增消费；请人工核查"
		return
	}
	item.Status, item.ResetStatus = "applied", "confirmed"
	item.Detail = "额度与当前周期消费清零已确认，累计消费未下降，其他预算配置未变化"
}
