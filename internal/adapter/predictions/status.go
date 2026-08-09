package predictions

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// 上游状态字符串。写成常量而不是散落的字面量，是为了让 statusMap 与
// 判定分支引用同一份取值——两处各写各的字面量，改一个漏一个不会有编译错误。
const (
	statusStarting   = "starting"
	statusProcessing = "processing"
	statusSucceeded  = "succeeded"
	statusFailed     = "failed"
	statusCanceled   = "canceled"
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

// prediction 是提交与轮询共用的响应体——这个协议两个端点回的是同一个对象。
//
// Output 与 Error 都用 json.RawMessage 接：上游在这两个字段上**类型不固定**
// （见 ParseOutput / errorMessage），提前定死类型会让一半的响应解不开。
//
// Input 刻意不解：轮询响应里的 input 是上游回填过的，与我们发出去的那份不等
// （实测多出 input_has_video），拿它做任何断言都会错。
type prediction struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Output json.RawMessage `json:"output"`
	Error  json.RawMessage `json:"error"`

	Metrics struct {
		PredictTime float64 `json:"predict_time"`
	} `json:"metrics"`
}

// ParseOutput 把 output 字段解成产物地址列表。
//
// **这是本驱动最容易埋 panic 的地方**：实测图像回字符串数组 ["url"]，
// 视频回单个字符串 "url"。两种都得吃。假定其一——无论假定哪一个——
// 都会在另一种模态上炸，而且是在任务已经成功、已经计过费之后炸。
//
// 导出是为了让单测能直接对着这两种形态对拍。
func ParseOutput(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if strings.TrimSpace(single) == "" {
			return nil, nil
		}
		return []string{single}, nil
	}

	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		out := make([]string, 0, len(many))
		for _, u := range many {
			if strings.TrimSpace(u) != "" {
				out = append(out, u)
			}
		}
		return out, nil
	}

	return nil, fmt.Errorf("output 既不是字符串也不是字符串数组: %s", snippet(raw))
}

// errorMessage 把 error 字段抽成一句话。
//
// 上游在这个字段上同样不守一种形态：正常时是 null，Replicate 风格是一个
// 字符串，而 GPUGeek 网关自己的错误是一个对象。三种都认，认不出就把原文
// 交出去——排障时一段能看的原文比一句「未知错误」有用得多。
func errorMessage(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return strings.TrimSpace(single)
	}

	var obj struct {
		Message string `json:"message"`
		Detail  string `json:"detail"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, s := range []string{obj.Message, obj.Detail, obj.Code} {
			if strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return snippet(raw)
}

func snippet(raw []byte) string {
	const limit = 512
	s := strings.TrimSpace(string(raw))
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}

// classify 按错误文案把失败归到平台的三分类。
//
// 这个协议**不给结构化错误码**——error 字段是一段人话。因此只能按关键词分。
// 不能只看 HTTP 状态码：审核拒绝与参数错误在网关那里都是 400，
// 只看状态码会把审核拒绝报成参数错误，前端就会去高亮一枚没有问题的芯片，
// 而用户真正该改的是提示词。
// 认不出线索时回 TaskErrorInternal，调用方据此判断"这段文案没给出信号"。
func classify(msg string) domain.TaskErrorCode {
	lower := strings.ToLower(msg)
	switch {
	case containsAny(lower, "sensitive", "moderation", "risk control", "审核", "违规", "敏感", "风控"):
		return domain.TaskErrorContentRejected
	case containsAny(lower, "rate limit", "too many requests", "quota", "overload", "insufficient balance",
		"限流", "配额", "并发", "余额不足", "欠费"):
		return domain.TaskErrorUpstreamRateLimited
	case containsAny(lower, "invalid", "missing", "not found", "unsupported", "must be", "无权限", "不存在", "参数"):
		return domain.TaskErrorInvalidParam
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

// failureFor 把一个终态失败的响应变成归一化后的 domain.TaskError。
func failureFor(resp prediction) *domain.TaskError {
	msg := errorMessage(resp.Error)
	if msg == "" {
		return adapter.Failure(domain.TaskErrorInternal, "上游任务失败但未给出原因")
	}
	return adapter.Failure(classify(msg), "%s", msg)
}

// reclassify 在 httpx 只按状态码分过一次类之后，再按错误文案纠正。
//
// httpx 是通用兜底，只认状态码；而正是文案决定了失败三分类能不能分对。
func reclassify(err error) error {
	var de *domain.Error
	if !errors.As(err, &de) {
		return err
	}
	code := classify(de.Message)
	if code == domain.TaskErrorInternal {
		// 文案里认不出线索就不动它，保留 httpx 按状态码给出的分类。
		return err
	}
	return adapter.CallError(code, de.Err, "%s", de.Message)
}

// imageMIME 从产物直链的路径后缀猜内容类型。
//
// 猜错的代价很小：转存时 asset 会优先用响应头里的 Content-Type，
// 这里给的只是响应头缺失时的兜底。实测 Seedream 出的是 jpeg。
func imageMIME(raw string) string {
	ext := ""
	if u, err := url.Parse(raw); err == nil {
		ext = strings.ToLower(path.Ext(u.Path))
	}
	switch ext {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	}
	return "image/jpeg"
}
