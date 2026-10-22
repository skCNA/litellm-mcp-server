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
	return server
}

func serveMCP(listenAddress, authToken string, pool *pgxpool.Pool) error {
	mcpServer := newMCPServer(pool)
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{SessionTimeout: mcpSessionLimit},
	)

	mux := http.NewServeMux()
	mux.Handle(mcpEndpoint, bearerAuth(authToken, mcpHandler))

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
