package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/adapter/ark"
	"github.com/aigc-pool/aigc-pool/internal/adapter/googlelro"
	"github.com/aigc-pool/aigc-pool/internal/adapter/mapping"
	"github.com/aigc-pool/aigc-pool/internal/adapter/mock"
	"github.com/aigc-pool/aigc-pool/internal/adapter/openaicompat"
	"github.com/aigc-pool/aigc-pool/internal/adapter/openaivideo"
	"github.com/aigc-pool/aigc-pool/internal/asset"
	"github.com/aigc-pool/aigc-pool/internal/canvas"
	"github.com/aigc-pool/aigc-pool/internal/capability"
	"github.com/aigc-pool/aigc-pool/internal/config"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/executor"
	"github.com/aigc-pool/aigc-pool/internal/httpapi"
	"github.com/aigc-pool/aigc-pool/internal/skill"
	mysqlstore "github.com/aigc-pool/aigc-pool/internal/store/mysql"
	"github.com/aigc-pool/aigc-pool/internal/stream"
)

// upstreamHTTPTimeout 是真实驱动发起上游调用的兜底超时。
//
// 单个 provider 可以用 Provider.TimeoutMS 收得更紧（见 executor.invoke），
// 这里给的是"再慢也不能挂着不放"的上限：出图接口偶尔要一分钟以上，
// 而没有超时的客户端会在上游装死时把 worker 名额一个个占光。
const upstreamHTTPTimeout = 5 * time.Minute

// app 是装配完成的进程，run 只负责它的生命周期。
type app struct {
	db   *sql.DB
	exec *executor.Service
	deps httpapi.Deps
}

