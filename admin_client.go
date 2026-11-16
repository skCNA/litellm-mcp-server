package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type teamMember struct {
	UserID    string `json:"user_id"`
	UserEmail string `json:"user_email"`
	Role      string `json:"role"`
}

type teamMembership struct {
	TeamID     string         `json:"team_id"`
	UserID     string         `json:"user_id"`
	BudgetID   string         `json:"budget_id"`
	Spend      *float64       `json:"spend"`
	TotalSpend *float64       `json:"total_spend"`
	Budget     map[string]any `json:"litellm_budget_table"`
}

type teamInfo struct {
	Team struct {
		ID       string         `json:"team_id"`
		Alias    string         `json:"team_alias"`
		Members  []teamMember   `json:"members_with_roles"`
		Metadata map[string]any `json:"metadata"`
	} `json:"team_info"`
	Memberships []teamMembership `json:"team_memberships"`
}

type budgetAdminAPI interface {
	Team(context.Context, string) (teamInfo, error)
	Update(context.Context, string, string, float64) error
	CheckResetSupport(context.Context) error
	ResetSpend(context.Context, string, string) (*spendResetReceipt, error)
}

func (c *adminClient) CheckKeyBudgetUpdateSupport(ctx context.Context) error {
	var schema struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := c.request(ctx, http.MethodGet, "/openapi.json", nil, &schema); err != nil {
		return fmt.Errorf("无法确认当前 LiteLLM 是否公开 key 预算更新接口，尚未修改预算")
	}
	var operation map[string]json.RawMessage
	if json.Unmarshal(schema.Paths["/key/update"]["post"], &operation) != nil || operation == nil {
		return fmt.Errorf("当前 LiteLLM 未公开 POST /key/update，尚未修改预算")
	}
	return nil
}

