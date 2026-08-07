package executor

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// breakers 是 CircuitBreaker 的进程内实现。
//
// 状态放内存而不是数据库，是一个有意的取舍：熔断是**本实例观测到的**
// 上游健康度，它天然是局部的、几十秒内失效的。放进数据库意味着每次
// Allow 都要读一次库——而 Allow 在每条任务的最热路径上——换来的只是
// 多实例间共享一份可能已经过期的判断。多实例各自熔断的代价是恢复时
// 每个实例各放一个探针，那完全可以接受。
//
// Snapshot 因此展示的是本实例的视角，管理后台看到的是"这台机器上
// 这家供应商现在能不能用"。
type breakers struct {
	cfg BreakerConfig
	now func() time.Time

	mu    sync.Mutex
	state map[string]*breakerEntry
}

// breakerEntry 是单个供应商的熔断状态。
type breakerEntry struct {
	state BreakerState
	// failures 是**连续**失败计数，任何一次成功都清零。
	// 用连续而不是窗口内累计：一家 99% 成功但零星失败的供应商不该被熔断，
	// 而累计计数在足够长的时间里一定会攒够阈值。
	failures    int
	openedAt    time.Time
	nextProbeAt time.Time
	// probesInFlight 是半开态已放出的探测数，达到 HalfOpenProbes 后
	// 后续请求继续被挡，直到探测有了结果。
	probesInFlight int
}

// BreakerDeps 是熔断器的依赖。
type BreakerDeps struct {
	// Config 为零值时取 DefaultBreakerConfig。
	Config BreakerConfig
	// Now 供测试注入时钟，为 nil 时取 time.Now。
	Now func() time.Time
}

// NewCircuitBreaker 组装一组按供应商隔离的熔断器。
func NewCircuitBreaker(d BreakerDeps) CircuitBreaker {
	cfg := d.Config
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = DefaultBreakerConfig.FailureThreshold
	}
	if cfg.OpenDuration <= 0 {
		cfg.OpenDuration = DefaultBreakerConfig.OpenDuration
	}
	if cfg.HalfOpenProbes <= 0 {
		cfg.HalfOpenProbes = DefaultBreakerConfig.HalfOpenProbes
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &breakers{cfg: cfg, now: now, state: make(map[string]*breakerEntry)}
}

