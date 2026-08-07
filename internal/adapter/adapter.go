// Package adapter is the L0 protocol-adaptation layer. It hides every
// difference between upstream providers behind one set of interfaces.
//
// # 红线
//
// **本层之上出现任何 `if provider == "..."` / `switch vendor` 都是设计失败。**
//
// L1（能力声明）、L2（执行）、L3（资产）、httpapi 都只认本包声明的
// Status / SubmitInput / PollResult / ArtifactRef。协议差异——端点长什么样、
// 轮询按 task id 还是按 operation name 寻址、状态枚举有几个值、产物是 URL
// 还是裸字节还是 base64——**只允许存在于各 driver 子包内部**。
// 一旦某个协议细节泄漏到 L0 之上，"接新供应商 = 新增一个 driver 包" 这条
// 验收线立刻失效，改成了 "接新供应商 = 全栈搜一遍 if"。
//
// # 同步与异步两条路
//
// chat / images 是同步的：一次 HTTP 往返就拿到结果（SyncDriver.Invoke）。
// video 是异步的：提交拿到一个引用，然后轮询到终态，再单独取产物
// （AsyncDriver.Submit / Poll / FetchArtifact）。硬把两者塞进一个接口，
// 要么让同步驱动实现三个空转方法，要么让异步驱动的 Invoke 阻塞几分钟——
// 因此分成两个接口，由 Registry 按 ProtocolFamily 分发。
//
// # 状态归一化
//
// 各家的状态字符串在 driver 内部映射到本包的 Status，L2 只认 Status。
// 归一化表在各 driver 的 statusMap 里，那是这份映射的唯一出处。
package adapter

