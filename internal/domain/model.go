package domain

import "time"

// Modality 决定模型归入前端哪个 tab。tab 结构固定为 image / video 两个。
//
// ModalityText 没有对应的 tab，这是刻意的：文本模型（chat 族）不是用户能
// 在创作页选的产品，它们是分镜拆解这类平台内部能力的载体。给它一个真实的
// 取值而不是塞进 image，是因为塞错的代价是**任务绿灯、产物是废的**——
// 前端拿文本当图片渲染就是裂图，而任务状态显示成功，骗过所有自动检查。
type Modality string

const (
	ModalityImage Modality = "image"
	ModalityVideo Modality = "video"
	ModalityText  Modality = "text"
)

// ModelVisibility 是模型在**用户端目录**里的可见性，与 Enabled 是两回事。
//
// Enabled=false 是"这个模型停用了"，谁都用不了，包括平台自己；
// VisibilityInternal 是"能用，但不摆进用户的模型下拉里"。mock 驱动、QA 探针、
// 画布内部调用的合成模型都属于后者：它们必须继续可用（QA 要拿它们跑回归，
// 画布一键合成要拿它出片），但绝不能让用户在创作页点到。
//
// 少了这个维度就只剩 enabled 一个开关，于是"从用户端摘掉夹具"和"让 QA 用不了
// 夹具"变成同一件事——2026-08-09 用户端暴露 qa-fail-probe（一个设计上必失败的
// 探针）正是这么来的。
type ModelVisibility string

const (
	VisibilityPublic   ModelVisibility = "public"
	VisibilityInternal ModelVisibility = "internal"
)

// ProtocolFamily 是协议族。
//
// chat / images 统一走 OpenAI 协议（同步返回）；video 内部再按 VideoProtocol
// 分驱动（异步提交-轮询）；mock 是本地开发用的假驱动，走**完全相同**的
// interface / 状态机 / 转存 / 记账路径，只替换最底层的 HTTP 调用。
//
// compose 是本地合成族：它不打任何上游，把一组已有产物按顺序编成一份清单。
// 之所以仍然做成一个协议族而不是在 httpapi 里就地拼一份 JSON，是因为
// 「一键合成」要的是一条**真任务**——任务表、状态机、退款、SSE、任务监控
// 一样不能少，而那条链路的入口只有 driver 一个。等到真正能拼视频的上游
// （或本地转码服务）接进来时，换掉的只是这一个 driver。
//
// predictions 是模型市场族（Replicate 风格 POST /predictions）的**同步**一侧，
// 出图走它。同一个协议的异步一侧挂在 VideoProtocolPredictions 上，出视频走那边——
// 两侧共用 "predictions" 这一个查找键，靠 Family() 与本列对齐来选边（见 adapter.IsAsync）。
type ProtocolFamily string

const (
	FamilyChat        ProtocolFamily = "chat"
	FamilyImages      ProtocolFamily = "images"
	FamilyVideo       ProtocolFamily = "video"
	FamilyMock        ProtocolFamily = "mock"
	FamilyCompose     ProtocolFamily = "compose"
	FamilyPredictions ProtocolFamily = "predictions"
)

// VideoProtocol 是视频族的具体协议。
//
// 四家供应商在**提交端点、轮询寻址、状态枚举、产物取法**四个维度上全不同，
// 因此不能用一套 URL 模板硬套——这就是 L0 driver 必须按协议分包的原因。
// 差异的具体内容见各 driver 包的包注释。
type VideoProtocol string

const (
	VideoProtocolArk         VideoProtocol = "ark"
	VideoProtocolOpenAIVideo VideoProtocol = "openai_video"
	VideoProtocolGoogleLRO   VideoProtocol = "google_lro"
	VideoProtocolMock        VideoProtocol = "mock"
	VideoProtocolPredictions VideoProtocol = "predictions"
)

// AuthStyle 是凭证注入上游请求的方式。Ark 与 OpenAI 一致都是 bearer，
// 因此同一套凭证结构可以复用。
type AuthStyle string

