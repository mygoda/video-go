// Package predictions implements the L0 driver for GPUGeek's model-market API
// (Replicate-style POST /predictions).
//
// # 为什么需要这个包
//
// GPUGeek 有**两套互不相通的 API 面**：
//
//	/v1/models、/v1/chat/completions   OpenAI 兼容层，只有 LLM 与视觉理解
//	POST /predictions                  模型市场，文生图 / 文生视频都在这里
//
// 早先只探了第一套，于是得出「平台没有生成能力」的结论（见
// migrations/000003 的文件头）。/v1/images/generations 回 404 并不代表
// 平台没有图像能力——生成类模型根本不走那条路。
//
// # 协议骨架
//
//	提交  POST {base_url}/predictions      {"model":"供应商/模型名","input":{...}}
//	轮询  GET  {base_url}/predictions/{id}
//	鉴权  Authorization: Bearer <key>      —— 与 OpenAI 完全一致
//
// 轮询按提交响应里的 id 寻址。产物是一个有时效的直链，取物就是一次不带鉴权的 GET。
//
// # 一个协议，两个驱动实例
//
// 同一个端点既出图也出视频，差别只在**耗时**：实测文生图约 10 秒、
// 文生视频约 256 秒。10 秒的东西让它走提交-轮询会白白多绕一圈状态机与一次
// 数据库往返，256 秒的东西让它走同步则必然把 HTTP 连接挂死。因此本包注册
// 两个实例，共用 "predictions" 这一个查找键：
//
//	NewSyncDriver   Family() = predictions   图像走它，Invoke 内部等到终态
//	NewAsyncDriver  Family() = video         视频走它，Submit / Poll / FetchArtifact
//
// 这不是特例，是 mock 已经在用的模式：adapter.IsAsync 靠
// async.Family() == m.Family 在同名双注册时选边，因此图像任务不会被送进
// 那个产视频的分支。
//
// # 三个实测出来的协议坑
//
//  1. **output 的类型不一致**：图像是字符串数组 ["url"]，视频是单个字符串 "url"。
//     两种都要吃——假定其一就是一个必然发生的 panic（见 parseOutput）。
//  2. **上游会回填 input**：轮询响应里的 input 会多出我们没传的字段
//     （实测 input_has_video: false，上游按输入内容做计费分档）。
//     因此绝不能用「回显的 input 等于我发的 input」做任何断言。
//  3. **产物 URL 只有 24 小时有效期**（X-Tos-Expires=86400），与 Ark 一致。
//     这正是 L3 必须在任务成功那一刻立刻转存的原因。
//
// # 状态
//
// Replicate 风格的五个状态。starting / processing 都是在途，
// canceled 是上游确实有的终态（与 openaivideo 不同），因此照实映射。
package predictions

import (
	"net/http"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/adapter/mapping"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Name 是 Registry 的查找键。同步与异步两个实例共用它，
// 因此它同时等于 domain.FamilyPredictions 与 domain.VideoProtocolPredictions。
const Name = string(domain.FamilyPredictions)

// SyncFamily / AsyncFamily 是两个实例各自自报的协议族。
//
// 它们必须与模型配置里的 protocol_family 那一列逐字对得上——
// adapter.IsAsync 正是拿这个值跟模型配置比对来决定走哪条路的。
const (
	SyncFamily  = domain.FamilyPredictions
	AsyncFamily = domain.FamilyVideo
)

// 协议端点，相对 Provider.BaseURL。
const (
	// PathSubmit 是提交端点。
	PathSubmit = "/predictions"
	// PathPollFormat 是轮询端点模板，占位符为上游 prediction id。
	PathPollFormat = "/predictions/%s"
)

// 请求体的顶层字段名。model 与 input 是这个协议仅有的两个顶层字段，
// 全部生成参数都在 input 里——request_mapping 必须渲染成这个形状。
const (
	FieldModel = "model"
	FieldInput = "input"
)

// ArtifactTTL 是产物直链的有效期（上游签名里的 X-Tos-Expires=86400）。
// 产物必须在这个窗口内转存到本平台存储。
const ArtifactTTL = 24 * time.Hour

// DefaultPollInterval 是本协议的建议轮询间隔。
//
// 15s 是照着实测定的：视频 p50 约 256 秒，再快也只是多打几十次无效请求；
// 再慢则终态被发现得太晚，而产物直链的 24h 窗口是从完成时刻开始走的。
const DefaultPollInterval = 15 * time.Second

// SyncSettleInterval 是同步实例在上游没有一次往返给出终态时的重试节奏。
//
// 它比 DefaultPollInterval 短得多：走同步这条路的模型本来就是秒级的
// （实测出图 10 秒），排队几秒就该醒一次，等 15 秒等于把一次快调用拖成慢调用。
const SyncSettleInterval = 2 * time.Second

// statusMap 是上游状态到平台归一化状态的映射，是这份归一化关系的唯一出处。
//
// starting 与 processing 都是在途，只是前者还没开始算。分开保留原始串
// （落 tasks.upstream_status_raw）而归一化到同一个平台状态，
// 是因为平台的状态机分不出这两者，但排障时分得出有意义。
var statusMap = map[string]adapter.Status{
	"starting":   adapter.StatusQueued,
	"queued":     adapter.StatusQueued,
	"processing": adapter.StatusRunning,
	"succeeded":  adapter.StatusSucceeded,
	"failed":     adapter.StatusFailed,
	"canceled":   adapter.StatusCanceled,
	"cancelled":  adapter.StatusCanceled,
}

// Driver 是 predictions 协议的驱动配置。
//
// 它本身无状态：所有 per-provider 的东西（base_url、凭证、超时）都随
// adapter.SubmitInput / PollRequest 传入，因此同一份配置可以服务多个
// 配成 predictions 协议的 provider。
type Driver struct {
	// HTTP 是发起上游调用的客户端。由外部注入，便于在测试里换成假 transport。
	HTTP *http.Client
	// Renderer 把 ModelConfig.RequestMapping 渲染成上游请求体。
	// driver 只管协议骨架，字段差异全在 mapping 里。
	Renderer mapping.Renderer
	// PollInterval 覆盖 DefaultPollInterval，零值表示用默认。
	PollInterval time.Duration
	// SettleInterval 覆盖 SyncSettleInterval，零值表示用默认。
	// 单测靠它把同步等待压到毫秒级。
	SettleInterval time.Duration
}
