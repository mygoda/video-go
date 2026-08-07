package ark

import (
	"errors"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// 上游状态字符串。写成常量而不是散落的字面量，是为了让 statusMap 与
// 判定分支引用同一份取值——两处各写各的字面量，改一个漏一个不会有编译错误。
const (
	statusQueued    = "queued"
	statusRunning   = "running"
	statusSucceeded = "succeeded"
	statusFailed    = "failed"
	statusExpired   = "expired"
)

// Normalize 把上游状态串归一化成平台状态。
//
// 导出是为了让状态归一化能被单测逐值对拍——这张表是本驱动最容易在
// 上游改版时悄悄失准的地方，没有测试覆盖它等于没有归一化。
func Normalize(raw string) adapter.Status {
	if s, ok := statusMap[strings.ToLower(strings.TrimSpace(raw))]; ok {
		return s
	}
	// 未知状态按 running 处理：上游随时可能加新状态，
	// 把在途任务判死比多轮询几圈代价大得多。
	return adapter.StatusRunning
}

type submitResponse struct {
	ID     string    `json:"id"`
	Status string    `json:"status"`
	Error  *apiError `json:"error"`
}

type pollResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Status  string `json:"status"`
	Content struct {
		VideoURL string `json:"video_url"`
	} `json:"content"`
	Usage struct {
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error     *apiError `json:"error"`
	CreatedAt int64     `json:"created_at"`
	UpdatedAt int64     `json:"updated_at"`
}

// apiError 是 Ark 的错误体形状：{"error":{"code":"...","message":"..."}}。
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// 上游错误码到平台失败三分类的映射。
//
// **不能只看 HTTP 状态码**：SensitiveContentDetected 与 InvalidParameter
// 都是 400，只看状态码会把审核拒绝误判成参数错误，前端就会去高亮一枚
// 没有问题的芯片并引导"改完重提"，而用户真正该改的是提示词。
var errorCodeMap = map[string]domain.TaskErrorCode{
	// 审核拒绝。
	"SensitiveContentDetected":   domain.TaskErrorContentRejected,
	"OutputVideoSensitive":       domain.TaskErrorContentRejected,
	"InputTextSensitiveContent":  domain.TaskErrorContentRejected,
	"InputImageSensitiveContent": domain.TaskErrorContentRejected,
	"PostImgRiskNotPass":         domain.TaskErrorContentRejected,

	// 限流与配额。
	"RateLimitExceeded":       domain.TaskErrorUpstreamRateLimited,
	"QuotaExceeded":           domain.TaskErrorUpstreamRateLimited,
	"AccountOverdueError":     domain.TaskErrorUpstreamRateLimited,
	"ServerOverloaded":        domain.TaskErrorUpstreamRateLimited,
	"ModelLoadingError":       domain.TaskErrorUpstreamRateLimited,
	"ConcurrencyLimitReached": domain.TaskErrorUpstreamRateLimited,

	// 参数错误。
	"InvalidParameter":          domain.TaskErrorInvalidParam,
	"MissingParameter":          domain.TaskErrorInvalidParam,
	"InvalidRequest":            domain.TaskErrorInvalidParam,
	"InvalidEndpointOrModel":    domain.TaskErrorInvalidParam,
	"ModelNotOpen":              domain.TaskErrorInvalidParam,
	"InputImageSizeExceed":      domain.TaskErrorInvalidParam,
	"InputImageResolutionError": domain.TaskErrorInvalidParam,
	"UrlFetchFailed":            domain.TaskErrorInvalidParam,
	"InputImageFormatError":     domain.TaskErrorInvalidParam,

	// 服务端错误。
	"InternalServiceError": domain.TaskErrorInternal,
	"ServiceUnavailable":   domain.TaskErrorInternal,
}

func classify(status int, e apiError) domain.TaskErrorCode {
	if code, ok := errorCodeMap[strings.TrimSpace(e.Code)]; ok {
		return code
	}
	// 上游偶尔只给文案不给码（尤其是网关层的错误），做一次关键词兜底。
	msg := strings.ToLower(e.Message)
	switch {
	case containsAny(msg, "sensitive", "risk", "审核", "违规", "敏感"):
		return domain.TaskErrorContentRejected
	case containsAny(msg, "rate limit", "quota", "overload", "限流", "配额", "并发"):
		return domain.TaskErrorUpstreamRateLimited
	case containsAny(msg, "invalid", "missing", "参数"):
		return domain.TaskErrorInvalidParam
	}
	if status != 0 {
		return adapter.ClassifyHTTPStatus(status)
	}
	return domain.TaskErrorInternal
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func upstreamError(status int, e apiError) *domain.Error {
	msg := e.Message
	if msg == "" {
		msg = "上游未给出错误描述"
	}
	return adapter.CallError(classify(status, e), nil, "%s", msg)
}

// failureFor 把一个终态失败的轮询响应变成归一化后的 domain.TaskError。
//
// expired 单独处理：它不是上游拒绝了什么，而是**我们轮询得太晚**，
// 上游任务记录只留 7 天、产物直链只有 24h。归到 internal_error 且可重试，
// 因为责任在平台不在用户，重跑一次就好；照例不扣费。
func failureFor(raw string, resp pollResponse) *domain.TaskError {
	if strings.EqualFold(strings.TrimSpace(raw), statusExpired) {
		return adapter.Failure(domain.TaskErrorInternal,
			"上游任务已过期（产物直链有效期 %s），轮询与转存没能跑在过期之前", ArtifactTTL)
	}
	if resp.Error != nil {
		e := upstreamError(0, *resp.Error)
		return adapter.Failure(domain.TaskErrorCode(e.Code), "%s", e.Message)
	}
	return adapter.Failure(domain.TaskErrorInternal, "上游任务失败但未给出原因")
}

// reclassify 在 httpx 只按状态码分过一次类之后，再按 Ark 的错误码文案纠正。
//
// httpx 是通用兜底，认不出 SensitiveContentDetected 这类协议特有的码——
// 而正是这些码决定了失败三分类能不能分对。
func reclassify(err error) error {
	var de *domain.Error
	if !errors.As(err, &de) {
		return err
	}
	msg := de.Message
	lower := strings.ToLower(msg)
	switch {
	case containsAny(msg, "SensitiveContentDetected", "OutputVideoSensitive", "InputTextSensitiveContent") ||
		containsAny(lower, "sensitive", "审核", "违规"):
		return adapter.CallError(domain.TaskErrorContentRejected, de.Err, "%s", msg)
	case containsAny(msg, "RateLimitExceeded", "QuotaExceeded", "AccountOverdueError", "ServerOverloaded") ||
		containsAny(lower, "rate limit", "quota", "限流", "配额"):
		return adapter.CallError(domain.TaskErrorUpstreamRateLimited, de.Err, "%s", msg)
	case containsAny(msg, "InvalidParameter", "MissingParameter", "InvalidEndpointOrModel"):
		return adapter.CallError(domain.TaskErrorInvalidParam, de.Err, "%s", msg)
	}
	return err
}
