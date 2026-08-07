package openaivideo

import (
	"errors"
	"fmt"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// 上游状态字符串。
const (
	statusQueued     = "queued"
	statusInProgress = "in_progress"
	statusCompleted  = "completed"
	statusFailed     = "failed"
)

// Normalize 把上游状态串归一化成平台状态。
//
// in_progress → running 是本表存在的最直白的理由：语义完全一致，
// 只是叫法不同，而这类纯字符串差异一旦泄漏到 L2，
// 那里就得写 `if raw == "in_progress" || raw == "running"`。
func Normalize(raw string) adapter.Status {
	if s, ok := statusMap[strings.ToLower(strings.TrimSpace(raw))]; ok {
		return s
	}
	return adapter.StatusRunning
}

// videoResponse 是提交与轮询共用的响应体形状（这个协议两处返回同一个对象）。
type videoResponse struct {
	ID        string    `json:"id"`
	Object    string    `json:"object"`
	Model     string    `json:"model"`
	Status    string    `json:"status"`
	Progress  int       `json:"progress"`
	Size      string    `json:"size"`
	Seconds   float64   `json:"seconds"`
	CreatedAt int64     `json:"created_at"`
	ExpiresAt int64     `json:"expires_at"`
	Error     *apiError `json:"error"`
}

// apiError 是 OpenAI 的错误体形状。
type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Param   string `json:"param"`
}

// 上游错误码到平台失败三分类的映射。
//
// 审核拒绝在这套协议里同样是 400，光看 HTTP 状态码分不出它与参数错误——
// 而这两者在前端是完全不同的两种呈现（灰色引导改提示词 vs 红色高亮芯片）。
var errorCodeMap = map[string]domain.TaskErrorCode{
	// 审核拒绝。
	"moderation_blocked":       domain.TaskErrorContentRejected,
	"content_policy_violation": domain.TaskErrorContentRejected,
	"input_moderation":         domain.TaskErrorContentRejected,
	"output_moderation":        domain.TaskErrorContentRejected,

	// 限流与配额。
	"rate_limit_exceeded":        domain.TaskErrorUpstreamRateLimited,
	"insufficient_quota":         domain.TaskErrorUpstreamRateLimited,
	"billing_hard_limit_reached": domain.TaskErrorUpstreamRateLimited,
	"server_overloaded":          domain.TaskErrorUpstreamRateLimited,

	// 参数错误。
	"invalid_request_error":   domain.TaskErrorInvalidParam,
	"invalid_prompt":          domain.TaskErrorInvalidParam,
	"unsupported_size":        domain.TaskErrorInvalidParam,
	"unsupported_duration":    domain.TaskErrorInvalidParam,
	"invalid_input_reference": domain.TaskErrorInvalidParam,

	// 服务端错误。
	"internal_error":    domain.TaskErrorInternal,
	"server_error":      domain.TaskErrorInternal,
	"generation_failed": domain.TaskErrorInternal,
}

func classify(status int, e apiError) domain.TaskErrorCode {
	for _, key := range []string{
		strings.ToLower(strings.TrimSpace(e.Code)),
		strings.ToLower(strings.TrimSpace(e.Type)),
	} {
		if key == "" {
			continue
		}
		if code, ok := errorCodeMap[key]; ok {
			return code
		}
	}
	msg := strings.ToLower(e.Message)
	switch {
	case containsAny(msg, "moderation", "content policy", "safety", "flagged"):
		return domain.TaskErrorContentRejected
	case containsAny(msg, "rate limit", "quota", "overloaded", "too many requests"):
		return domain.TaskErrorUpstreamRateLimited
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
	if e.Param != "" {
		// 上游字段名只进消息文本，不当 FieldError.Key——
		// 前端按 ParamSpec.Key 找芯片，上游字段名在那张表里查不到，
		// 硬塞进去会让前端渲染出一枚指向不存在参数的高亮。
		msg += " (上游字段: " + e.Param + ")"
	}
	return adapter.CallError(classify(status, e), nil, "%s", msg)
}

// failureFor 把终态失败的轮询响应变成归一化后的 domain.TaskError。
func failureFor(resp videoResponse) *domain.TaskError {
	if resp.Error != nil {
		e := upstreamError(0, *resp.Error)
		return adapter.Failure(domain.TaskErrorCode(e.Code), "%s", e.Message)
	}
	return adapter.Failure(domain.TaskErrorInternal, "上游任务失败但未给出原因")
}

// reclassify 在 httpx 只按状态码分过一次类之后，再按本协议的错误码文案纠正。
func reclassify(err error) error {
	var de *domain.Error
	if !errors.As(err, &de) {
		return err
	}
	msg := strings.ToLower(de.Message)
	switch {
	case containsAny(msg, "moderation_blocked", "content_policy_violation", "moderation", "content policy"):
		return adapter.CallError(domain.TaskErrorContentRejected, de.Err, "%s", de.Message)
	case containsAny(msg, "rate_limit_exceeded", "insufficient_quota", "rate limit", "quota"):
		return adapter.CallError(domain.TaskErrorUpstreamRateLimited, de.Err, "%s", de.Message)
	}
	return err
}

// parseSize 解析 "1280x720" 形态的尺寸串。
func parseSize(size string) (int, int, bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(size)), "x")
	if len(parts) != 2 {
		return 0, 0, false
	}
	var w, h int
	if _, err := fmt.Sscanf(parts[0], "%d", &w); err != nil || w <= 0 {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &h); err != nil || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}
