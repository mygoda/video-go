package predictions

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"regexp"
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

// moderationNeedles 是判定"这是一次内容审核拒绝"的关键词表。
//
// 这个协议**不给结构化错误码**，上游把审核结论写成一句人话，因此措辞会变——
// 只钉死某一串（例如 DEM-100 实测的 real person）下次换个说法又会漏。
// 按语义族收词，但收词有代价：本表在 classify 里排在 invalid 之前，
// 一次误命中会把真正的参数错误判成 content_rejected（Retryable=false），
// 用户被告知"改内容"，而他该改的是一个参数。因此宁可漏收也不错收。
//
// 收进来的词，共同点是**离开审核语境就不成句**：
//   - real person / real people / 真人：DEM-100 实测的原话，写实真人首帧被拒。
//     没有任何参数校验会说"真人"。
//   - content policy / usage policy / policy violation：各家网关表达"违反内容
//     政策"的标准说法，两词连用，不会与参数错误撞。
//   - content filter / nsfw / prohibited：只出现在内容安全语境。
//   - blocked by：其后接的一定是拦截方（content filter、safety system）。
//
// 刻意**没有**收进来的词，以及理由：
//   - 裸 policy：retry policy / bucket policy / IAM policy 都是正经技术名词，
//     一次误伤就把"重试策略配置错误"报成"你的内容违规"。
//   - 裸 not allowed：参数错误里高频——"value not allowed"、"duration not
//     allowed for this model" 说的都是参数，收它等于主动制造误伤。
//   - 裸 safety：Flux 系有个正经参数就叫 safety_tolerance，
//     "safety_tolerance must be in range [0, 6]" 是标准参数错误。
//   - 裸 blocked：account blocked / IP blocked 讲的是账号与配额，
//     归到审核会让排查的人去查用户的提示词，而该查的是账号状态。
//
// 这四个词单独看覆盖面更大，但它们的误伤是"把一个可修复的参数问题说成
// 内容违规"，比漏收一条审核拒绝更难被发现，也更难被用户自己纠正。
var moderationNeedles = []string{
	"sensitive", "moderation", "risk control",
	"real person", "real people",
	"content policy", "usage policy", "policy violation",
	"content filter", "nsfw", "prohibited",
	"blocked by",
	"审核", "违规", "敏感", "风控", "真人",
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
	case containsAny(lower, moderationNeedles...):
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

// requestIDPattern 从上游错误体里抠出追踪号。
// 上游在这个键上没统一大小写与下划线（实测 requestId），三种写法都认。
var requestIDPattern = regexp.MustCompile(`(?i)"request[_-]?id"\s*:\s*"([^"]{1,128})"`)

// moderationMessage 把一次审核拒绝翻成用户能看懂的一句中文。
//
// 上游给的是整段英文 JSON，而这个字符串会原样落进 tasks.error_message 再出到
// API——用户看到的就是它。直接透传有两处害：一是他看不懂，二是里面那句
// "The request failed" 会被理解成"系统抽风了，再点一次"，而实测（DEM-100）
// 同一张图重放照样被拒，再点一次只是又输一次。因此这里必须说清楚
// **是什么被拒了**，并且明确"重提同样的内容没有意义"。
//
// requestId 保留在用户可见文案里：它不是密钥，是这次拒绝在上游唯一的追溯
// 句柄，客服与排障全靠用户把它念出来。英文原文只进日志——排障要的是原文，
// 用户要的是能读懂的话，两者不是同一个受众。
func moderationMessage(raw string) string {
	id := ""
	if m := requestIDPattern.FindStringSubmatch(raw); m != nil {
		id = strings.TrimSpace(m[1])
	}
	slog.Warn("上游内容审核拒绝，原文只进日志不进用户文案",
		"driver", Name, "request_id", id, "upstream_message", raw)

	lower := strings.ToLower(raw)
	subject := "输入内容未通过上游的内容审核"
	switch {
	case containsAny(lower, "real person", "real people", "真人"):
		subject = "上游判定输入图片里出现了可辨认的真人，拒绝生成"
	case containsAny(lower, "image", "图片", "图像"):
		subject = "上游判定输入图片未通过内容审核"
	case containsAny(lower, "text", "prompt", "提示词", "文本"):
		subject = "上游判定提示词未通过内容审核"
	}

	msg := "内容审核未通过：" + subject +
		"。重复提交同样的内容仍会被拒，请更换输入素材或改写提示词后重新发起。"
	if id != "" {
		msg += "（上游追踪号 " + id + "）"
	}
	return msg
}

// failureFor 把一个终态失败的响应变成归一化后的 domain.TaskError。
func failureFor(resp prediction) *domain.TaskError {
	msg := errorMessage(resp.Error)
	if msg == "" {
		return adapter.Failure(domain.TaskErrorInternal, "上游任务失败但未给出原因")
	}
	code := classify(msg)
	if code == domain.TaskErrorContentRejected {
		return adapter.Failure(code, "%s", moderationMessage(msg))
	}
	return adapter.Failure(code, "%s", msg)
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
	msg := de.Message
	if code == domain.TaskErrorContentRejected {
		msg = moderationMessage(de.Message)
	}
	return adapter.CallError(code, de.Err, "%s", msg)
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
