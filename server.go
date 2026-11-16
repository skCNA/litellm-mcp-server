package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpEndpoint         = "/mcp"
	httpReadHeaderLimit = 5 * time.Second
	httpIdleLimit       = 60 * time.Second
	httpShutdownLimit   = 10 * time.Second
	mcpSessionLimit     = 30 * time.Minute
	maxHTTPHeaderBytes  = 1 << 20
)

func newMCPServer(pool budgetQuerier) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "litellm", Version: "v1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name: "query_team_member_budget",
		Description: "查询 LiteLLM 团队成员费用与预算详情，至少提供 team_alias 或 user_email；" +
			"team_alias 按团队别名精确匹配；user_email 接受完整邮箱或前缀，按 @ 前的完整内容区分大小写精确匹配，忽略域名，不做模糊匹配。" +
			"只提供 team_alias 时返回该团队所有成员的完整记录，只提供 user_email 时返回该前缀在各域名和团队的完整记录，" +
			"同时提供时必须满足两个条件。同前缀可能属于不同用户，应依据返回的 user_id 区分，不应自动合并身份。" +
			"返回所有匹配记录，包含团队和用户标识、邮箱、本周期费用、累计费用、预算上限及预算来源。" +
			"数据仅来自 " + budgetViewName + "；成员预算不代表团队总预算。",
	}, (&budgetService{pool: pool}).QueryTeamMemberBudget)
	mcp.AddTool(server, &mcp.Tool{
		Name: "query_team_key_budget",
		Description: "查询 LiteLLM 团队下虚拟 key 的预算与消费，只允许按 team_alias 精确查询，可选 key_alias 精确筛选。" +
			"按页返回 key 名称、所属用户、消费、key 自身额度、关联预算额度、生效额度、周期和状态；" +
			"不返回 key/token 或其哈希。数据仅来自 " + keyBudgetViewName + "。",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, (&keyBudgetService{pool: pool}).QueryTeamKeyBudget)
	mcp.AddTool(server, &mcp.Tool{
		Name: "query_team_key_model_tpm_limit",
		Description: "只读查询团队 key 的按模型 TPM 限制。team_alias 必填并精确匹配，可选 key_alias 精确筛选；" +
			"按页返回 key_model_tpm_limit 与 team_model_tpm_limit 完整映射。空映射表示该层未配置按模型 TPM 限制；不返回 key/token 或其哈希。",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, (&keyModelTPMQueryService{pool: pool}).QueryTeamKeyModelTPMLimit)
	return server
}

func newAdminMCPServer(pool budgetQuerier, admin *budgetAdminService) *mcp.Server {
	server := newMCPServer(pool)
	if admin.keyBudgets != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name: "preview_team_key_budget_change",
			Description: "预览指定团队下单个 key 的 max_budget 修改，不写入 LiteLLM。" +
				"team_alias 与 key_alias 必须精确匹配唯一目标；新额度必须为正数且不超过管理员上限。" +
				"budget_duration 可选，省略时使用 1d；可指定如 7d、30d、1mo、weekly 的周期。" +
				"返回 key 自身额度、关联预算、生效额度和预期变更；关联预算更低时，最终生效额度仍由关联预算限制。" +
				"预算周期变更会保留当前消费，不修改关联预算或 TPM/RPM。必须先向管理员完整展示预览并取得确认，再调用执行工具。",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, admin.keyBudgets.Preview)
		mcp.AddTool(server, &mcp.Tool{
			Name: "apply_team_key_budget_change",
			Description: "执行已经预览且经管理员明确确认的单个 key 预算修改。" +
				"只提交 key_alias、team_id、max_budget 与预算周期至 LiteLLM /key/update；执行前复核目标和预算配置，执行后回读确认。" +
				"同一 change_id 不重复提交；结果不确定时必须人工核查，不得自动生成新计划重试。",
			Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(true)},
		}, admin.keyBudgets.Apply)
	}
	if admin.keyTPM != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name: "preview_team_key_model_tpm_change",
			Description: "预览团队下单个 key 对一个 model 的 model_tpm_limit 设置、修改或清除，不写入 LiteLLM。" +
				"team_alias、key_alias 和 model_name 均精确匹配；action=set 时 tpm_limit 必须为正整数，action=remove 时只清除该模型在 key 上的条目并保留其他模型配置。" +
				"返回变更前后的完整 key model_tpm_limit 映射及该模型的团队级 TPM 限制；团队限制独立生效。必须先向管理员展示完整预览并取得确认，再调用执行工具。",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, admin.keyTPM.Preview)
		mcp.AddTool(server, &mcp.Tool{
			Name: "apply_team_key_model_tpm_change",
			Description: "执行已经预览且经管理员明确确认的单个 key、单个 model TPM 限制变更。" +
				"只向 LiteLLM /key/update 提交 team_id、key_alias 和完整 model_tpm_limit 映射；执行前复核 key 与团队 TPM 快照，执行后回读确认。" +
				"同一 change_id 不重复提交；结果不确定时必须人工核查，不得自动生成新计划重试。",
			Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(true)},
		}, admin.keyTPM.Apply)
	}
	mcp.AddTool(server, &mcp.Tool{
		Name: "preview_member_budget_change",
		Description: "预览指定团队普通成员的预算上限修改，不写入 LiteLLM。" +
			"必须明确选择单成员或团队批量模式；仅支持现有独立预算，管理员、共享预算和歧义目标均拒绝。" +
			"可选 reset_current_cycle_spend 默认 false，只有显式 true 才计划同时清零当前周期已消费金额；不会清空累计消费或修改周期。" +
			"返回十分钟有效的 change_id、冻结成员名单、旧额度、新额度、清零选择和风险。必须向管理员完整展示并获得确认后才可调用执行工具。",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, admin.Preview)
	mcp.AddTool(server, &mcp.Tool{
		Name: "apply_member_budget_change",
		Description: "执行已经预览且经管理员明确确认的成员预算修改。" +
			"默认只修改 max_budget，只有预览显式设置 reset_current_cycle_spend=true 才额外清零当前周期消费；不清空累计消费或修改周期、重置时间。" +
			"额度修改和清零分步执行，可能部分成功；执行时不能改变预览中的清零选择。" +
			"同一 change_id 不重复提交；结果不确定时必须人工核查，不能自行生成新计划重试。",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(true)},
	}, admin.Apply)
	return server
}

