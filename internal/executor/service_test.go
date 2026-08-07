package executor

import (
	"context"
	"sync"
	"testing"
	"time"
)

// trackingQueue 记下 Claim / Renew / Release 的调用，用来验证租约的生命周期。
type trackingQueue struct {
	mu       sync.Mutex
	claimed  []Lease
	released []string
	renewed  map[string]int
	// pending 是待发放的租约，Claim 逐个取走。
	pending []Lease
}

func newTrackingQueue(pending ...Lease) *trackingQueue {
	return &trackingQueue{pending: pending, renewed: make(map[string]int)}
}

func (q *trackingQueue) Claim(_ context.Context, owner string, _ time.Duration, _ []string) (Lease, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return Lease{}, false, nil
	}
	l := q.pending[0]
	q.pending = q.pending[1:]
	l.Owner = owner
	q.claimed = append(q.claimed, l)
	return l, true, nil
}

func (q *trackingQueue) Renew(_ context.Context, taskID, _ string, _ time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.renewed[taskID]++
	return nil
}

func (q *trackingQueue) Release(_ context.Context, taskID, _ string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.released = append(q.released, taskID)
	return nil
}

func (q *trackingQueue) ClaimPoll(context.Context, string, time.Duration, int) ([]Lease, error) {
	return nil, nil
}

func (q *trackingQueue) releasedIDs() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, len(q.released))
	copy(out, q.released)
	return out
}

