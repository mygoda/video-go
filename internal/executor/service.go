package executor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/asset"
	"github.com/aigc-pool/aigc-pool/internal/billing"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
	"github.com/aigc-pool/aigc-pool/internal/stream"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// 默认参数。都可以在 Deps 里覆盖，给默认值只是为了让 cmd 里的接线不必
// 为每一个旋钮都想一个数。
const (
	// DefaultLeaseTTL 是租约时长。
	//
	// 30s 是"进程崩了要多久才能被接管"和"续租有多频繁"之间的折中：
	// 更短会让正常运行的 worker 频繁写库续租，更长会让崩溃后的任务多躺一会儿。
	// 视频任务跑几分钟没关系，Runner 全程续租。
	DefaultLeaseTTL = 30 * time.Second

	// DefaultPollBatch 是一次 ClaimPoll 领取的待轮询任务数。
	DefaultPollBatch = 32

	// DefaultPollTick 是轮询调度器扫描到期任务的节奏。
	//
	// 它不是轮询间隔——真正的间隔由每条任务的 next_poll_at 决定。
	// 这个值只决定"到点之后最多晚多久被发现"，2s 的抖动对分钟级的
	// 生成任务毫无影响，而扫得更勤只是白打数据库。
	DefaultPollTick = 2 * time.Second

	// DefaultFirstPollDelay 是提交成功后到第一次轮询的等待。
	//
	// 各家官方建议都在 10s 上下：更早去问，答案必然是 running，
	// 纯粹是一次白打的往返。
	DefaultFirstPollDelay = 10 * time.Second
)

// Canceler 是驱动的可选能力：向上游请求取消一条已提交的任务。
//
// 它不在 adapter.AsyncDriver 里，因为三家供应商里只有一部分支持取消，
// 而把一个半数实现只能返回"不支持"的方法塞进主接口，会让每个新 driver
// 都要写一个空转方法。用可选接口 + 类型断言表达"支持就用，不支持就本地标记"，
// 正是 Runner.Cancel 注释里那句"尽力取消"的落点。
type Canceler interface {
	CancelUpstream(ctx context.Context, req adapter.PollRequest) error
}

// Deps 是 L2 执行层的全部依赖。
//
// 全部以接口形态注入，由 cmd/aigcd 显式接线。摊开成一个结构体而不是
// 一长串构造参数，是因为这一层碰的东西确实多（五个仓储、账本、三个 L3
// 组件、驱动注册表、事件总线），位置参数在第八个之后就没人数得清了。
type Deps struct {
	// ── 存储 ─────────────────────────────────────────────────────
	Tasks store.TaskRepo
	// Models 取模型配置（协议族、请求映射、轮询间隔）。
	Models store.ModelRepo
	// Providers 取供应商接入配置。SubmitInput 需要完整的 domain.Provider
	// （base_url / 鉴权方式 / 超时 / 凭证变量名），而 ModelConfig 只有 provider_id。
	Providers store.ProviderRepo
	// Assets 用于把输入槽里的 asset_id 解析成可喂给上游的素材。
	Assets store.AssetRepo
	// Uploads 用于识别输入槽里的 upload_id。
	Uploads store.UploadRepo
	// Blobs 供 request_mapping 里带 inline 的规则读素材原始字节。为 nil 时
	// 只有那类模型会失败，走地址的模型不受影响。
	Blobs asset.Store

	// ── 队列（实现在 store/mysql，用数据库行做租约）────────────────
	Queue Queue

	// ── L0 协议适配 ──────────────────────────────────────────────
	Drivers adapter.Registry
	// Credential 按 Provider.CredentialRef 取密钥明文，为 nil 时用 os.Getenv。
	// 密钥只在进程内传递，不入库、不进日志。
	Credential func(ref string) string

	// ── L3 资产 ──────────────────────────────────────────────────
	Transferor asset.Transferor
	Deriver    asset.Deriver
	Lineage    asset.LineageRecorder

	// ── 横向服务 ─────────────────────────────────────────────────
	//
	// Ledger 只用 Charge / Refund / Balance：Hold 发生在**提交入口**
	// （余额不足要以 402 同步拒绝、任务根本不入队），到了执行层那笔冻结
	// 早就成立了，这里只负责结算或退还。
	Ledger billing.Ledger
	Hub    stream.Hub

	// ── 执行参数 ─────────────────────────────────────────────────
	//
	// Breakers 为 nil 时由 New 用 DefaultBreakerConfig 造一个。
	Breakers CircuitBreaker
	// Concurrency 是提交 worker 数，<=0 时取 1。
	Concurrency int
	// LeaseTTL <=0 时取 DefaultLeaseTTL。
	LeaseTTL time.Duration
	// PollTick <=0 时取 DefaultPollTick。
	PollTick time.Duration
	// PollBatch <=0 时取 DefaultPollBatch。
	PollBatch int
	// FirstPollDelay <=0 时取 DefaultFirstPollDelay。
	FirstPollDelay time.Duration
	// DefaultPollInterval 在模型与驱动都没给间隔时兜底，<=0 时取 12s。
	DefaultPollInterval time.Duration
	// SubmitRetry 为零值时取 DefaultRetryPolicy。
	SubmitRetry RetryPolicy
	// PollRetry 为零值时取 PollRetryPolicy。
	PollRetry RetryPolicy

	// PublicBaseURL 用于把本地素材拼成上游能回源拉取的地址。
	PublicBaseURL string

	Logger *slog.Logger
	Now    func() time.Time
}

