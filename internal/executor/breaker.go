package executor

import (
	"context"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// BreakerState 是熔断器的三态。
//
//	closed     正常放行。失败计数累积到阈值就转 open。
//	open       直接快速失败，不打上游。到 NextProbeAt 后转 half_open。
//	half_open  放少量探测流量。探测成功转 closed 并清零计数，失败则退回 open
//	           并重新计时——半开态存在的意义就是用一两个请求试水，
//	           而不是把全部积压任务一次性放出去把刚恢复的上游再打死。
type BreakerState string

const (
	BreakerClosed   BreakerState = "closed"
	BreakerOpen     BreakerState = "open"
	BreakerHalfOpen BreakerState = "half_open"
)

// BreakerConfig 是熔断器的参数。
type BreakerConfig struct {
	// FailureThreshold 是连续失败多少次后打开熔断。
	FailureThreshold int
	// OpenDuration 是打开后多久转入半开。
	OpenDuration time.Duration
	// HalfOpenProbes 是半开态允许通过的探测请求数。
	HalfOpenProbes int
}

// DefaultBreakerConfig 是默认熔断参数。
//
// 5 次连续失败即打开：单次偶发抖动不该触发熔断，但连挂 5 次基本可以断定
// 是这家真的不行了，而不是运气差。30s 后探测，是因为多数上游故障
// （限流窗口、临时过载）在这个量级内会自行恢复。
var DefaultBreakerConfig = BreakerConfig{
	FailureThreshold: 5,
	OpenDuration:     30 * time.Second,
	HalfOpenProbes:   1,
}

// CircuitBreaker 是**按供应商**隔离故障的闸门。
//
// 粒度是 provider 而不是全局，这一点是整个故障隔离设计的核心：
// Ark 挂了不该影响走 OpenAI 的任务。全局熔断等于把一家的故障放大成全站故障。
//
// Allow 在放行前调用。返回 false 时任务应立刻以 upstream_rate_limited 失败
// 并**退还冻结积分**——快速失败比让用户等 30s 超时体验更好，
// 而且不占用 worker 名额。
//
// Success / Failure 上报调用结果，驱动状态转移。
//
// Snapshot 供 GET /api/admin/circuit-breakers 展示，让"故障隔离"这件事
// 在管理后台里可观测——否则运维只能靠猜某家是不是被熔断了。
type CircuitBreaker interface {
	Allow(ctx context.Context, providerID string) (bool, error)
	Success(ctx context.Context, providerID string) error
	Failure(ctx context.Context, providerID string) error
	State(ctx context.Context, providerID string) (BreakerState, error)
	Snapshot(ctx context.Context) ([]domain.BreakerSnapshot, error)
}
