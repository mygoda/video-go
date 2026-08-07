package adapter

import (
	"fmt"
	"net/http"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// 失败分类的两条硬规则，写成代码而不是留给各 driver 自觉:
//
//  1. Retryable 由错误码唯一决定。让 driver 各自填这个布尔，迟早会出现
//     "同样是 400，ark 说可重试、sora 说不可"——L2 的重试循环就成了看运气。
//  2. Charged 恒为 false。失败三分类一律不扣费是产品承诺，
//     由本层断言而不是靠调用方记得传 false。

// retryableByCode 是错误码到可重试性的唯一映射表。
//
// invalid_param 可重试指的是"用户改完参数重提"，不是系统自动重试——
// 这个区别由 L2 的 RetryPolicy 决定要不要真的自动重来，本层只声明语义。
// content_rejected 不可重试：同一段提示词再送一万次仍然会被审核拒掉。
var retryableByCode = map[domain.TaskErrorCode]bool{
	domain.TaskErrorInvalidParam:        true,
	domain.TaskErrorUpstreamRateLimited: true,
	domain.TaskErrorContentRejected:     false,
	domain.TaskErrorInsufficientCredit:  false,
	domain.TaskErrorInternal:            true,
}

// Failure 构造一个归一化后的任务失败，用于 PollResult.Err。
//
// 它表达的是"上游那个任务失败了"，与 CallError 表达的"这次调用没打通"
// 是两件事：前者是终态，L2 要落库并退款；后者可能只是网络抖动，L2 会重试。
func Failure(code domain.TaskErrorCode, format string, args ...any) *domain.TaskError {
	return &domain.TaskError{
		Code:      code,
		Message:   fmt.Sprintf(format, args...),
		Retryable: retryableByCode[code],
		Charged:   false,
	}
}

// FailureWithFields 在 Failure 的基础上带上字段定位，供上游 400 能指到具体参数时使用。
// Key 必须是 ParamSpec.Key / InputSlotSpec.Key，即**平台侧**的名字——
// 回填上游字段名会让前端高亮不到任何一枚芯片。
func FailureWithFields(code domain.TaskErrorCode, message string, fields []domain.FieldError) *domain.TaskError {
	err := Failure(code, "%s", message)
	err.FieldErrors = fields
	return err
}

// CallError 构造一次上游调用本身的失败（连不上、非 2xx、响应体解不开）。
//
// 返回 *domain.Error 而不是裸 errors.New，是为了让 L2 能用 errors.As 拿到分类，
// 从而决定重试还是快速失败——否则 L2 只能去 match 错误文案，
// 那等于把上游的错误串又泄漏到了 L0 之上。
func CallError(code domain.TaskErrorCode, cause error, format string, args ...any) *domain.Error {
	return &domain.Error{
		Code:      domain.ErrorCode(code),
		Message:   fmt.Sprintf(format, args...),
		Retryable: retryableByCode[code],
		Charged:   false,
		Err:       cause,
	}
}

// ClassifyHTTPStatus 把 HTTP 状态码归到平台的失败分类。
//
// 这是**兜底**：各 driver 应当先看上游自己的错误码（Ark 的 SensitiveContentDetected、
// OpenAI 的 moderation_blocked 都是 400，光看状态码会误判成 invalid_param），
// 认不出来才落到这里。
func ClassifyHTTPStatus(status int) domain.TaskErrorCode {
	switch {
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		return domain.TaskErrorInvalidParam
	case status == http.StatusPaymentRequired,
		status == http.StatusRequestTimeout,
		status == http.StatusTooManyRequests:
		// 402 是上游账户欠费/配额耗尽。它不是平台的 insufficient_credit
		// （那说的是用户的积分），归到限流类：都属于"等一等或换条路"。
		return domain.TaskErrorUpstreamRateLimited
	case status >= 500:
		return domain.TaskErrorInternal
	}
	return domain.TaskErrorInternal
}