import (
	"context"
	"io"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Status 是归一化后的上游任务状态。
//
// 它与 domain.TaskStatus 取值相同但**不是同一个类型**：Status 描述的是
// "上游那边现在什么情况"，domain.TaskStatus 描述的是 "平台这边这条任务什么情况"。
// 二者在 L2 里由状态机衔接——例如上游 succeeded 时，平台任务要等产物转存完成
// 才置为 succeeded；上游 expired（Ark 的产物 URL 过期）在这里归一化为 StatusFailed。
// 混用一个类型会让这层区别消失。
type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

// InputRef 是一份已经在本平台存储里的输入素材，driver 按各自协议决定
// 是转成 URL、base64 还是直接上传字节喂给上游。
type InputRef struct {
	// Slot 是 InputSlotSpec.Key，如 reference_images / first_frame。
	Slot string
	// URL 是本平台可对外访问的地址，供上游回源拉取。
	URL string
	// StorageKey 是本平台存储键，供 driver 需要读原始字节时使用。
	StorageKey string
	MIME       string
	Bytes      int64
}

// SubmitInput 是喂给 driver 的一次调用的全部输入。
//
// 它是**平台侧**的形态：Params 的 key 是 ParamSpec.Key，还没翻译成上游字段名。
// 翻译由 mapping.Renderer 按 ModelConfig.RequestMapping 完成，driver 只负责
// 协议骨架（端点、鉴权、寻址、产物取法）。
type SubmitInput struct {
	TaskID        string
	Provider      domain.Provider
	Model         domain.ModelConfig
	UpstreamModel string
	Prompt        string
	Params        map[string]any
	Inputs        []InputRef
	// Credential 是从 env 读出的密钥明文，仅在进程内传递。
	// 它不入库、不进日志、不进 ProbeResult.RenderedRequest。
	Credential string
	// IdempotencyKey 供支持幂等头的上游使用，避免重试产生两次计费调用。
	IdempotencyKey string
}

// SubmitResult 是提交动作的返回。
//
// UpstreamRef 是后续轮询的寻址依据，**它的语义按协议不同而不同**：
// ark / openai_video 是任务 id，google_lro 是 operation name（一个完整路径）。
// L2 只把它当不透明字符串存进 tasks.upstream_ref 再原样回传给同一个 driver，
// 绝不解析——这正是"协议差异不外泄"的具体体现。
//
// Inline 在同步族（chat / images）里直接携带产物，此时 UpstreamRef 为空。
type SubmitResult struct {
	UpstreamRef string
	Status      Status
	// RawStatus 是归一化前的上游原始状态字符串，排障用，落 tasks.upstream_status_raw。
	RawStatus     string
	Inline        []ArtifactRef
	QueuePosition *int
	ETASeconds    *float64
}

// PollRequest 是一次轮询的输入。
type PollRequest struct {
	TaskID      string
	Provider    domain.Provider
	Model       domain.ModelConfig
	UpstreamRef string
	Credential  string
	// Attempt 是本次是第几次轮询，driver 可据此做指数退避。
	Attempt int
}

// PollResult 是一次轮询的结果。
//
// Artifacts 只在 Status == StatusSucceeded 时有值，且此时里面的 URL **随时会失效**
// （Ark 的 video_url 只有 24h），因此 L2 必须立刻交给 asset.Transferor 转存，
// 不能把它存进数据库当作长期地址。
//
// Err 在 Status == StatusFailed 时有值，且已经由 driver 归类到平台的失败三分类
// （上游 429 / rate limit 文案 → upstream_rate_limited，上游审核拒绝 → content_rejected，
// 其余 → internal_error）。这个归类是 driver 的职责，L2 不认识上游错误码。
type PollResult struct {
	Status        Status
	RawStatus     string
	Progress      *float64
	QueuePosition *int
	ETASeconds    *float64
	Artifacts     []ArtifactRef
	Err           *domain.TaskError
}

// ArtifactKind 表示产物以什么形态交付。
//
// 三种形态不是过度设计，是三家视频供应商的实测差异：
//
//	KindURL     Ark 返回 content.video_url，一个有时效的直链
//	KindBinary  Sora 的 GET /v1/videos/{id}/content 直接流式吐 mp4 裸字节，根本没有 URL
//	KindBase64  Veo 的 predictLongRunning 结果可能内联 base64
//	KindGCSURI  Veo 也可能给一个 gs:// 地址，需要用 Google 凭证再取
//
// 硬统一成 URL 会逼着 Sora 那条路先落地再造一个假 URL，凭空多一次读写。
type ArtifactKind string

const (
	KindURL    ArtifactKind = "url"
	KindBinary ArtifactKind = "binary"
	KindBase64 ArtifactKind = "base64"
	KindGCSURI ArtifactKind = "gcs_uri"
)

// ArtifactRef 指向一件上游产物。**它是短命的**：URL 会过期、流只能读一次。
// 它的唯一正当用途是立刻交给 asset.Transferor 转存，绝不入库。
type ArtifactRef struct {
	Kind ArtifactKind
	Type domain.AssetType
	MIME string

	// URL 用于 KindURL；ExpiresAt 已知时填上（Sora 给 expires_at，Ark 是固定 24h）。
	URL       string
	ExpiresAt *time.Time

	// Base64 用于 KindBase64。
	Base64 string

	// GCSURI 用于 KindGCSURI，形如 gs://bucket/object。
	GCSURI string

	// Index 是同一任务产出多件产物时的序号（如一次出 4 张图），决定展示顺序。
	Index int

	Width      *int
	Height     *int
	DurationMS *int
	Bytes      int64
}

// ArtifactStream 是产物的字节流。
//
// Body 必须由调用方关闭。用流而不是 []byte 是硬要求：视频动辄几十上百 MB，
// 全读进内存再落盘，几个并发就能把进程撑爆；转存路径必须是 io.Copy 到存储，
// 内存占用与文件大小无关。
type ArtifactStream struct {
	Body io.ReadCloser
	MIME string
	// Bytes 是已知的内容长度，未知时为 0（chunked 传输拿不到长度）。
	Bytes int64
}

// Driver 是所有驱动的公共部分。
type Driver interface {
	// Name 是驱动标识，与 ModelConfig 的 protocol_family / video_protocol 对应，
	// 也是 Registry 的查找键。
	Name() string
	// Family 决定这个驱动走同步还是异步路径。
	Family() domain.ProtocolFamily
}

// SyncDriver 是同步驱动（chat / images）：一次往返就拿到产物。
type SyncDriver interface {
	Driver
	// Invoke 发起调用并直接返回产物。上下文超时即为上游超时，
	// 由 Provider.TimeoutMS 决定。
	Invoke(ctx context.Context, in SubmitInput) (SubmitResult, error)
}

// AsyncDriver 是异步驱动（video）：提交 → 轮询 → 取产物三步。
type AsyncDriver interface {
	Driver
	// Submit 把任务提交给上游，返回后续轮询用的 UpstreamRef。
	Submit(ctx context.Context, in SubmitInput) (SubmitResult, error)
	// Poll 查询一次上游状态。它必须是**幂等**的：webhook 与定时轮询是推进
	// 同一个状态机的两条同源路径，任何一条被调用多次都不应产生副作用。
	Poll(ctx context.Context, req PollRequest) (PollResult, error)
	// FetchArtifact 把一个 ArtifactRef 变成可读字节流。
	// 各协议在这里的实现差别最大：KindURL 是一次 GET，KindBinary 是带鉴权的
	// 流式端点，KindBase64 是本地解码，KindGCSURI 要走 Google 凭证。
	FetchArtifact(ctx context.Context, ref ArtifactRef, req PollRequest) (ArtifactStream, error)
	// DefaultPollInterval 是该协议的建议轮询间隔，
	// 在 ModelConfig.PollIntervalSeconds 为空时生效。
	DefaultPollInterval() time.Duration
}

// Registry 按驱动名查找驱动。
//
// L2 拿到一条 ModelConfig 后，用 protocol_family（video 时用 video_protocol）
// 去 Registry 里取驱动。**这是全系统唯一一处按协议名分发的地方**，
// 也正因为集中在这里，其他所有层才能对协议差异一无所知。
type Registry interface {
	Sync(name string) (SyncDriver, bool)
	Async(name string) (AsyncDriver, bool)
	// Names 列出已注册的驱动名，供管理后台展示可选协议。
	Names() []string
}
