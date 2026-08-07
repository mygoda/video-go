package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/capability"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// maxJSONBody 是请求体上限。上传走 multipart 另有上限，这里管的是 JSON 端点——
// 没有上限时一个畸形请求就能把内存吃干净。
const maxJSONBody = 1 << 20 // 1 MiB

// DefaultLimit / MaxLimit 是游标分页的每页条数。
const (
	DefaultLimit = 20
	MaxLimit     = 100
)

// writeJSON 输出一个 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// 头已经发出去了，改不了状态码，只能记一笔。
		slog.Error("write json response", "err", err)
	}
}

// writeError 把任意 error 归一化成 ErrorEnvelope 下发。
//
// 所有错误响应都必须经过这里：前端严格按 error.code 分支，
// 任何一个裸 http.Error 都会给它一个解析不了的响应体。
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	de := asDomainError(err)
	status := statusFor(de.Code)

	// 5xx 记服务端日志并抹掉细节：内部错误的 message 可能含 DSN、SQL 片段。
	if status >= 500 {
		slog.Error("request failed",
			"method", r.Method, "path", r.URL.Path, "code", de.Code, "err", err)
		de = &domain.Error{
			Code:      domain.CodeInternal,
			Message:   "internal error",
			Retryable: true,
		}
	}
	writeJSON(w, status, domain.ErrorEnvelope{Error: *de})
}

// asDomainError 把任意 error 收敛成 *domain.Error。
//
// capability 的字段级校验错误在这里翻译成 invalid_param + field_errors，
// 这样"哪个芯片填错了"能一路带到前端，而不是塌成一句话。
func asDomainError(err error) *domain.Error {
	var de *domain.Error
	if errors.As(err, &de) {
		return de
	}
	var fe *capability.FieldErrors
	if errors.As(err, &fe) {
		return &domain.Error{
			Code:        domain.CodeInvalidParam,
			Message:     "参数校验未通过",
			FieldErrors: fe.Fields,
			Err:         err,
		}
	}
	return &domain.Error{Code: domain.CodeInternal, Message: err.Error(), Err: err}
}

// statusFor 是错误码到 HTTP 状态码的唯一映射表。
//
// 单独一张表而不是在各 handler 里现写状态码：同一个 code 在不同端点返回
// 不同状态码，前端就没法只按 code 分支了。
func statusFor(code domain.ErrorCode) int {
	switch code {
	case domain.CodeInvalidParam:
		return http.StatusBadRequest
	case domain.CodeUnauthorized:
		return http.StatusUnauthorized
	case domain.CodeForbidden:
		return http.StatusForbidden
	case domain.CodeNotFound:
		return http.StatusNotFound
	case domain.CodeConflict:
		return http.StatusConflict
	case domain.CodeRevisionConflict:
		return http.StatusConflict
	case domain.CodeInsufficientCredit:
		return http.StatusPaymentRequired
	case domain.CodeQuotaExceeded:
		return http.StatusInsufficientStorage
	case domain.CodeRateLimited, domain.CodeUpstreamRateLimited:
		return http.StatusTooManyRequests
	case domain.CodeContentRejected:
		// 内容被拒是**请求本身**不被接受，不是服务端故障。
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

// 构造常见错误的快捷方式。散落各处手写 &domain.Error{...} 迟早会漏掉字段。
func errInvalid(format string, a ...any) *domain.Error {
	return &domain.Error{Code: domain.CodeInvalidParam, Message: fmt.Sprintf(format, a...)}
}

func errFields(fields []domain.FieldError, format string, a ...any) *domain.Error {
	return &domain.Error{Code: domain.CodeInvalidParam, Message: fmt.Sprintf(format, a...), FieldErrors: fields}
}

func errUnauthorized(msg string) *domain.Error {
	return &domain.Error{Code: domain.CodeUnauthorized, Message: msg}
}

func errForbidden(msg string) *domain.Error {
	return &domain.Error{Code: domain.CodeForbidden, Message: msg}
}

func errNotFound(what string) *domain.Error {
	return &domain.Error{Code: domain.CodeNotFound, Message: what + " 不存在"}
}

func errConflict(format string, a ...any) *domain.Error {
	return &domain.Error{Code: domain.CodeConflict, Message: fmt.Sprintf(format, a...)}
}

func errInternal(err error) *domain.Error {
	return &domain.Error{Code: domain.CodeInternal, Message: "internal error", Retryable: true, Err: err}
}

// decodeJSON 读入并解码请求体，带体积上限与未知字段拒绝。
//
// DisallowUnknownFields 是故意的：前端多传一个字段通常意味着契约漂了，
// 静默忽略会让这种漂移一直藏到某个字段"怎么没生效"的排查现场。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errInvalid("请求体为空")
		}
		return errInvalid("请求体解析失败: %v", err)
	}
	return nil
}

// pagination 读取 cursor / limit 两个查询参数。
func pagination(r *http.Request) (cursor string, limit int) {
	cursor = r.URL.Query().Get("cursor")
	limit = DefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	return cursor, limit
}

// identity 取出中间件放进 context 的身份。走到 handler 里它必然存在，
// 缺失只可能是路由挂错了组，因此当成内部错误而不是 401。
func identity(r *http.Request) (Identity, error) {
	id, ok := IdentityFrom(r.Context())
	if !ok {
		return Identity{}, errInternal(errors.New("httpapi: handler is not behind the auth middleware"))
	}
	return id, nil
}

// bearerToken 从 Authorization 头取 token。
//
// **只认头，不认 query。** query 里的 token 会进访问日志、进 Referer、
// 进浏览器历史；SSE 也一样走头，这正是前端要用 fetch 而非 EventSource 的原因。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}
