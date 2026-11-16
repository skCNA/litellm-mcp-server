package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

const changeLifetime = 10 * time.Minute

type memberSnapshot struct {
	TeamID     string         `json:"team_id"`
	UserID     string         `json:"user_id"`
	Email      string         `json:"user_email"`
	BudgetID   string         `json:"budget_id"`
	Budget     map[string]any `json:"budget"`
	Spend      *float64       `json:"spend"`
	TotalSpend *float64       `json:"total_spend"`
}

type changeItem struct {
	Before                memberSnapshot     `json:"before"`
	Status                string             `json:"status"`
	Detail                string             `json:"detail"`
	BudgetConfirmed       bool               `json:"budget_confirmed"`
	ResetStatus           string             `json:"reset_status,omitempty"`
	ResetBeforeSpend      *float64           `json:"reset_before_spend,omitempty"`
	ResetBeforeTotalSpend *float64           `json:"reset_before_total_spend,omitempty"`
	ResetReceipt          *spendResetReceipt `json:"reset_receipt,omitempty"`
	ObservedSpend         *float64           `json:"observed_current_cycle_spend"`
}

type budgetChange struct {
	ID                     string       `json:"change_id"`
	Owner                  string       `json:"owner"`
	Target                 string       `json:"target"`
	TeamAlias              string       `json:"team_alias"`
	Scope                  string       `json:"scope"`
	Amount                 float64      `json:"max_budget"`
	ResetCurrentCycleSpend bool         `json:"reset_current_cycle_spend"`
	CreatedAt              time.Time    `json:"created_at"`
	ExpiresAt              time.Time    `json:"expires_at"`
	StartedAt              *time.Time   `json:"started_at,omitempty"`
	UpdatedAt              time.Time    `json:"updated_at"`
	State                  string       `json:"state"`
	Items                  []changeItem `json:"items"`
}

type changeStore struct{ dir string }

func newChangeStore(dir string) (*changeStore, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("MCP_CHANGE_DIR 必须是绝对路径")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("创建变更目录失败: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("变更目录必须为真实目录且权限为 0700")
	}
	return &changeStore{dir: dir}, nil
}

func newChangeID() (string, error) {
	var data [24]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("生成变更编号失败: %w", err)
	}
	return hex.EncodeToString(data[:]), nil
}

var changeIDPattern = regexp.MustCompile(`^[0-9a-f]{48}$`)

func (s *changeStore) load(id string) (*budgetChange, error) {
	if !changeIDPattern.MatchString(id) {
		return nil, fmt.Errorf("change_id 无效")
	}
	data, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if err != nil {
		return nil, fmt.Errorf("变更记录不存在或无法读取")
	}
	var change budgetChange
	if err := json.Unmarshal(data, &change); err != nil || change.ID != id || !validStoredChange(&change) {
		return nil, fmt.Errorf("变更记录损坏，拒绝执行")
	}
	return &change, nil
}