// TestStartIsIdempotentGuard 验证重复 Start 会被挡住。
func TestStartIsIdempotentGuard(t *testing.T) {
	s := newTestService(t, newFakeTasks(), &stubDriver{}, nil)
	ctx := context.Background()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("首次 Start: %v", err)
	}
	if err := s.Start(ctx); err == nil {
		t.Fatal("重复 Start 应报错，否则会起两套 worker 池抢同一批任务")
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestStopReleasesInFlightLeases 验证优雅停机把在途租约立刻还回队列。
//
// 不还也能自愈（等租约过期），但那意味着每次滚动重启后在途任务都要多躺
// 一个租约周期才被接管——部署越频繁这个延迟越显眼，而它完全可以避免。
func TestStopReleasesInFlightLeases(t *testing.T) {
	q := newTrackingQueue()
	s := newTestService(t, newFakeTasks(), &stubDriver{}, func(d *Deps) { d.Queue = q })

	// 手工登记两条在途租约，模拟 worker 正在跑。
	s.trackLease("task-a")
	s.trackLease("task-b")

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	got := map[string]bool{}
	for _, id := range q.releasedIDs() {
		got[id] = true
	}
	if !got["task-a"] || !got["task-b"] {
		t.Fatalf("停机应释放全部在途租约，实际释放了 %v", q.releasedIDs())
	}
}

// TestStopWithoutStartIsNoop 验证没启动过就 Stop 不报错。
//
// cmd/aigcd 的 defer Stop 会在启动失败的路径上跑到，那时报错只会掩盖真正的原因。
func TestStopWithoutStartIsNoop(t *testing.T) {
	s := newTestService(t, newFakeTasks(), &stubDriver{}, nil)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("未启动时 Stop 应无操作，实际 %v", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("重复 Stop 应无操作，实际 %v", err)
	}
}

// TestKeepAliveRenewsLease 验证租约在任务执行期间被持续续租。
//
// 不续租的后果是另一个 worker 把同一条任务再提交一次到上游——那是真金白银的
// 重复调用，也是这一层最贵的一种 bug。
func TestKeepAliveRenewsLease(t *testing.T) {
	q := newTrackingQueue()
	s := newTestService(t, newFakeTasks(), &stubDriver{}, func(d *Deps) {
		d.Queue = q
		// TTL/3 = 10ms 的续租节奏，几十毫秒内就能观察到。
		d.LeaseTTL = 30 * time.Millisecond
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := s.keepAlive(ctx, "task-x")
	time.Sleep(120 * time.Millisecond)
	stop()

	q.mu.Lock()
	n := q.renewed["task-x"]
	q.mu.Unlock()
	if n < 2 {
		t.Fatalf("应至少续租 2 次，实际 %d 次", n)
	}

	// stop 之后不该再续。
	time.Sleep(80 * time.Millisecond)
	q.mu.Lock()
	after := q.renewed["task-x"]
	q.mu.Unlock()
	if after != n {
		t.Fatalf("stop 之后不该继续续租，%d → %d", n, after)
	}
}

// TestOwnerIsStableAndUnique 验证租约持有者标识稳定且跨实例不撞。
//
// 少了随机后缀，同一台机器上快速重启后 pid 复用的两个进程会互相认领
// 对方的租约。
func TestOwnerIsStableAndUnique(t *testing.T) {
	a := newTestService(t, newFakeTasks(), &stubDriver{}, nil)
	b := newTestService(t, newFakeTasks(), &stubDriver{}, nil)

	if a.Owner() == "" {
		t.Fatal("owner 不能为空")
	}
	if a.Owner() != a.Owner() {
		t.Fatal("同一实例的 owner 应稳定")
	}
	if a.Owner() == b.Owner() {
		t.Fatalf("两个实例的 owner 不该相同：%s", a.Owner())
	}
}

// TestNewRequiresCoreDeps 验证"少了就一定跑不起来"的依赖会在组装期被拦下。
//
// 让它在 New 报错，好过在第一条任务进来时才 panic——前者是启动失败，
// 后者是带着一个坏掉的执行层对外提供服务。
func TestNewRequiresCoreDeps(t *testing.T) {
	full := func() Deps {
		return Deps{
			Tasks:     newFakeTasks(),
			Models:    stubModels{},
			Providers: stubProviders{},
			Queue:     &fakeQueue{},
			Drivers:   stubRegistry{drv: &stubDriver{}},
			Logger:    discardLogger(),
		}
	}

	cases := map[string]func(*Deps){
		"Tasks":     func(d *Deps) { d.Tasks = nil },
		"Models":    func(d *Deps) { d.Models = nil },
		"Providers": func(d *Deps) { d.Providers = nil },
		"Queue":     func(d *Deps) { d.Queue = nil },
		"Drivers":   func(d *Deps) { d.Drivers = nil },
	}
	for name, drop := range cases {
		d := full()
		drop(&d)
		if _, err := New(d); err == nil {
			t.Errorf("缺少 %s 时 New 应报错", name)
		}
	}

	// 可降级的依赖缺席时仍应能组装出来（本地跑不落库的 mock 链路时有用）。
	if _, err := New(full()); err != nil {
		t.Fatalf("只有核心依赖时应能组装：%v", err)
	}
}

// TestNewAppliesDefaults 验证零值被填成可用的默认参数。
func TestNewAppliesDefaults(t *testing.T) {
	s := newTestService(t, newFakeTasks(), &stubDriver{}, nil)

	if s.deps.LeaseTTL != DefaultLeaseTTL {
		t.Errorf("LeaseTTL = %v, want %v", s.deps.LeaseTTL, DefaultLeaseTTL)
	}
	if s.deps.PollTick != DefaultPollTick {
		t.Errorf("PollTick = %v, want %v", s.deps.PollTick, DefaultPollTick)
	}
	if s.deps.PollBatch != DefaultPollBatch {
		t.Errorf("PollBatch = %d, want %d", s.deps.PollBatch, DefaultPollBatch)
	}
	if s.deps.Concurrency != 1 {
		t.Errorf("Concurrency 应至少为 1，实际 %d", s.deps.Concurrency)
	}
	if s.deps.SubmitRetry.MaxAttempts != DefaultRetryPolicy.MaxAttempts {
		t.Errorf("SubmitRetry 未取默认值：%+v", s.deps.SubmitRetry)
	}
	if s.deps.PollRetry.MaxAttempts != PollRetryPolicy.MaxAttempts {
		t.Errorf("PollRetry 未取默认值：%+v", s.deps.PollRetry)
	}
	if s.Breakers() == nil {
		t.Error("Breakers 缺席时应自动造一个")
	}
	if s.Runner() == nil || s.Poller() == nil {
		t.Error("Runner / Poller 不该为 nil")
	}
}

// TestPollerStopIsSeparateFromServiceStop 验证两个 Stop 各司其职。
//
// PollScheduler.Stop(ctx, taskID) 只停一条任务的轮询，绝不能顺手把整个
// 执行层停掉——webhook 处理器每收到一条终态回调都会调它。
func TestPollerStopIsSeparateFromServiceStop(t *testing.T) {
	s := newTestService(t, newFakeTasks(), &stubDriver{}, nil)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Stop(context.Background()) }()

	s.intervals.Store("task-1", time.Second)
	s.inputs.Store("task-1", []string{"a1"})

	if err := s.Poller().Stop(context.Background(), "task-1"); err != nil {
		t.Fatalf("Poller().Stop: %v", err)
	}
	if _, ok := s.intervals.Load("task-1"); ok {
		t.Error("停止轮询后应清掉间隔缓存")
	}
	if _, ok := s.inputs.Load("task-1"); ok {
		t.Error("停止轮询后应清掉输入缓存")
	}

	// 执行层本身还活着。
	s.mu.Lock()
	stopped := s.stopped
	s.mu.Unlock()
	if stopped {
		t.Fatal("停一条任务的轮询不该把整个执行层停掉")
	}
}
