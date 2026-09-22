// AvdbAsst 是一个自托管的资源搜索站。
//
// 它把上游 Avdb API 包成一个可以直接被浏览器"自定义搜索引擎"调用的网页：
//
//	https://<你的域名>/s?q=<关键词>
//
// 配合仓库内的 .github/workflows/docker-publish.yml，推送到 main 分支会
// 自动构建多架构镜像并推送到 Docker Hub。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println("avdbasst", version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, notes := ResolveConfig()
	logger.Info("启动中", "version", version, "addr", cfg.Addr, "upstream", cfg.APIBaseURL)
	for _, n := range notes {
		logger.Warn("配置", "detail", n)
	}
	for _, warning := range cfg.Warnings() {
		logger.Warn("配置", "detail", warning)
	}

	if err := cfg.Validate(); err != nil {
		logger.Error("配置无效，无法启动", "err", err)
		os.Exit(1)
	}

	app := NewApp(cfg, logger)
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	// 收到 SIGINT/SIGTERM 时取消 ctx，触发优雅退出。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("服务已就绪", "url", "http://localhost"+normalizeAddr(cfg.Addr), "search", "http://localhost"+normalizeAddr(cfg.Addr)+"/s?q=关键词")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		logger.Error("监听失败", "err", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅关闭")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("优雅关闭超时，强制退出", "err", err)
		_ = server.Close()
		os.Exit(1)
	}
	logger.Info("已退出")
}
