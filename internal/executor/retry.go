package executor

import "time"

// RetryPolicy 是指数退避重试策略。
//
// 第 n 次重试（n 从 1 起）的等待时长:
//
//	delay = min(BaseDelay * Multiplier^(n-1), MaxDelay)
//	delay = delay * (1 + rand(-Jitter, +Jitter))
//
// **Jitter 不是可选的。** 没有抖动时，一家供应商恢复的瞬间，所有积压任务会在
// 同一毫秒一起重试打过去，把刚恢复的服务再打挂一次（惊群）。抖动把这波流量
// 摊开在一个时间窗里。
//
// 重试只对**可重试**的失败生效：upstream_rate_limited 和 internal_error 重试；
// invalid_param 与 content_rejected 不重试（参数错了重试多少次还是错，
// 审核拒了重试也还是拒），insufficient_credit 更不重试。
// 这个判定看 domain.TaskError.Retryable，由 driver 归类时给出。
//
// 重试期间任务停留在 running 而不是回到 queued：用户看到的是"正在自动重试 (2/5)"，
// 冻结的积分也不释放——重试成功后要用同一笔额度结算，中途退回再冻结会在
// 余额不足时把任务卡死在一个本来能成功的位置。
type RetryPolicy struct {
	// MaxAttempts 是总尝试次数（含首次），1 表示不重试。
	MaxAttempts int
	// BaseDelay 是首次重试前的等待时长。
	BaseDelay time.Duration
	// MaxDelay 是单次等待的上限，防止指数增长到荒谬的量级。
	MaxDelay time.Duration
	// Multiplier 是每次退避的倍率。
	Multiplier float64
	// Jitter ∈ [0,1]，抖动比例。0.2 表示实际等待在计算值的 ±20% 内随机。
	Jitter float64
}

// DefaultRetryPolicy 是通用重试策略。
//
// 5 次尝试、2s 起步、2 倍退避、封顶 60s，总时长约 2+4+8+16 = 30s 加抖动。
// 取这个量级的理由：上游限流通常是秒级到十几秒的窗口，30s 内重试足够穿过去；
// 再长用户就该看到失败而不是一直等着——生成任务本身就是分钟级的，
// 在重试上再耗几分钟会让"到底还要多久"变得不可解释。
var DefaultRetryPolicy = RetryPolicy{
	MaxAttempts: 5,
	BaseDelay:   2 * time.Second,
	MaxDelay:    60 * time.Second,
	Multiplier:  2.0,
	Jitter:      0.2,
}

// PollRetryPolicy 是轮询失败时的重试策略。
//
// 与 DefaultRetryPolicy 分开是因为语义不同：提交失败是"这次调用没成功"，
// 轮询失败是"查状态时网络抖了一下，任务本身可能好好的"。后者更该密集地多试几次，
// 因为放弃轮询意味着把一条可能已经成功的任务判死，白扔掉一次已经花掉的上游生成。
var PollRetryPolicy = RetryPolicy{
	MaxAttempts: 10,
	BaseDelay:   3 * time.Second,
	MaxDelay:    30 * time.Second,
	Multiplier:  1.5,
	Jitter:      0.2,
}
