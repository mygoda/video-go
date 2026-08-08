package domain

import "time"

// Modality 决定模型归入前端哪个 tab。tab 结构固定为 image / video 两个。
type Modality string

const (
	ModalityImage Modality = "image"
	ModalityVideo Modality = "video"
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
type ProtocolFamily string

const (
	FamilyChat    ProtocolFamily = "chat"
	FamilyImages  ProtocolFamily = "images"
	FamilyVideo   ProtocolFamily = "video"
	FamilyMock    ProtocolFamily = "mock"
	FamilyCompose ProtocolFamily = "compose"
)

// VideoProtocol 是视频族的具体协议。
//
// 三家供应商在**提交端点、轮询寻址、状态枚举、产物取法**四个维度上全不同，
// 因此不能用一套 URL 模板硬套——这就是 L0 driver 必须按协议分包的原因。
// 差异的具体内容见各 driver 包的包注释。
type VideoProtocol string

const (
	VideoProtocolArk         VideoProtocol = "ark"
	VideoProtocolOpenAIVideo VideoProtocol = "openai_video"
	VideoProtocolGoogleLRO   VideoProtocol = "google_lro"
	VideoProtocolMock        VideoProtocol = "mock"
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

	Enabled   bool      `json:"-"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ModelFilter 是模型配置查询条件。用户端只取 Enabled=true，
// 管理端可取全量（含已禁用）。
type ModelFilter struct {
	Modality Modality
	Enabled  *bool
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
