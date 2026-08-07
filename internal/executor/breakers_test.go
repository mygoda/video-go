package executor

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock 让熔断的时间转移可以被精确断言，而不必真的 sleep 30s。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testBreaker() (CircuitBreaker, *fakeClock) {
	clk := newClock()
	b := NewCircuitBreaker(BreakerDeps{
		Config: BreakerConfig{FailureThreshold: 3, OpenDuration: 30 * time.Second, HalfOpenProbes: 1},
		Now:    clk.Now,
	})
	return b, clk
}

// TestBreakerClosedToOpen 验证连续失败到阈值才打开，且是**连续**而非累计。
func TestBreakerClosedToOpen(t *testing.T) {
	ctx := context.Background()
	b, _ := testBreaker()

	for i := 0; i < 2; i++ {
		_ = b.Failure(ctx, "p1")
	}
	if st, _ := b.State(ctx, "p1"); st != BreakerClosed {
		t.Fatalf("未到阈值不该熔断，实际 %s", st)
	}

	// 一次成功清零：99% 成功但零星失败的供应商不该被熔断。
	_ = b.Success(ctx, "p1")
	for i := 0; i < 2; i++ {
		_ = b.Failure(ctx, "p1")
	}
	if st, _ := b.State(ctx, "p1"); st != BreakerClosed {
		t.Fatalf("成功应清零连续失败计数，实际 %s", st)
	}

	_ = b.Failure(ctx, "p1")
	if st, _ := b.State(ctx, "p1"); st != BreakerOpen {
		t.Fatalf("连续 3 次失败应打开，实际 %s", st)
	}
	if ok, _ := b.Allow(ctx, "p1"); ok {
		t.Fatal("open 时必须拒绝放行")
	}
}

// TestBreakerOpenToHalfOpenToClosed 验证完整的恢复路径。
func TestBreakerOpenToHalfOpenToClosed(t *testing.T) {
	ctx := context.Background()
	b, clk := testBreaker()

	for i := 0; i < 3; i++ {
		_ = b.Failure(ctx, "p1")
	}
	if ok, _ := b.Allow(ctx, "p1"); ok {
		t.Fatal("刚打开就该拒绝")
	}

	clk.Advance(30 * time.Second)

	// State 看一眼不该消耗探测名额。
	if st, _ := b.State(ctx, "p1"); st != BreakerHalfOpen {
		t.Fatalf("到点应转半开，实际 %s", st)
	}
	if st, _ := b.State(ctx, "p1"); st != BreakerHalfOpen {
		t.Fatalf("重复看状态仍应是半开，实际 %s", st)
	}

	// 第一个请求拿到探针名额。
	if ok, _ := b.Allow(ctx, "p1"); !ok {
		t.Fatal("半开应放出第一个探针")
	}
	// 名额用完，其余继续被挡——否则积压任务会把刚恢复的上游再打死。
	if ok, _ := b.Allow(ctx, "p1"); ok {
		t.Fatal("HalfOpenProbes=1 时第二个请求必须被挡")
	}

	_ = b.Success(ctx, "p1")
	if st, _ := b.State(ctx, "p1"); st != BreakerClosed {
		t.Fatalf("探针成功应闭合，实际 %s", st)
	}
	if ok, _ := b.Allow(ctx, "p1"); !ok {
		t.Fatal("闭合后应正常放行")
	}
}

// TestBreakerHalfOpenFailureReopensAndResetsTimer 验证探针失败后重新计时。
//
// 不重新计时的话，nextProbeAt 已经是过去时刻，下一个请求会立刻又拿到探针，
// 半开就退化成了完全放行。
func TestBreakerHalfOpenFailureReopensAndResetsTimer(t *testing.T) {
	ctx := context.Background()
	b, clk := testBreaker()

	for i := 0; i < 3; i++ {
		_ = b.Failure(ctx, "p1")
	}
	clk.Advance(30 * time.Second)
	if ok, _ := b.Allow(ctx, "p1"); !ok {
		t.Fatal("应放出探针")
	}

	_ = b.Failure(ctx, "p1")
	if st, _ := b.State(ctx, "p1"); st != BreakerOpen {
		t.Fatalf("探针失败应退回 open，实际 %s", st)
	}
	if ok, _ := b.Allow(ctx, "p1"); ok {
		t.Fatal("退回 open 后必须重新计时，不能立刻再放探针")
	}

	clk.Advance(30 * time.Second)
	if ok, _ := b.Allow(ctx, "p1"); !ok {
		t.Fatal("再过一个 OpenDuration 应能再探一次")
	}
}

