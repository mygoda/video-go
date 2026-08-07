// Command aigcd is the AIGC Pool backend server.
//
// 三个子命令：
//
//	aigcd serve          启动 HTTP 服务与 worker 池（默认，不带参数时即此）
//	aigcd migrate up     应用全部未应用的迁移
//	aigcd migrate down N 回滚最高的 N 个版本（默认 1）
//	aigcd seed           建初始管理员与普通用户
//
// migrate 做成子命令而不是让 README 要求先装 golang-migrate：
// 「照着 README 走能从零到可用」这条验收线不该卡在装工具上。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/aigc-pool/aigc-pool/internal/config"
	"github.com/aigc-pool/aigc-pool/internal/httpapi"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		err = run()
	case "migrate":
		err = runMigrate(args)
	case "seed":
		err = runSeed(args)
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, usage)
		return
	default:
		err = fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}

	if err != nil {
		slog.Error("aigcd exited with error", "cmd", cmd, "err", err)
		os.Exit(1)
	}
}

const usage = `usage: aigcd [command]

  serve            start the HTTP server and worker pool (default)
  migrate up       apply all pending migrations
  migrate down [n] roll back the highest n versions (default 1)
  seed             create the initial admin and regular user

configuration is read from AIGC_-prefixed environment variables; see
configs/.env.example.
`

// mustAtoi 解析子命令参数里的整数，缺省时返回 def。
func mustAtoi(args []string, def int) (int, error) {
	if len(args) == 0 {
		return def, nil
	}
	n, err := strconv.Atoi(args[0])
	if err != nil {
		return 0, fmt.Errorf("expected an integer, got %q", args[0])
	}
	return n, nil
}

// run 承载真正的启动逻辑。
//
// 与直接写在 main 里相比，它能用 return err 表达失败，让 os.Exit 只出现在
// 一个地方——os.Exit 不跑 defer，散落在各处会让清理逻辑静默失效。
func run() error {
	logger := slog.Default()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger.Info("configuration loaded", "config", cfg.Redacted())

	app, err := build(cfg)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	httpapi.Register(mux, app.deps)

	// NotifyContext 在收到 SIGINT / SIGTERM 时取消 ctx。
	// SIGINT 是本地 Ctrl-C，SIGTERM 是容器编排发的停止信号，两个都要接——
	// 只接一个会导致另一种场景下进程被直接杀掉，在途请求全部断连。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := app.start(ctx); err != nil {
		app.stop(context.Background())
		return err
	}
	logger.Info("schema version", "version", currentVersion(ctx, app.db))

	srv := &http.Server{
		Addr:        cfg.HTTPAddr,
		Handler:     mux,
		ReadTimeout: cfg.ReadTimeout,
		// WriteTimeout 默认为 0（不限制）。SSE 是长连接，任何非零写超时
		// 都会在到点时把正在推事件的连接掐断。
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

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
		app.stop(context.Background())
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
		app.stop(shutdownCtx)
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	// worker 在 HTTP 之后停：反过来的话，还在接的请求会提交出一批
	// 没有 worker 去执行的任务。
	app.stop(shutdownCtx)
	// 等 ListenAndServe 那条 goroutine 真正收尾，避免进程先于它退出。
	if err := <-errCh; err != nil {
		return err
	}

	logger.Info("aigcd stopped cleanly")
	return nil
}
