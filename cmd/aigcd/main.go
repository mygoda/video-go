// Command aigcd is the AIGC Pool backend server.
//
// 子命令：
//
//	aigcd serve          启动 HTTP 服务与 worker 池（默认，不带参数时即此）
//	aigcd migrate up     应用全部未应用的迁移
//	aigcd migrate down N 回滚最高的 N 个版本（默认 1）
//	aigcd seed           建初始管理员与普通用户
//	aigcd backfill-media 给历史资产补上宽高与时长
//
// migrate 做成子命令而不是让 README 要求先装 golang-migrate：
// 「照着 README 走能从零到可用」这条验收线不该卡在装工具上。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
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
	case "backfill-media":
		err = runBackfillMedia(args)
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
  backfill-media   probe stored images/videos and fill in missing
                   width/height/duration (idempotent, safe to re-run)

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

// listenHTTP 占住监听地址，端口被占时给出一条能直接照着做的错误。
//
// 静默失败是这条代码路径上最贵的失败：端口被别人占着而我们一声不吭地退化，
// 表现是「服务看起来活着，但所有请求都落到另一个进程」——2026-08-08 的
// 5.5 小时静默中断正是这么来的。所以这里宁可吵，也不能含糊。
func listenHTTP(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, nil
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return nil, fmt.Errorf(
			"port %s is already in use — another process is listening there.\n"+
				"  find it:      lsof -nP -iTCP:%s -sTCP:LISTEN\n"+
				"  or move on:   %sHTTP_ADDR=:<free port >= %d> aigcd serve\n"+
				"  if you move the backend, point the frontend proxy at it too "+
				"(frontend/.env.local: VITE_API_PROXY_TARGET)",
			addr, portOf(addr), config.Prefix, config.MinPort)
	}
	return nil, fmt.Errorf("listen on %s: %w", addr, err)
}

// portOf 从监听地址里取端口，取不到就原样返回，仅用于拼错误信息。
func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return port
	}
	return addr
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

	// 先抢监听口，再连库起 worker。顺序反过来的话，端口被占的进程会先把库连上、
	// 把 worker 拉起来，直到最后一步才失败退出，中间那段时间它已经在抢任务了。
	// 用显式 net.Listen 而不是 srv.ListenAndServe，也顺带消掉了「先探测再监听」
	// 之间的竞态窗口。
	ln, err := listenHTTP(cfg.HTTPAddr)
	if err != nil {
		return err
	}

	app, err := build(cfg)
	if err != nil {
		_ = ln.Close()
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
		_ = ln.Close()
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
		logger.Info("http server listening", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
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