const (
	AuthStyleBearer AuthStyle = "bearer"
	AuthStyleHeader AuthStyle = "header"
	AuthStyleQuery  AuthStyle = "query"
)

// Provider 是一个上游供应商的接入配置。
//
// CredentialRef 存的是**环境变量名**（例 AIGC_PROVIDER_ARK_KEY），不是密钥本身。
// 密钥只从 env 读，不入库、不进代码、不进样例。对外只回 CredentialPresent 布尔，
// 永不回显密钥。
//
// MaxConcurrency 是该供应商的全局并发闸门，是故障隔离的一部分：
// 一家挂掉时它的 worker 被闸门挡住，不会把其他供应商的任务一起拖死。
type Provider struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	BaseURL        string    `json:"base_url"`
	CredentialRef  string    `json:"credential_ref"`
	AuthStyle      AuthStyle `json:"auth_style,omitempty"`
	AuthHeaderName string    `json:"auth_header_name,omitempty"`
	Enabled        bool      `json:"enabled"`
	TimeoutMS      int       `json:"timeout_ms,omitempty"`
	MaxConcurrency int       `json:"max_concurrency,omitempty"`

	// CredentialPresent 只读，表示 CredentialRef 指向的 env 变量当前是否已配置。
	CredentialPresent bool      `json:"credential_present"`
	CreatedAt         time.Time `json:"created_at"`
}

// ModelConfig 是一条记录 = 一个可用模型。**改配置不重启、不发版。**
//
// 这是管理后台存在的首要理由：接一个走已知协议的新模型 = 加一条这样的记录，
// 前端零改动、后端零发版。Capability 直接作为 GET /api/models 的元素下发前端；
// RequestMapping 承载平台参数到上游请求体的声明式翻译。
//
// Capability 与 RequestMapping 在 domain 层是 json.RawMessage 形态的 any：
// 它们的强类型定义分别在 internal/capability 与 internal/adapter/mapping，
// 让 domain 反过来依赖那两个包会造成依赖环，因此在这里保持结构化的 any，
// 由上层解码为具体类型。
type ModelConfig struct {
	ID            string         `json:"id"`
	ProviderID    string         `json:"provider_id"`
	UpstreamModel string         `json:"upstream_model"`
	Family        ProtocolFamily `json:"protocol_family"`
	// VideoProtocol 仅 Family == FamilyVideo 时必填。
	VideoProtocol *VideoProtocol `json:"video_protocol"`

	// Capability 的强类型是 capability.ModelCapabilitySchema。
	Capability any `json:"capability"`
	// RequestMapping 的强类型是 mapping.RequestMapping。
	RequestMapping any `json:"request_mapping,omitempty"`

	// PollIntervalSeconds 缺省时取 driver 的 DefaultPollInterval
	// （ark 12s / openai_video 15s 起指数退避）。
	PollIntervalSeconds *int `json:"poll_interval_seconds"`

	Enabled bool `json:"-"`
	// Visibility 缺省为空串，由仓储写库时补成 VisibilityPublic——
	// 让"没想过这件事"落到"用户看得见"上，是因为反过来（默认藏起来）会让
	// 管理员新加的模型静默不出现在用户端，那种错更难被发现。
	Visibility ModelVisibility `json:"visibility"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// ModelFilter 是模型配置查询条件。用户端只取 Enabled=true + Visibility=public，
// 管理端可取全量（含已禁用与内部模型）。
//
// Visibility 留空表示**不过滤**：分镜拆解、画布合成这类平台内部的模型查找
// 走的就是这条路径，它们要能找到内部模型。
type ModelFilter struct {
	Modality   Modality
	Enabled    *bool
	Visibility ModelVisibility
}

// BreakerSnapshot 是某供应商熔断器的对外可观测状态，
// 对应 GET /api/admin/circuit-breakers。State 的取值域见 executor.BreakerState。
type BreakerSnapshot struct {
	ProviderID   string     `json:"provider_id"`
	State        string     `json:"state"`
	FailureCount int        `json:"failure_count"`
	OpenedAt     *time.Time `json:"opened_at"`
	NextProbeAt  *time.Time `json:"next_probe_at"`
}
