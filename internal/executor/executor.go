package executor

import (
	"context"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Lease 是一条被 worker 领取的任务租约。
//
// Owner 标识领取者（进程 id + 随机后缀），ExpiresAt 是租约到期时刻。
// 租约过期而任务仍未终结时，它自动回到可领取状态被别的 worker 接手——
// 这是进程崩溃后任务能自愈的全部机制。
type Lease struct {
	Task      domain.Task
	Owner     string
	ExpiresAt time.Time
}

// Queue 是任务队列，实现落在 store/mysql 上（用数据库行做租约，不引入额外中间件）。
//
// Claim 原子地领取一条可执行任务并打上租约。providerIDs 限定只领这几家供应商的
// 任务，是故障隔离的一部分：熔断打开的供应商不出现在这个列表里，它的任务就
// 留在队列里等熔断恢复，而不是被领出来一个个超时。
// 没有可领的任务时返回 (Lease{}, false, nil)，这不是错误。
//
// Renew 延长租约。长任务（视频动辄几分钟）必须周期性调用它，
// 否则租约过期会被另一个 worker 重复领取，同一个任务被提交两次到上游。
//
// Release 主动放弃租约。任务进入终态时调用，或 worker 优雅退出时把在途任务
// 立刻还回队列（不还也行，等租约过期，但那要多等一个租约周期）。
type Queue interface {
	Claim(ctx context.Context, owner string, ttl time.Duration, providerIDs []string) (Lease, bool, error)
	Renew(ctx context.Context, taskID, owner string, ttl time.Duration) error
	Release(ctx context.Context, taskID, owner string) error

	// ClaimPoll 领取一批到点该轮询的 running 任务（next_poll_at <= now），
	// 同样打租约。它与 Claim 分开，是因为两者扫的是不同的索引、也不该互相
	// 饿死：一个 worker 池被大量待轮询任务占满时，新提交的任务仍要能被领走。
	ClaimPoll(ctx context.Context, owner string, ttl time.Duration, limit int) ([]Lease, error)
}

// Runner 执行一条已领取的任务。
//
// Run 覆盖从 queued → 终态的完整过程：取模型与供应商配置 → 渲染上游请求 →
// 调 driver（同步族一次拿结果，异步族提交后交给 PollScheduler）→ 转存产物 →
// 派生缩略图 → 记录血缘 → 结算积分 → 发 SSE 事件。
//
// 实现必须在整个过程中周期性 Queue.Renew，并在返回前保证任务处于终态或
// 已交接给轮询调度（不能既不是终态又没人管）。
//
// Cancel 尽力取消一条任务：queued 直接置 canceled 并全额退回冻结额度；
// running 时上游支持取消就调上游，不支持就本地标记为 canceled。
// **两种情况都不扣费。**
type Runner interface {
	Run(ctx context.Context, lease Lease) error
	Cancel(ctx context.Context, taskID string) error
}

// PollScheduler 驱动异步任务的轮询。
//
// Schedule 登记一条已提交到上游的任务，按 interval 周期性轮询直至终态。
// interval 取 ModelConfig.PollIntervalSeconds，为空时取 driver 的 DefaultPollInterval。
//
// Advance 用一次轮询结果推进状态机。它是**幂等**的，且是 webhook 与定时轮询
// 共用的那一段代码：webhook 收到上游回调后同样调用 Advance，
// 效果等价于"把下一次轮询提前到现在"。同一个结果重复 Advance、
// 或新旧结果乱序到达，最终状态必须一致。
//
// Stop 停止对某任务的轮询，任务进终态或被取消时调用。
type PollScheduler interface {
	Schedule(ctx context.Context, taskID string, interval time.Duration) error
	Advance(ctx context.Context, taskID string, upstreamStatus string) error
	Stop(ctx context.Context, taskID string) error
}
