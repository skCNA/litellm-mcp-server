package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

type adminConfig struct {
	token  string
	base   string
	key    string
	dir    string
	max    float64
	keyMax float64
}

func loadAdminConfig(readToken string) (*adminConfig, error) {
	names := []string{"MCP_WRITE_AUTH_TOKEN", "LITELLM_URL", "LITELLM_MASTER_KEY", "MCP_CHANGE_DIR", "MCP_MAX_MEMBER_BUDGET"}
	values := make([]string, len(names))
	enabled := false
	keyMaxSetting := strings.TrimSpace(os.Getenv("MCP_MAX_KEY_BUDGET"))
	for i, name := range names {
		values[i] = os.Getenv(name)
		enabled = enabled || values[i] != ""
	}
	enabled = enabled || keyMaxSetting != ""
	if !enabled {
		return nil, nil
	}
	for i, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("启用预算修改必须配置 %s", names[i])
		}
	}
	if len(values[0]) < authTokenMinLength || values[0] == readToken ||
		strings.TrimSpace(values[0]) != values[0] {
		return nil, fmt.Errorf("MCP_WRITE_AUTH_TOKEN 必须至少 32 字节、无首尾空白且不同于只读令牌")
	}
	max, err := strconv.ParseFloat(values[4], 64)
	if err != nil || max <= 0 || math.IsNaN(max) || math.IsInf(max, 0) {
		return nil, fmt.Errorf("MCP_MAX_MEMBER_BUDGET 必须为有限正数")
	}
	keyMax := max
	if keyMaxSetting != "" {
		keyMax, err = strconv.ParseFloat(keyMaxSetting, 64)
		if err != nil || keyMax <= 0 || math.IsNaN(keyMax) || math.IsInf(keyMax, 0) {
			return nil, fmt.Errorf("MCP_MAX_KEY_BUDGET 必须为有限正数")
		}
	}
	return &adminConfig{token: values[0], base: values[1], key: values[2], dir: values[3], max: max, keyMax: keyMax}, nil
}

func (c *adminConfig) service(pool budgetQuerier) (*budgetAdminService, error) {
	api, err := newAdminClient(c.base, c.key)
	if err != nil {
		return nil, err
	}
	store, err := newChangeStore(c.dir)
	if err != nil {
		return nil, err
	}
	owner := sha256.Sum256([]byte(c.token))
	service := &budgetAdminService{
		query: &budgetService{pool: pool}, api: api, safety: &postgresBudgetSafety{pool: pool},
		store: store, owner: hex.EncodeToString(owner[:]), target: api.base, max: c.max, now: time.Now,
	}
	service.keyBudgets = &keyBudgetAdminService{
		query: &keyBudgetService{pool: pool}, api: api, store: store,
		owner: service.owner, target: service.target, max: c.keyMax, now: time.Now,
	}
	return service, nil
}
