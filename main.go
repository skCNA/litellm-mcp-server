package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultListenAddress = ":8080"
	authTokenMinLength   = 32
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("环境变量 DATABASE_URL 未设置")
	}

	authToken := os.Getenv("MCP_AUTH_TOKEN")
	if len(authToken) < authTokenMinLength {
		return fmt.Errorf("环境变量 MCP_AUTH_TOKEN 至少需要 %d 字节", authTokenMinLength)
	}

	listenAddress := os.Getenv("MCP_ADDR")
	if listenAddress == "" {
		listenAddress = defaultListenAddress
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("创建 PostgreSQL 连接池失败: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}

	return serveMCP(listenAddress, authToken, pool)
}