// build 把配置变成一个可运行的进程。
//
// **全部接线都集中在这里**：哪些驱动被注册、哪个仓储实现被选中、
// 各组件之间怎么串，只有这一个文件知道。内层包互相之间只见接口——
// 这正是 internal/adapter 不 import 任何 driver 包、httpapi 不认识 MySQL
// 的原因。接一家新供应商在这里加一行 MustRegister，别处一行不改。
func build(cfg *config.Config) (*app, error) {
	db, err := mysqlstore.Open(cfg.MySQLDSN)
	if err != nil {
		return nil, err
	}

	blobs, err := asset.NewFSStore(cfg.StorageRoot)
	if err != nil {
		db.Close()
		return nil, err
	}

	users := mysqlstore.NewUserRepo(db)
	providers := mysqlstore.NewProviderRepo(db)
	models := mysqlstore.NewModelRepo(db)
	tasks := mysqlstore.NewTaskRepo(db)
	events := mysqlstore.NewEventRepo(db)
	uploads := mysqlstore.NewUploadRepo(db)
	assets := mysqlstore.NewAssetRepo(db)
	ledgers := mysqlstore.NewLedgerRepo(db)
	canvases := mysqlstore.NewCanvasRepo(db)
	skills := mysqlstore.NewSkillRepo(db)

	evaluator := capability.NewEvaluator()
	drivers := registerDrivers(mapping.NewRenderer(evaluator), cfg)

	quota := mysqlstore.NewQuotaGuard(db)
	transferor, err := asset.NewTransferor(asset.TransferorDeps{
		Blobs:   blobs,
		Assets:  assets,
		Uploads: uploads,
		Tasks:   tasks,
		Drivers: drivers,
		Quota:   quota,
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	deriver, err := asset.NewDeriver(asset.DeriverDeps{Blobs: blobs, Assets: assets})
	if err != nil {
		db.Close()
		return nil, err
	}
	lineage, err := asset.NewLineageRecorder(asset.LineageDeps{Assets: assets})
	if err != nil {
		db.Close()
		return nil, err
	}

	ledger := mysqlstore.NewLedger(db)
	hub := stream.NewHub(stream.HubDeps{Events: events})

	exec, err := executor.New(executor.Deps{
		Tasks:               tasks,
		Models:              models,
		Providers:           providers,
		Assets:              assets,
		Uploads:             uploads,
		Queue:               mysqlstore.NewQueue(db),
		Drivers:             drivers,
		Transferor:          transferor,
		Deriver:             deriver,
		Lineage:             lineage,
		Ledger:              ledger,
		Hub:                 hub,
		Concurrency:         cfg.WorkerConcurrency,
		DefaultPollInterval: cfg.PollInterval,
		PublicBaseURL:       cfg.PublicBaseURL,
		// Credential 留 nil：executor 会退回 os.Getenv(provider.credential_ref)。
		// 凭证只在进程内从环境读到 driver，不入库、不进日志。
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	return &app{
		db:   db,
		exec: exec,
		deps: httpapi.Deps{
			Config: *cfg,
			Tokens: httpapi.NewJWTIssuer(cfg.JWTSecret, time.Now),
			Hasher: httpapi.NewPasswordHasher(),

			Users:     users,
			Providers: providers,
			Models:    models,
			Tasks:     tasks,
			Events:    events,
			Uploads:   uploads,
			Assets:    assets,
			Ledgers:   ledgers,
			Canvases:  canvases,
			Skills:    skills,

			Evaluator: evaluator,
			Pricer:    capability.NewPricer(evaluator),
			Validator: capability.NewValidator(evaluator),

			Drivers: drivers,

			Queue:    mysqlstore.NewQueue(db),
			Runner:   exec.Runner(),
			Poller:   exec.Poller(),
			Breakers: exec.Breakers(),

			Blobs:      blobs,
			Transferor: transferor,
			Deriver:    deriver,
			Lineage:    lineage,
			Quota:      quota,

			Ledger: ledger,
			Canvas: canvas.NewApplier(canvases),
			Skill:  skill.NewRepository(skills),
			Stream: hub,
		},
	}, nil
}

// registerDrivers 装配 L0 驱动注册表。
//
// 每个 driver 实例都是无状态的：base_url、凭证、超时全部随
// adapter.SubmitInput 从模型与供应商配置传入，因此一家供应商配几条模型、
// 甚至同一协议配几家供应商，都只需要这里的一个实例。
//
// **注册表里出现的是协议名，不是供应商名。** "接一个走已知协议的新模型"
// 因此完全不需要碰这个函数——那正是它只有几行的原因。
func registerDrivers(renderer mapping.Renderer, cfg *config.Config) adapter.Registry {
	reg := adapter.NewRegistry()
	mut, ok := reg.(adapter.Mutable)
	if !ok {
		panic("adapter.NewRegistry 的返回值必须实现 adapter.Mutable")
	}

	client := &http.Client{Timeout: upstreamHTTPTimeout}

	// mock 走与真实驱动完全相同的接口与状态机，只把最底层那次 HTTP 换掉。
	// 零凭证环境下它是唯一能把全链路真的跑上一遍的东西。
	// 同步与异步共用 "mock" 这一个查找键，分别落在两张表里（见 adapter.registry）。
	mockCfg := mock.Driver{PlaceholderRoot: mockPlaceholderRoot(cfg)}
	mut.MustRegister(mock.NewSyncDriver(mockCfg))
	mut.MustRegister(mock.NewAsyncDriver(mockCfg))

	mut.MustRegister(openaicompat.New(openaicompat.Driver{
		HTTP: client, Renderer: renderer, ProtocolFamily: domain.FamilyChat,
	}))
	mut.MustRegister(openaicompat.New(openaicompat.Driver{
		HTTP: client, Renderer: renderer, ProtocolFamily: domain.FamilyImages,
	}))

	mut.MustRegister(ark.New(ark.Driver{HTTP: client, Renderer: renderer}))
	mut.MustRegister(openaivideo.New(openaivideo.Driver{HTTP: client, Renderer: renderer}))
	mut.MustRegister(googlelro.New(googlelro.Driver{HTTP: client, Renderer: renderer}))

	slog.Info("drivers registered", "names", reg.Names())
	return reg
}

// mockPlaceholderRoot 让 mock 额外把产物写一份到磁盘，方便不开前端也能
// 双击看一眼。它不是产物交付路径，交付照常走转存。
func mockPlaceholderRoot(cfg *config.Config) string {
	if v := os.Getenv("AIGC_MOCK_PLACEHOLDER_ROOT"); v != "" {
		return v
	}
	return ""
}

// start 起 worker 池与轮询循环。
func (a *app) start(ctx context.Context) error {
	if err := a.exec.Start(ctx); err != nil {
		return fmt.Errorf("start executor: %w", err)
	}
	return nil
}

// stop 收敛后台设施。它在 HTTP server 停完之后调用：反过来的话，
// 还在接的请求会提交出一批没有 worker 去执行的任务。
func (a *app) stop(ctx context.Context) {
	if err := a.exec.Stop(ctx); err != nil {
		slog.Warn("executor did not stop cleanly", "err", err)
	}
	if err := a.db.Close(); err != nil {
		slog.Warn("database did not close cleanly", "err", err)
	}
}