// TestBreakerIsolatedPerProvider 验证熔断粒度是供应商而不是全局。
//
// 全局熔断等于把一家的故障放大成全站故障——这是整个故障隔离设计的核心。
func TestBreakerIsolatedPerProvider(t *testing.T) {
	ctx := context.Background()
	b, _ := testBreaker()

	for i := 0; i < 5; i++ {
		_ = b.Failure(ctx, "ark")
	}
	if ok, _ := b.Allow(ctx, "ark"); ok {
		t.Fatal("ark 应被熔断")
	}
	if ok, _ := b.Allow(ctx, "openai"); !ok {
		t.Fatal("openai 不该被 ark 的故障牵连")
	}
}

// TestAvailableProvidersFiltersOpenOnly 验证只有 open 被摘掉，half_open 算可领。
//
// 半开就是要放一两个真实请求试水，而试水的载体正是任务；把 half_open 也摘掉
// 会让熔断器永远等不到能让它恢复的那个请求。
func TestAvailableProvidersFiltersOpenOnly(t *testing.T) {
	ctx := context.Background()
	b, clk := testBreaker()

	for i := 0; i < 3; i++ {
		_ = b.Failure(ctx, "down")
	}
	for i := 0; i < 3; i++ {
		_ = b.Failure(ctx, "recovering")
	}
	clk.Advance(30 * time.Second)
	// down 也到点了，把它重新打回 open 以便区分两者。
	if ok, _ := b.Allow(ctx, "down"); !ok {
		t.Fatal("应放出探针")
	}
	_ = b.Failure(ctx, "down")

	got, err := AvailableProviders(ctx, b, []string{"down", "recovering", "healthy"})
	if err != nil {
		t.Fatalf("AvailableProviders: %v", err)
	}
	want := map[string]bool{"recovering": true, "healthy": true}
	if len(got) != 2 {
		t.Fatalf("应保留 2 家，实际 %v", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("不该保留 %s，实际 %v", id, got)
		}
	}

	// 熔断器缺席时不过滤：降级运行好过一个都领不到。
	all, err := AvailableProviders(ctx, nil, []string{"a", "b"})
	if err != nil || len(all) != 2 {
		t.Fatalf("nil 熔断器应原样返回，got %v err %v", all, err)
	}
}

// TestBreakerSnapshotIsSorted 验证快照按 provider id 排序。
//
// map 遍历每次顺序都不同，管理员刷新一下列表就整个重排，那种页面没法看。
func TestBreakerSnapshotIsSorted(t *testing.T) {
	ctx := context.Background()
	b, _ := testBreaker()

	for _, id := range []string{"zeta", "alpha", "mid"} {
		_ = b.Failure(ctx, id)
	}
	snap, err := b.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap) != 3 {
		t.Fatalf("应有 3 条，实际 %d", len(snap))
	}
	for i := 1; i < len(snap); i++ {
		if snap[i-1].ProviderID > snap[i].ProviderID {
			t.Fatalf("快照未按 id 排序：%v", snap)
		}
	}
}

// TestBreakerEmptyProviderIDAlwaysAllows 验证空 provider id 不参与熔断。
//
// 它出现在配置不全的边角情形里，此时熔断一个"没有名字的供应商"
// 会连带挡住所有同样没名字的调用。
func TestBreakerEmptyProviderIDAlwaysAllows(t *testing.T) {
	ctx := context.Background()
	b, _ := testBreaker()
	for i := 0; i < 10; i++ {
		_ = b.Failure(ctx, "")
	}
	if ok, _ := b.Allow(ctx, ""); !ok {
		t.Fatal("空 provider id 应始终放行")
	}
}

// TestBreakerConcurrentAccess 在 -race 下验证并发安全。
func TestBreakerConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	b, _ := testBreaker()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = b.Allow(ctx, "p1")
				if j%3 == 0 {
					_ = b.Failure(ctx, "p1")
				} else {
					_ = b.Success(ctx, "p1")
				}
				_, _ = b.State(ctx, "p1")
				_, _ = b.Snapshot(ctx)
			}
		}(i)
	}
	wg.Wait()
}