// Service 是 L2 执行层的装配体：它同时是 Runner 与 PollScheduler 的实现，
// 并持有 worker 池与轮询循环的生命周期。
//
// Runner 与 PollScheduler 由同一个类型实现，不是偷懒：提交路径与轮询路径
// 推进的是**同一个状态机**，收尾动作（转存 → 派生 → 血缘 → 结算 → 推事件）
// 一字不差。拆成两个类型意味着这段收尾要么复制两份、要么再抽一个双方都依赖的
// 第三者，而那个第三者会持有和这里一模一样的全部依赖。
type Service struct {
	deps  Deps
	owner string
	log   *slog.Logger
	now   func() time.Time

	breakers CircuitBreaker

	// intervals 记录每条在轮询中的任务的间隔。它是**缓存不是真相**：
	// 真相是 tasks.next_poll_at，进程重启后由别的 worker 按模型配置重新算。
	intervals sync.Map // taskID -> time.Duration

	// inputs 缓存每条在途任务的输入资产 id，供成功时写血缘的 input 边。
	// 同样是缓存：未命中时从 task.Inputs 重新解析（见 recallInputs）。
	inputs sync.Map // taskID -> []string

	// advancing 是进程内的"同一条任务同时只推进一次"闸门。它挡不住别的进程
	// （那由 UpdateStatus 的 CAS 兜底），但挡住了最常见的一种并发：
	// 定时轮询刚捞起一条任务，webhook 就为同一条任务到了。少一次重复的
	// 上游查询，也少一次重复转存同一批产物的机会。
	advancing sync.Map // taskID -> struct{}

	// inflight 是本进程当前持有租约的任务，Stop 时逐条 Release，
	// 让它们立刻回到队列而不是干等一个租约周期。
	mu       sync.Mutex
	inflight map[string]struct{}

	cancel context.CancelFunc
	wg     sync.WaitGroup
	// started / stopped 保证 Start / Stop 各只生效一次。
	started bool
	stopped bool
}

