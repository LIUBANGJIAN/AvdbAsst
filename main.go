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

// version 是**语义化版本号**，显示在页面右上角。
//
// 它刻意不是"序号/构建号"：用户要的是"这个容器跑的是哪一版功能"，
// 而 `main@abc1234` 这种串只能回答"哪次提交"，答不了"比上一版多了什么"。
// 每次代码改动都要递增这里的值——**任何**改动，不只是新功能
// （这源自用户的明确要求："任何代码改动，都变更版本号"）。
// 打 tag 发布时由 CI 用 tag 覆盖它。
var version = "v1.2.1"

// build 是构建标识（分支@短 sha），用于区分同一版本号的不同构建。
// 它不进右上角的徽标，只在设置页「运行信息」与 /healthz 里出现。
var build = "dev"

func main() {
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println("avdbasst", version, build)
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
