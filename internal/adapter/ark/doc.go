// Package ark implements the L0 driver for Volcengine Ark (Seedance / Doubao).
//
// # 协议骨架
//
//	提交  POST {base_url}/api/v3/contents/generations/tasks
//	轮询  GET  {base_url}/api/v3/contents/generations/tasks/{task_id}
//	鉴权  Authorization: Bearer <key>   —— 与 OpenAI 完全一致，凭证结构可复用
//
// 轮询按**提交响应里返回的 task id** 寻址（与 google_lro 按 operation name
// 寻址形成对照）。
//
// # 请求体
//
// 三段结构:
//
//	model       上游模型名，如 doubao-seedance-2-0-260128
//	content     多模态部分数组（text / image_url / video_url）
//	parameters  生成参数对象（分辨率、时长、比例等）
//
// **content 数组的顺序影响生成语义**——首帧图放在提示词之前还是之后，
// 上游的理解是不同的。因此 RequestMapping 里追加进 content[] 的规则顺序
// 必须被当作契约保持稳定，不能当成无所谓的实现细节随手调换。
//
// # 产物
//
// 成功时产物在 content.video_url，是一个直链。**这个 URL 只有 24 小时有效期。**
// 这正是 L3 必须在任务成功那一刻立刻转存的原因：一旦把它当长期地址存进库，
// 用户第二天回来看到的就是一片打不开的卡片。见 internal/asset.Transferor。
//
// # 状态
//
// 上游五个状态。expired 是 Ark 独有的：任务本身跑完了但产物已过期，
// 对平台而言等价于失败（拿不到产物就是没产物），因此归一化为 failed，
// 而不是硬造一个第六种平台状态出来。
package ark

import (
	"net/http"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/adapter/mapping"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Name 是 Registry 的查找键，与 domain.VideoProtocolArk 一致。
const Name = string(domain.VideoProtocolArk)

// Family 是本驱动所属的协议族。
const Family = domain.FamilyVideo

// 协议端点。{base} 由 Provider.BaseURL 提供，末尾不带斜杠。
const (
	// PathSubmit 是提交端点，相对 base_url。
	PathSubmit = "/api/v3/contents/generations/tasks"
	// PathPollFormat 是轮询端点模板，占位符为上游 task id。
	PathPollFormat = "/api/v3/contents/generations/tasks/%s"
)

// 请求体的顶层字段名。
const (
	FieldModel      = "model"
	FieldContent    = "content"
	FieldParameters = "parameters"
)

// ArtifactTTL 是 Ark 产物直链的有效期。产物必须在这个窗口内转存到本平台存储。
const ArtifactTTL = 24 * time.Hour

// DefaultPollInterval 是本协议的建议轮询间隔。
const DefaultPollInterval = 12 * time.Second

// statusMap 是上游状态到平台归一化状态的映射，是这份归一化关系的唯一出处。
//
// expired → failed: 任务跑完但产物已过期，平台拿不到产物，等价于失败。
var statusMap = map[string]adapter.Status{
	"queued":    adapter.StatusQueued,
	"running":   adapter.StatusRunning,
	"succeeded": adapter.StatusSucceeded,
	"failed":    adapter.StatusFailed,
	"expired":   adapter.StatusFailed,
}

// Driver 是 Ark 协议的驱动实例。
//
// 一个进程里只需要一个实例，它本身无状态：所有 per-provider 的东西
// （base_url、凭证、超时）都随 adapter.SubmitInput / PollRequest 传入，
// 因此同一个实例可以服务多个配置成 ark 协议的 provider。
type Driver struct {
	// HTTP 是发起上游调用的客户端。由外部注入，便于在测试里换成假 transport。
	HTTP *http.Client
	// Renderer 把 ModelConfig.RequestMapping 渲染成上游请求体。
	// driver 只管协议骨架，字段差异全在 mapping 里。
	Renderer mapping.Renderer
	// PollInterval 覆盖 DefaultPollInterval，零值表示用默认。
	PollInterval time.Duration
}
