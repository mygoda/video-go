// Command aigcd is the AIGC Pool backend server.
//
// 本阶段（技术设计）它只做三件真事：加载配置、提供 /healthz、优雅停机。
// 业务路由、worker 池与各层实现在实施阶段接进来——接入点已经在下面标出，
// 都是往现成的 ServeMux 上挂 handler、往 shutdown 序列里加一步的事。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/config"
)

func main() {
	if err := run(); err != nil {
		slog.Error("aigcd exited with error", "err", err)
		os.Exit(1)
	}
}

// run 承载真正的启动逻辑。
//
// 与直接写在 main 里相比，它能用 return err 表达失败，让 os.Exit 只出现在
// 一个地方——os.Exit 不跑 defer，散落在各处会让清理逻辑静默失效。
func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger.Info("configuration loaded", "config", cfg.Redacted())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)

	// 实施阶段的接入点:
	//   httpapi.Register(mux, deps)  挂载 /api/*、/api/admin/*、/api/stream、/api/webhooks/*
	//   worker.Start(ctx, deps)      起 cfg.WorkerConcurrency 个 L2 worker

	srv := &http.Server{
		Addr:        cfg.HTTPAddr,
		Handler:     mux,
		ReadTimeout: cfg.ReadTimeout,
		// WriteTimeout 默认为 0（不限制）。SSE 是长连接，任何非零写超时
		// 都会在到点时把正在推事件的连接掐断。
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	// NotifyContext 在收到 SIGINT / SIGTERM 时取消 ctx。
	// SIGINT 是本地 Ctrl-C，SIGTERM 是容器编排发的停止信号，两个都要接——
	// 只接一个会导致另一种场景下进程被直接杀掉，在途请求全部断连。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		// 监听失败（如端口被占）在这里立刻返回，不必等信号。
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received", "timeout", cfg.ShutdownTimeout)
	}

	// 收到信号后先解除信号处理，让第二次 Ctrl-C 能立刻强杀——
	// 停机卡住时用户总得有个办法退出去。
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	// 等 ListenAndServe 那条 goroutine 真正收尾，避免进程先于它退出。
	if err := <-errCh; err != nil {
		return err
	}

	logger.Info("aigcd stopped cleanly")
	return nil
}

// healthz 是存活探针。
//
// 它刻意**不查数据库**：存活探针回答的是"进程还在不在"，就绪探针才回答
// "能不能干活"。把依赖检查塞进 healthz，会让数据库抖一下就把好好的进程重启掉,
// 而重启并不能修好数据库。
func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}