// Allow 在放行前调用，返回 false 表示该供应商当前被熔断。
//
// open 到点后在这里转半开并放出一个探针——转移由**请求**驱动而不是定时器：
// 没有请求时转不转半开没有任何区别，而一个后台 goroutine 要为每个见过的
// 供应商维护一个 timer，多出来的那份生命周期管理换不来任何东西。
func (b *breakers) Allow(_ context.Context, providerID string) (bool, error) {
	if providerID == "" {
		return true, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.entry(providerID)
	now := b.now()

	switch e.state {
	case BreakerOpen:
		if now.Before(e.nextProbeAt) {
			return false, nil
		}
		e.state = BreakerHalfOpen
		e.probesInFlight = 1
		return true, nil
	case BreakerHalfOpen:
		if e.probesInFlight >= b.cfg.HalfOpenProbes {
			// 半开的意义就是只用一两个请求试水。把积压的任务一次性放出去，
			// 会把刚恢复的上游再打死一次。
			return false, nil
		}
		e.probesInFlight++
		return true, nil
	default:
		return true, nil
	}
}

// Success 上报一次成功调用。
//
// 无论此前是什么状态都直接闭合并清零：半开的探针成功了说明上游回来了，
// 闭合态的成功说明它一直好着。没有"逐步恢复"的中间态——那需要一个
// 成功计数阈值，而多等几个成功才放行只会让恢复变慢，换不来更多信息。
func (b *breakers) Success(_ context.Context, providerID string) error {
	if providerID == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.entry(providerID)
	e.state = BreakerClosed
	e.failures = 0
	e.probesInFlight = 0
	e.openedAt = time.Time{}
	e.nextProbeAt = time.Time{}
	return nil
}

// Failure 上报一次失败调用，必要时打开熔断。
//
// 半开态下的任何一次失败立刻退回 open 并**重新计时**：探针失败说明
// 上游还没好，此时不该继续按原来的到点时间放下一个探针。
func (b *breakers) Failure(_ context.Context, providerID string) error {
	if providerID == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.entry(providerID)
	now := b.now()

	if e.state == BreakerHalfOpen {
		b.open(e, now)
		return nil
	}

	e.failures++
	if e.state == BreakerClosed && e.failures >= b.cfg.FailureThreshold {
		b.open(e, now)
	}
	return nil
}

// State 返回某供应商当前的熔断状态。
//
// 它按当前时刻推算 open → half_open 的到点转移，但**不**消耗探测名额：
// 只是看一眼状态的调用方（管理后台、Claim 的供应商过滤）不该因为看了一眼
// 就把那个宝贵的探测名额用掉。
func (b *breakers) State(_ context.Context, providerID string) (BreakerState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.entry(providerID)
	if e.state == BreakerOpen && !b.now().Before(e.nextProbeAt) {
		return BreakerHalfOpen, nil
	}
	return e.state, nil
}

// Snapshot 返回全部已知供应商的熔断状态，按 provider id 排序。
//
// 排序是为了让 GET /api/admin/circuit-breakers 的输出稳定——map 遍历
// 每次顺序都不同，管理员刷新一下列表就整个重排，那种页面没法看。
func (b *breakers) Snapshot(_ context.Context) ([]domain.BreakerSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	out := make([]domain.BreakerSnapshot, 0, len(b.state))
	for id, e := range b.state {
		s := domain.BreakerSnapshot{
			ProviderID:   id,
			State:        string(e.state),
			FailureCount: e.failures,
		}
		if e.state == BreakerOpen && !now.Before(e.nextProbeAt) {
			s.State = string(BreakerHalfOpen)
		}
		if !e.openedAt.IsZero() {
			at := e.openedAt
			s.OpenedAt = &at
		}
		if !e.nextProbeAt.IsZero() {
			at := e.nextProbeAt
			s.NextProbeAt = &at
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProviderID < out[j].ProviderID })
	return out, nil
}

// open 把一个熔断器打开并重新计时。调用方必须持锁。
func (b *breakers) open(e *breakerEntry, now time.Time) {
	e.state = BreakerOpen
	e.openedAt = now.UTC()
	e.nextProbeAt = now.Add(b.cfg.OpenDuration).UTC()
	e.probesInFlight = 0
	if e.failures < b.cfg.FailureThreshold {
		e.failures = b.cfg.FailureThreshold
	}
}

// entry 取（必要时创建）某供应商的状态。调用方必须持锁。
func (b *breakers) entry(providerID string) *breakerEntry {
	e, ok := b.state[providerID]
	if !ok {
		e = &breakerEntry{state: BreakerClosed}
		b.state[providerID] = e
	}
	return e
}

// AvailableProviders 从候选里筛掉当前被熔断的供应商，结果交给 Queue.Claim。
//
// **这是熔断真正省下资源的地方。** 只在 Allow 处快速失败的话，被熔断那家的
// 任务仍然会被一条条领出队列、判失败、退款——用户看到的是一批任务瞬间全挂。
// 把它从 Claim 的供应商列表里摘掉，那些任务安安静静留在队列里等上游恢复，
// 恢复后照常执行，用户全程只觉得慢了一点。
//
// half_open 算可领：半开就是要放一两个真实请求试水，而试水的载体正是任务。
func AvailableProviders(ctx context.Context, b CircuitBreaker, candidates []string) ([]string, error) {
	if b == nil {
		return candidates, nil
	}
	out := make([]string, 0, len(candidates))
	for _, id := range candidates {
		st, err := b.State(ctx, id)
		if err != nil {
			return nil, err
		}
		if st == BreakerOpen {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}
