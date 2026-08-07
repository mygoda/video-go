// Package googlelro implements the L0 driver for the Google long-running
// operation protocol (Veo).
//
// # 协议骨架
//
//	提交  POST {base_url}/v1beta/models/{model}:predictLongRunning
//	轮询  GET  {base_url}/{operation_name}
//
// # 三处与另外两家不同的地方
//
// 1) **轮询按 operation name 寻址，不是按 task id。**
//
// 提交响应返回的是一个 operation name（形如 models/xxx/operations/yyy，
// 一个完整的相对路径），轮询时把它直接拼在 base_url 后面。这意味着上层
// 不能假设"上游引用是个 id、拼进某个固定模板"——这正是 adapter.SubmitResult.UpstreamRef
// 被定义为**不透明字符串**、只由同一个 driver 解释的原因。
//
// 2) **没有状态枚举，只有一个 done 布尔。**
//
// 上游不给 queued / running / succeeded 这类字符串，只有 done: false 和
// done: true（true 时再看有没有 error 字段区分成功与失败）。因此本包的
// statusMap 的 key 是布尔的字符串形式，而不是上游状态名——它仍然存在，
// 是为了让"归一化关系有唯一出处"这件事在三个驱动里保持一致的形状。
//
// 3) **产物两种形态：base64 内联，或一个 gs:// URI。**
//
// 前者直接解码即可；后者要用 Google 凭证再去取一次。两种都落在
// adapter.ArtifactRef 的 KindBase64 / KindGCSURI 上。
package googlelro

import (
	"net/http"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/adapter/mapping"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Name 是 Registry 的查找键，与 domain.VideoProtocolGoogleLRO 一致。
const Name = string(domain.VideoProtocolGoogleLRO)

// Family 是本驱动所属的协议族。
const Family = domain.FamilyVideo

// 协议端点，相对 Provider.BaseURL。
const (
	// PathSubmitFormat 是提交端点模板，占位符为上游模型名。
	PathSubmitFormat = "/v1beta/models/%s:predictLongRunning"
	// PathPollFormat 的占位符是**提交响应返回的 operation name**，
	// 不是任务 id。operation name 本身已经是一个相对路径。
	PathPollFormat = "/%s"
)

// 响应体字段名。上游没有状态字符串，状态全靠 done 与 error 两个字段推出来。
const (
	// FieldOperationName 是提交响应里承载 operation name 的字段。
	FieldOperationName = "name"
	// FieldDone 是轮询响应里的完成标志，本协议唯一的状态信号。
	FieldDone = "done"
	// FieldError 在 done 为 true 时区分成功与失败。
	FieldError = "error"
	// FieldResponse 在 done 为 true 且无 error 时承载产物。
	FieldResponse = "response"
)

// GCSURIScheme 是 Google Cloud Storage URI 的前缀。
const GCSURIScheme = "gs://"

// DefaultPollInterval 是本协议的建议轮询间隔。
const DefaultPollInterval = 10 * time.Second

// statusMap 把 done 布尔（以字符串形式作为 key）映射到平台归一化状态。
//
// 本协议没有 queued / running 之分：done=false 一律视为 running。
// done=true 时还要看有没有 error 字段才能定成功还是失败，
// 那一步在 Poll 里做，不在本表内——本表只表达"done 这个信号本身"的含义。
var statusMap = map[string]adapter.Status{
	"false": adapter.StatusRunning,
	"true":  adapter.StatusSucceeded,
}

// Driver 是 Google LRO 协议的驱动实例。无状态，可被多个 provider 共用。
type Driver struct {
	// HTTP 是发起上游调用的客户端。
	HTTP *http.Client
	// Renderer 把 ModelConfig.RequestMapping 渲染成上游请求体。
	Renderer mapping.Renderer
	// PollInterval 覆盖 DefaultPollInterval，零值表示用默认。
	PollInterval time.Duration
}