func validStoredChange(change *budgetChange) bool {
	if change.Owner == "" || change.Target == "" || change.TeamAlias == "" ||
		change.Amount <= 0 || math.IsNaN(change.Amount) || math.IsInf(change.Amount, 0) ||
		len(change.Items) == 0 || len(change.Items) > maxChangeMembers ||
		change.CreatedAt.IsZero() || !change.ExpiresAt.After(change.CreatedAt) ||
		(change.State != "preview" && change.State != "executing" && change.State != "done") ||
		(change.Scope != "member" && change.Scope != "team_members") ||
		(change.Scope == "member" && len(change.Items) != 1) {
		return false
	}
	seen := map[string]bool{}
	teamID := change.Items[0].Before.TeamID
	for _, item := range change.Items {
		member := item.Before
		if member.TeamID == "" || member.TeamID != teamID || seen[member.UserID] {
			return false
		}
		seen[member.UserID] = true
		var team teamInfo
		team.Team.ID = teamID
		team.Team.Members = []teamMember{{UserID: member.UserID, UserEmail: member.Email, Role: "user"}}
		team.Memberships = []teamMembership{{
			TeamID: teamID, UserID: member.UserID, BudgetID: member.BudgetID,
			Budget: member.Budget, Spend: member.Spend, TotalSpend: member.TotalSpend,
		}}
		if _, err := snapshotMember(team, member.UserID); err != nil {
			return false
		}
		switch item.Status {
		case "pending", "inflight", "applied", "unchanged", "conflict", "skipped", "unconfirmed", "partial":
		default:
			return false
		}
		if (change.State == "preview" && item.Status != "pending") ||
			(change.State == "done" && (item.Status == "pending" || item.Status == "inflight")) {
			return false
		}
		if change.ResetCurrentCycleSpend {
			if (change.State == "preview" && item.ResetStatus != "pending") ||
				(change.State == "done" && (item.ResetStatus == "pending" || item.ResetStatus == "inflight")) ||
				item.Status == "unchanged" ||
				(item.Status == "partial" && (!item.BudgetConfirmed || item.ResetStatus != "skipped")) {
				return false
			}
			switch item.ResetStatus {
			case "pending", "skipped":
			case "inflight", "confirmed", "unconfirmed":
				if item.ResetBeforeSpend == nil || item.ResetBeforeTotalSpend == nil ||
					!validSpend(*item.ResetBeforeSpend) || !validSpend(*item.ResetBeforeTotalSpend) {
					return false
				}
			default:
				return false
			}
			if item.Status == "applied" && item.ResetStatus != "confirmed" {
				return false
			}
			if (item.ResetStatus == "inflight" && item.Status != "inflight") ||
				(item.ResetStatus == "confirmed" && item.Status != "applied") ||
				(item.ResetStatus == "unconfirmed" && item.Status != "unconfirmed") {
				return false
			}
		} else if item.ResetStatus != "" || item.ResetReceipt != nil {
			return false
		}
	}
	return true
}

func (s *changeStore) save(change *budgetChange) error {
	change.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(change, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(s.dir, ".change-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(data); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), filepath.Join(s.dir, change.ID+".json")); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *changeStore) loadKey(id string) (*keyBudgetChange, error) {
	if !changeIDPattern.MatchString(id) {
		return nil, fmt.Errorf("change_id 无效")
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "key-"+id+".json"))
	if err != nil {
		return nil, fmt.Errorf("key 变更记录不存在或无法读取")
	}
	var change keyBudgetChange
	if err := json.Unmarshal(data, &change); err != nil || change.ID != id || !validKeyBudgetChange(&change) {
		return nil, fmt.Errorf("key 变更记录损坏，拒绝执行")
	}
	return &change, nil
}

func (s *changeStore) saveKey(change *keyBudgetChange) error {
	change.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(change, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(s.dir, ".key-change-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(data); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), filepath.Join(s.dir, "key-"+change.ID+".json")); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *changeStore) loadKeyTPM(id string) (*keyModelTPMChange, error) {
	if !changeIDPattern.MatchString(id) {
		return nil, fmt.Errorf("change_id 无效")
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "key-tpm-"+id+".json"))
	if err != nil {
		return nil, fmt.Errorf("key TPM 变更记录不存在或无法读取")
	}
	var change keyModelTPMChange
	if err := json.Unmarshal(data, &change); err != nil || change.ID != id || !validKeyModelTPMChange(&change) {
		return nil, fmt.Errorf("key TPM 变更记录损坏，拒绝执行")
	}
	return &change, nil
}

func (s *changeStore) saveKeyTPM(change *keyModelTPMChange) error {
	if !validKeyModelTPMChange(change) {
		return fmt.Errorf("key TPM 变更记录无效，拒绝保存")
	}
	change.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(change, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(s.dir, ".key-tpm-change-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(data); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), filepath.Join(s.dir, "key-tpm-"+change.ID+".json")); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// 锁覆盖执行与记录落盘，保证同目录的多进程不会重复提交；等待可被调用上下文取消。
func (s *changeStore) lock(ctx context.Context) (func(), error) {
	file, err := os.OpenFile(filepath.Join(s.dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