func (c *adminClient) CheckKeyModelTPMUpdateSupport(ctx context.Context) error {
	var schema struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := c.request(ctx, http.MethodGet, "/openapi.json", nil, &schema); err != nil {
		return fmt.Errorf("无法确认 LiteLLM 是否支持 key 的 model_tpm_limit，尚未修改")
	}
	var operation struct {
		RequestBody struct {
			Content map[string]struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"content"`
		} `json:"requestBody"`
	}
	if json.Unmarshal(schema.Paths["/key/update"]["post"], &operation) != nil {
		return fmt.Errorf("当前 LiteLLM 未公开 POST /key/update，尚未修改")
	}
	for _, content := range operation.RequestBody.Content {
		if openAPISchemaHasProperty(content.Schema, "model_tpm_limit", schema.Components.Schemas, map[string]bool{}) {
			return nil
		}
	}
	return fmt.Errorf("当前 LiteLLM 未在 POST /key/update 声明 model_tpm_limit，尚未修改")
}

func openAPISchemaHasProperty(raw json.RawMessage, property string, schemas map[string]json.RawMessage, visited map[string]bool) bool {
	var schema struct {
		Ref        string                     `json:"$ref"`
		Properties map[string]json.RawMessage `json:"properties"`
		AllOf      []json.RawMessage          `json:"allOf"`
	}
	if json.Unmarshal(raw, &schema) != nil {
		return false
	}
	if _, ok := schema.Properties[property]; ok {
		return true
	}
	for _, branch := range schema.AllOf {
		if openAPISchemaHasProperty(branch, property, schemas, visited) {
			return true
		}
	}
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(schema.Ref, prefix) {
		return false
	}
	name := strings.TrimPrefix(schema.Ref, prefix)
	if visited[name] {
		return false
	}
	visited[name] = true
	return openAPISchemaHasProperty(schemas[name], property, schemas, visited)
}

type adminClient struct {
	base   string
	key    string
	client *http.Client
}

func newAdminClient(base, key string) (*adminClient, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("LITELLM_URL 必须是无凭证、查询参数和片段的 HTTP(S) 地址，且管理密钥不能为空")
	}
	return &adminClient{
		base: strings.TrimRight(u.String(), "/"), key: key,
		client: &http.Client{
			Timeout: 15 * time.Second,
			// 禁止将管理凭证转发至重定向地址。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (c *adminClient) request(ctx context.Context, method, path string, body any, output any) error {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("管理请求编码失败")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("管理请求创建失败")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		// 不返回原始请求错误或响应体，避免泄露管理地址中的敏感信息及上游数据。
		return fmt.Errorf("LiteLLM 管理请求未确认完成，请检查网络与服务日志")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("LiteLLM 管理请求返回 HTTP %d", res.StatusCode)
	}
	const limit = 8 << 20
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || len(data) > limit {
		return fmt.Errorf("LiteLLM 管理响应读取失败或超出大小限制")
	}
	if output != nil {
		if err := json.Unmarshal(data, output); err != nil {
			return fmt.Errorf("LiteLLM 管理响应结构无效")
		}
	}
	return nil
}

func (c *adminClient) Team(ctx context.Context, id string) (teamInfo, error) {
	var result teamInfo
	// 预算工具不需要密钥列表，限制无关数据的返回量。
	err := c.request(ctx, http.MethodGet, "/team/info?key_limit=1&team_id="+url.QueryEscape(id), nil, &result)
	if err == nil && (result.Team.ID != id || result.Team.Members == nil || result.Memberships == nil) {
		err = fmt.Errorf("LiteLLM 团队信息不完整或目标不一致")
	}
	return result, err
}

func (c *adminClient) Update(ctx context.Context, teamID, userID string, amount float64) error {
	// 只发送额度字段；缺省字段与显式 null 的上游语义不同，不能发送空周期或重置字段。
	return c.request(ctx, http.MethodPost, "/team/member_update", struct {
		TeamID string  `json:"team_id"`
		UserID string  `json:"user_id"`
		Amount float64 `json:"max_budget_in_team"`
	}{teamID, userID, amount}, nil)
}

func (c *adminClient) UpdateKeyBudget(ctx context.Context, teamID, keyAlias string, amount float64, budgetDuration string) error {
	return c.request(ctx, http.MethodPost, "/key/update", struct {
		TeamID         string  `json:"team_id"`
		KeyAlias       string  `json:"key_alias"`
		MaxBudget      float64 `json:"max_budget"`
		BudgetDuration string  `json:"budget_duration"`
	}{teamID, keyAlias, amount, budgetDuration}, nil)
}

func (c *adminClient) UpdateKeyModelTPM(ctx context.Context, teamID, keyAlias string, limits map[string]int64) error {
	if limits == nil {
		limits = map[string]int64{}
	}
	return c.request(ctx, http.MethodPost, "/key/update", struct {
		TeamID        string           `json:"team_id"`
		KeyAlias      string           `json:"key_alias"`
		ModelTPMLimit map[string]int64 `json:"model_tpm_limit"`
	}{teamID, keyAlias, limits}, nil)
}

const memberSpendResetRoute = "/team/{team_id}/member/{user_id}/reset_spend"

type spendResetReceipt struct {
	TeamID        string   `json:"team_id"`
	UserID        string   `json:"user_id"`
	Spend         *float64 `json:"spend"`
	PreviousSpend *float64 `json:"previous_spend"`
	MaxBudget     *float64 `json:"max_budget"`
}

func (c *adminClient) CheckResetSupport(ctx context.Context) error {
	var schema struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := c.request(ctx, http.MethodGet, "/openapi.json", nil, &schema); err != nil {
		return fmt.Errorf("无法确认成员消费清零能力，尚未修改预算: %w", err)
	}
	var operation map[string]json.RawMessage
	if json.Unmarshal(schema.Paths[memberSpendResetRoute]["post"], &operation) != nil || operation == nil {
		return fmt.Errorf("当前 LiteLLM 未公开成员 reset_spend 接口，不能清零；请升级或设置 reset_current_cycle_spend=false，尚未修改预算")
	}
	return nil
}

func (c *adminClient) ResetSpend(ctx context.Context, teamID, userID string) (*spendResetReceipt, error) {
	var result spendResetReceipt
	path := "/team/" + url.PathEscape(teamID) + "/member/" + url.PathEscape(userID) + "/reset_spend"
	err := c.request(ctx, http.MethodPost, path, struct {
		ResetTo float64 `json:"reset_to"`
	}{ResetTo: 0}, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}