// New 组装执行层。
//
// 只校验"少了就一定跑不起来"的那几项：队列、任务仓储、模型仓储、供应商仓储、
// 驱动注册表。转存/派生/血缘/账本/事件缺席时降级运行（本地跑一个不落库的
// mock 链路时有用），但都会在启动日志里说清楚少了什么——静默降级会让人
// 花半天找"为什么积分没扣"。
func New(d Deps) (*Service, error) {
	switch {
	case d.Tasks == nil:
		return nil, fmt.Errorf("executor: Deps.Tasks 不能为空")
	case d.Models == nil:
		return nil, fmt.Errorf("executor: Deps.Models 不能为空")
	case d.Providers == nil:
		return nil, fmt.Errorf("executor: Deps.Providers 不能为空")
	case d.Queue == nil:
		return nil, fmt.Errorf("executor: Deps.Queue 不能为空")
	case d.Drivers == nil:
		return nil, fmt.Errorf("executor: Deps.Drivers 不能为空")
	}

	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Credential == nil {
		d.Credential = os.Getenv
	}
	if d.Concurrency <= 0 {
		d.Concurrency = 1
	}
	if d.LeaseTTL <= 0 {
		d.LeaseTTL = DefaultLeaseTTL
	}
	if d.PollTick <= 0 {
		d.PollTick = DefaultPollTick
	}
	if d.PollBatch <= 0 {
		d.PollBatch = DefaultPollBatch
	}
	if d.FirstPollDelay <= 0 {
		d.FirstPollDelay = DefaultFirstPollDelay
	}
	if d.DefaultPollInterval <= 0 {
		d.DefaultPollInterval = 12 * time.Second
	}
	if d.SubmitRetry.MaxAttempts <= 0 {
		d.SubmitRetry = DefaultRetryPolicy
	}
	if d.PollRetry.MaxAttempts <= 0 {
		d.PollRetry = PollRetryPolicy
	}

	b := d.Breakers
	if b == nil {
		b = NewCircuitBreaker(BreakerDeps{Config: DefaultBreakerConfig, Now: d.Now})
	}

	for _, missing := range []struct {
		nil  bool
		what string
	}{
		{d.Transferor == nil, "Transferor（产物不会转存，任务不会成功）"},
		{d.Ledger == nil, "Ledger（不结算也不退款）"},
		{d.Hub == nil, "Hub（不推 SSE 事件）"},
		{d.Deriver == nil, "Deriver（不生成缩略图）"},
		{d.Lineage == nil, "LineageRecorder（不记血缘）"},
	} {
		if missing.nil {
			d.Logger.Warn("执行层依赖缺席，相应能力关闭", "missing", missing.what)
		}
	}

	return &Service{
		deps:     d,
		owner:    newOwner(),
		log:      d.Logger,
		now:      d.Now,
		breakers: b,
		inflight: make(map[string]struct{}),
	}, nil
}

// Runner 返回任务执行器。
func (s *Service) Runner() Runner { return s }

// Poller 返回轮询调度器。webhook 处理器拿它调 Advance。
//
// 返回的是一个薄包装而不是 s 本身：PollScheduler.Stop(ctx, taskID) 与
// Service.Stop(ctx) 重名，一个类型挂不下两个 Stop（见 poller 的注释）。
func (s *Service) Poller() PollScheduler { return poller{s: s} }

// Breakers 返回按供应商隔离的熔断器，供管理端查看与 Claim 前过滤。
func (s *Service) Breakers() CircuitBreaker { return s.breakers }

// Owner 是本实例的租约持有者标识，排障时用来认领"这条任务归谁"。
func (s *Service) Owner() string { return s.owner }

// Start 起 worker 池与轮询循环，立即返回。
//
// 它不阻塞：调用方（cmd/aigcd）随后还要起 HTTP server，两者是并列的
// 后台设施。循环内部的错误只记日志不往外抛——一次领取失败不该让整个进程退出，
// 下一轮再试就是了。
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("executor: 已经启动过")
	}
	s.started = true
	s.mu.Unlock()

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel

	for i := 0; i < s.deps.Concurrency; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.submitLoop(runCtx)
		}()
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.pollLoop(runCtx)
	}()

	s.log.Info("执行层已启动",
		"owner", s.owner,
		"concurrency", s.deps.Concurrency,
		"lease_ttl", s.deps.LeaseTTL,
		"poll_tick", s.deps.PollTick)
	return nil
}

// Stop 优雅停机：停止领取新任务，等在途任务收尾，然后释放全部租约。
//
// 释放租约是这里最要紧的一步。不释放也能自愈（等租约过期），但那意味着
// 每次滚动重启后，在途的任务都要多躺一个租约周期才被接管——部署越频繁
// 这个延迟越显眼，而它完全可以避免。
//
// ctx 到期时不再等 worker，直接释放租约走人：停机超时的场景下，
// 把任务尽快还给队列比等一个可能永远等不到的 worker 更重要。
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.started || s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	var waitErr error
	select {
	case <-done:
	case <-ctx.Done():
		waitErr = ctx.Err()
		s.log.Warn("等待 worker 收尾超时，直接释放租约", "err", waitErr)
	}

	s.releaseAll()
	s.log.Info("执行层已停止", "owner", s.owner)
	return waitErr
}

// releaseAll 把本进程还持有的租约全部还回队列。
func (s *Service) releaseAll() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.inflight))
	for id := range s.inflight {
		ids = append(ids, id)
	}
	s.inflight = make(map[string]struct{})
	s.mu.Unlock()

	if len(ids) == 0 {
		return
	}
	// 用一个独立的短超时 ctx：停机 ctx 此时可能已经过期，而释放租约
	// 恰恰是最不该被跳过的收尾动作。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, id := range ids {
		if err := s.deps.Queue.Release(ctx, id, s.owner); err != nil {
			s.log.Error("释放租约失败，任务将等租约自然过期", "task_id", id, "err", err)
		}
	}
}