func boolPointer(value bool) *bool { return &value }

func newAuthenticatedMCPHandler(readToken, writeToken string, pool budgetQuerier, admin *budgetAdminService) http.Handler {
	handlerFor := func(server *mcp.Server) http.Handler {
		return mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return server },
			&mcp.StreamableHTTPOptions{SessionTimeout: mcpSessionLimit},
		)
	}
	readHandler := bearerAuth(readToken, handlerFor(newMCPServer(pool)))
	var writeHandler http.Handler
	if admin != nil {
		writeHandler = bearerAuth(writeToken, handlerFor(newAdminMCPServer(pool, admin)))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		scheme, credential, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		// 两套处理器隔离会话，不能通过只读令牌借用管理员的会话编号调用写工具。
		if writeHandler != nil && ok && strings.EqualFold(scheme, "Bearer") &&
			subtle.ConstantTimeCompare([]byte(credential), []byte(writeToken)) == 1 {
			writeHandler.ServeHTTP(w, r)
			return
		}
		readHandler.ServeHTTP(w, r)
	})
}

func serveMCP(listenAddress, authToken string, pool *pgxpool.Pool, config *adminConfig) error {
	var admin *budgetAdminService
	writeToken := ""
	if config != nil {
		var err error
		admin, err = config.service(pool)
		if err != nil {
			return fmt.Errorf("初始化预算管理失败: %w", err)
		}
		writeToken = config.token
		log.Print("预算管理已启用，写工具仅对独立管理令牌开放")
	}

	mux := http.NewServeMux()
	mux.Handle(mcpEndpoint, newAuthenticatedMCPHandler(authToken, writeToken, pool, admin))

	httpServer := &http.Server{
		Addr:              listenAddress,
		Handler:           mux,
		ReadHeaderTimeout: httpReadHeaderLimit,
		IdleTimeout:       httpIdleLimit,
		MaxHeaderBytes:    maxHTTPHeaderBytes,
	}

	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("MCP Streamable HTTP 服务监听于 %s%s", listenAddress, mcpEndpoint)
		serverErrors <- httpServer.ListenAndServe()
	}()

	stopContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("MCP HTTP 服务异常退出: %w", err)
	case <-stopContext.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), httpShutdownLimit)
		defer cancel()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("关闭 MCP HTTP 服务失败: %w", err)
		}
		return nil
	}
}

func bearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, credential, found := strings.Cut(r.Header.Get("Authorization"), " ")
		if !found ||
			!strings.EqualFold(scheme, "Bearer") ||
			subtle.ConstantTimeCompare([]byte(credential), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "未授权", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
