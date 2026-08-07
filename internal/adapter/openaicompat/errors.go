package openaicompat

import (
	"errors"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// response 是两个端点的响应体的并集。
//
// 合成一个结构体而不是两个，是因为二者的 error 外壳完全一致，
// 分开写会让错误分类那段代码复制两份。用不到的字段解出来是 nil，无害。
type response struct {
	// images 端点。
	Data         []imageItem `json:"data"`
	Size         string      `json:"size"`
	OutputFormat string      `json:"output_format"`

	// chat 端点。
	Choices []choice `json:"choices"`

	// 两个端点共用的错误外壳。有的兼容实现回 200 但在这里带 error。
	Error *apiError `json:"error"`
}

type imageItem struct {
	URL           string `json:"url"`
	B64JSON       string `json:"b64_json"`
	RevisedPrompt string `json:"revised_prompt"`
}

type choice struct {
	Index   int `json:"index"`
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

// apiError 是 OpenAI 错误体的形状：{"error":{"message":..,"type":..,"code":..,"param":..}}。
type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Param   string `json:"param"`
}

// 上游错误码里能确定归到"内容被拒"的那些。
//
// **这一步不能只看 HTTP 状态码**：审核拒绝在这套协议里也是 400，
// 光看状态码会把它误判成 invalid_param，前端就会去高亮一枚根本没错的参数芯片，
// 并给出"改完重提"的引导——而实际上用户该改的是提示词。
var contentRejectedCodes = map[string]bool{
	"content_policy_violation": true,
	"moderation_blocked":       true,
	"content_filter":           true,
}

// 上游错误码里能确定归到"限流/配额"的那些。
var rateLimitedCodes = map[string]bool{
	"rate_limit_exceeded":        true,
	"insufficient_quota":         true,
	"quota_exceeded":             true,
	"billing_hard_limit_reached": true,
	"engine_overloaded":          true,
	"server_overloaded":          true,
}

// 上游错误码里能确定归到"参数错误"的那些。
var invalidParamCodes = map[string]bool{
	"invalid_request_error":       true,
	"invalid_parameter":           true,
	"invalid_value":               true,
	"unknown_parameter":           true,
	"image_generation_user_error": true,
	"string_above_max_length":     true,
}

// classify 把上游错误体归到平台的失败三分类。
//
// 优先级：显式错误码 > 错误类型 > 文案关键词 > HTTP 状态码。
// 越往后越不可靠，文案匹配尤其脆（上游改一版文案就失效），
// 因此它只是兜底而不是主判据——但没有它，很多兼容实现（它们不填 code）
// 的审核拒绝会全部落进 internal_error 并被反复重试。
func classify(status int, e apiError) domain.TaskErrorCode {
	code := strings.ToLower(strings.TrimSpace(e.Code))
	typ := strings.ToLower(strings.TrimSpace(e.Type))

	for _, key := range []string{code, typ} {
		if key == "" {
			continue
		}
		if contentRejectedCodes[key] {
			return domain.TaskErrorContentRejected
		}
		if rateLimitedCodes[key] {
			return domain.TaskErrorUpstreamRateLimited
		}
		if invalidParamCodes[key] {
			return domain.TaskErrorInvalidParam
		}
	}

	msg := strings.ToLower(e.Message)
	switch {
	case containsAny(msg, "content policy", "safety system", "moderation", "flagged", "内容审核", "违规"):
		return domain.TaskErrorContentRejected
	case containsAny(msg, "rate limit", "too many requests", "quota", "overloaded", "限流", "配额"):
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

// upstreamError 把一个上游错误体变成归一化后的调用错误。
//
// e.Param 是**上游的**字段名，直接当作 FieldError.Key 回给前端是错的——
// 前端按 ParamSpec.Key 找芯片，上游字段名在那张表里不存在。
// 因此这里只把它写进消息文本，不造 FieldError:宁可不高亮，
// 也不能高亮到一枚不存在的芯片上（那会让前端渲染出一个空壳）。
func upstreamError(status int, e apiError) *domain.Error {
	code := classify(status, e)
	msg := e.Message
	if msg == "" {
		msg = "上游未给出错误描述"
	}
	if e.Param != "" {
		msg += " (上游字段: " + e.Param + ")"
	}
	return adapter.CallError(code, nil, "%s", msg)
}

// reclassify 在 httpx 只按状态码分过一次类之后，再看一眼响应体里的错误码。
//
// httpx 是通用兜底，它不认识 content_policy_violation 这类协议特有的码；
// 而这些码正是失败三分类能不能分对的关键。因此非 2xx 的响应体在这里
// 被再解一次。解不出来就保留 httpx 给的分类。
func reclassify(err error) error {
	var de *domain.Error
	if !errors.As(err, &de) {
		return err
	}
	// httpx.StatusError 把响应体片段拼进了 Message。这里不去反解那段文本，
	// 而是直接对文案做一次关键词判定——比正则抠 JSON 更耐上游改结构。
	msg := strings.ToLower(de.Message)
	switch {
	case containsAny(msg, "content_policy_violation", "moderation_blocked", "content policy", "safety system", "内容审核"):
		return adapter.CallError(domain.TaskErrorContentRejected, de.Err, "%s", de.Message)
	case containsAny(msg, "rate_limit_exceeded", "insufficient_quota", "rate limit", "quota"):
		return adapter.CallError(domain.TaskErrorUpstreamRateLimited, de.Err, "%s", de.Message)
	}
	return err
}