// trackLease / untrackLease 维护在途租约集合。
func (s *Service) trackLease(taskID string) {
	s.mu.Lock()
	s.inflight[taskID] = struct{}{}
	s.mu.Unlock()
}

func (s *Service) untrackLease(taskID string) {
	s.mu.Lock()
	delete(s.inflight, taskID)
	s.mu.Unlock()
}

// release 归还一条租约并从在途集合里摘掉。
func (s *Service) release(ctx context.Context, taskID string) {
	s.untrackLease(taskID)
	if err := s.deps.Queue.Release(context.WithoutCancel(ctx), taskID, s.owner); err != nil {
		s.log.Warn("释放租约失败，任务将等租约自然过期", "task_id", taskID, "err", err)
	}
}

// keepAlive 在后台周期性续租，直到返回的 stop 被调用。
//
// 续租周期取租约的三分之一：一次失败还能再试两次才到期。取一半的话，
// 一次网络抖动就可能让租约过期，而过期意味着另一个 worker 会把同一条任务
// 再提交一次到上游——那是真金白银的重复调用。
//
// stop 是**同步**的：它一直等到续租协程真的退出才返回。异步的 stop 会留下
// 一次已经在途的 Renew，而调用方 stop 之后紧接着就是 Queue.Release——
// 那次迟到的 Renew 会把刚释放的租约重新续上，任务因此在队列里躺满一个
// TTL 才被别人接管。
func (s *Service) keepAlive(ctx context.Context, taskID string) (stop func()) {
	ttl := s.deps.LeaseTTL
	every := ttl / 3
	if every <= 0 {
		every = time.Second
	}

	done := make(chan struct{})
	exited := make(chan struct{})
	var once sync.Once

	go func() {
		defer close(exited)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				// 每一跳都先复查一次退出信号：ticker 与 done 同时就绪时
				// select 是随机挑一个分支的，不复查就可能在已经该停的时候
				// 再发一次续租。
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				default:
				}
				// 用 WithoutCancel：停机时正在转存的任务仍需要租约，
				// 不然它会在收尾途中被另一个实例接管。
				rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				err := s.deps.Queue.Renew(rctx, taskID, s.owner, ttl)
				cancel()
				if err != nil {
					s.log.Warn("续租失败", "task_id", taskID, "err", err)
				}
			}
		}
	}()

	return func() {
		once.Do(func() { close(done) })
		<-exited
	}
}

// newOwner 生成租约持有者标识：主机名 + pid + 随机后缀。
//
// 三段都要：主机名定位到机器，pid 定位到进程，随机后缀区分同一台机器上
// 快速重启后 pid 复用的两个进程——少了它，旧进程留下的租约会被新进程
// 误认成自己的。
func newOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	if len(host) > 24 {
		host = host[:24]
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), uid.Token(4))
}

// publish 发一条 SSE 事件。失败只记日志：事件是通知，任务状态已经落库，
// 前端还有重连补发和全量对账两条路；让一条推送失败把状态机卡住是本末倒置。
func (s *Service) publish(ctx context.Context, ev stream.Event) {
	if s.deps.Hub == nil {
		return
	}
	if err := s.deps.Hub.Publish(context.WithoutCancel(ctx), ev); err != nil {
		s.log.Warn("推送事件失败", "type", ev.Type, "user_id", ev.UserID, "err", err)
	}
}

// publishCredit 推一条余额变动事件。前端不做乐观扣减，余额一律以服务端为准。
func (s *Service) publishCredit(ctx context.Context, userID string) {
	if s.deps.Hub == nil || s.deps.Ledger == nil {
		return
	}
	bal, err := s.deps.Ledger.Balance(context.WithoutCancel(ctx), userID)
	if err != nil {
		s.log.Warn("读取余额失败，跳过 credit.updated", "user_id", userID, "err", err)
		return
	}
	s.publish(ctx, stream.CreditUpdated(userID, bal))
}

// publishCard 在任务挂在画布上时同步卡片状态。
func (s *Service) publishCard(ctx context.Context, t domain.Task, status domain.TaskStatus, assetID *string) {
	if t.CanvasID == nil || t.CardID == nil {
		return
	}
	s.publish(ctx, stream.CanvasCardUpdated(t.UserID, *t.CanvasID, *t.CardID, status, assetID))
}
