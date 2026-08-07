package executor

import (
	"math/rand/v2"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Delay 返回第 n 次重试（n 从 1 起）前的等待时长。
//
//	delay = min(BaseDelay * Multiplier^(n-1), MaxDelay)
//	delay = delay * (1 + rand(-Jitter, +Jitter))
//
// 抖动在**封顶之后**加，因此实际等待可能略超 MaxDelay。反过来（先抖动再封顶）
// 会让所有触到上限的重试被压回同一个值，惊群正好在退避最久、积压最多的
// 那一档复活——而那正是最需要打散的时候。
func (p RetryPolicy) Delay(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := float64(p.BaseDelay)
	m := p.Multiplier
	if m <= 0 {
		m = 1
	}
	for i := 1; i < n; i++ {
		d *= m
		if p.MaxDelay > 0 && d >= float64(p.MaxDelay) {
			d = float64(p.MaxDelay)
			break
		}
	}
	if p.MaxDelay > 0 && d > float64(p.MaxDelay) {
		d = float64(p.MaxDelay)
	}

	j := p.Jitter
	if j > 0 {
		if j > 1 {
			j = 1
		}
		// rand.Float64() ∈ [0,1) → 因子 ∈ [1-j, 1+j)
		d *= 1 + j*(2*rand.Float64()-1)
	}
	if d < 0 {
		d = 0
	}
	return time.Duration(d)
}

// ShouldRetry 判断第 attempt 次尝试失败后是否还该再来一次。
//
// attempt 是**已经用掉的尝试次数**（含首次），因此 attempt >= MaxAttempts
// 就是耗尽。判定同时看错误的可重试性：这个标志由 driver 在归类上游错误时给出，
// L2 不认识上游错误码，也就不该在这里二次猜测。
//
// terr 为 nil 时按可重试处理：那是"调用本身失败了但没归类"的情形
// （网络错、超时），这类天然该退避重试。
func (p RetryPolicy) ShouldRetry(attempt int, terr *domain.TaskError) bool {
	if attempt >= p.MaxAttempts {
		return false
	}
	if terr == nil {
		return true
	}
	return IsRetryable(terr.Code)
}

// IsRetryable 报告一个失败分类是否值得重试。
//
// 这份表是「重试对哪些失败有意义」的唯一出处，不看 TaskError.Retryable：
// 那个字段是给**前端**看的语义（invalid_param 标 retryable=true，意思是
// 「你改完参数再提交一次」，那是用户的动作），而自动重试问的是
// 「同样的请求再发一次会不会不一样」——参数错了发一万次还是错。
// 两个问题共用一个布尔会让机器替用户做无意义的重试。
func IsRetryable(code domain.TaskErrorCode) bool {
	switch code {
	case domain.TaskErrorUpstreamRateLimited, domain.TaskErrorInternal:
		return true
	case domain.TaskErrorInvalidParam, domain.TaskErrorContentRejected, domain.TaskErrorInsufficientCredit:
		return false
	default:
		// 未知分类当作不可重试：重试一个我们不理解的失败，最坏情况是
		// 反复对上游发起计费调用，而那是真金白银。
		return false
	}
}

// RetryInfoFor 拼出挂在 TaskError 上的重试进度，供前端显示
// 「正在自动重试 (2/5)」。next 为零值时不带下次时刻。
func RetryInfoFor(p RetryPolicy, attempt int, next time.Time) *domain.RetryInfo {
	info := &domain.RetryInfo{Attempt: attempt, MaxAttempts: p.MaxAttempts}
	if !next.IsZero() {
		at := next.UTC()
		info.NextAt = &at
	}
	return info
}
