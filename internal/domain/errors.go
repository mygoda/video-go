package domain

// ErrorCode 是 HTTP 层错误码，取值域 = TaskErrorCode 全集 + 若干传输层码。
//
// 分成两个枚举而不是往 TaskErrorCode 里塞，是为了让 TaskErrorCode 与
// TypeScript 契约保持逐值可对拍：任务失败分类是产品语义，401/403/404 是传输语义，
// 混在一起前端的失败三分类分支会被污染。
//
// 前端遇到未知 code 按 internal_error 降级呈现，不允许白屏。
type ErrorCode string

const (
	// 与 TaskErrorCode 逐值对应的产品语义码。
	CodeInvalidParam        ErrorCode = "invalid_param"
	CodeUpstreamRateLimited ErrorCode = "upstream_rate_limited"
	CodeContentRejected     ErrorCode = "content_rejected"
	CodeInsufficientCredit  ErrorCode = "insufficient_credit"
	CodeInternal            ErrorCode = "internal_error"

	// 传输语义码。
	CodeUnauthorized     ErrorCode = "unauthorized"
	CodeForbidden        ErrorCode = "forbidden"
	CodeNotFound         ErrorCode = "not_found"
	CodeConflict         ErrorCode = "conflict"
	CodeRevisionConflict ErrorCode = "revision_conflict"
	CodeRateLimited      ErrorCode = "rate_limited"
	CodeQuotaExceeded    ErrorCode = "quota_exceeded"
)

// Error 是全平台统一的错误类型，序列化后即 openapi 的 ApiError。
//
// 所有错误响应统一包在 ErrorEnvelope 里，前端严格按 Code 分支，不解析 Message。
// Message 是给人看的，Code 是给程序看的——这条边界不能模糊。
//
// Err 保存底层原因，供服务端日志与 errors.Is/As 链式判定使用，不序列化下发。
type Error struct {
	Code        ErrorCode    `json:"code"`
	Message     string       `json:"message"`
	FieldErrors []FieldError `json:"field_errors,omitempty"`
	Retryable   bool         `json:"retryable"`
	Charged     bool         `json:"charged"`
	Retry       *RetryInfo   `json:"retry,omitempty"`
	Err         error        `json:"-"`
}

// Error 实现 error 接口。
func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// Unwrap 暴露底层原因，使 errors.Is / errors.As 能穿透本类型。
func (e *Error) Unwrap() error { return e.Err }

// ErrorEnvelope 是**所有**错误响应的统一外壳。
type ErrorEnvelope struct {
	Error Error `json:"error"`
}
