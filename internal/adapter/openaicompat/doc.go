// Package openaicompat implements the L0 driver for the OpenAI-compatible
// synchronous protocols: chat completions and image generations.
//
// # 协议骨架
//
//	对话  POST {base_url}/v1/chat/completions
//	出图  POST {base_url}/v1/images/generations
//	鉴权  Authorization: Bearer <key>
//
// **同步**：一次 HTTP 往返就拿到结果，没有提交-轮询两段。因此本驱动实现的是
// adapter.SyncDriver 而不是 AsyncDriver——产物直接落在 SubmitResult.Inline 里，
// UpstreamRef 为空，L2 不会为它安排轮询。
//
// 之所以叫 "compat" 而不是 "openai"，是因为这套协议已经是事实标准：
// 大量第三方与自建推理服务都兼容它。同一个驱动配上不同的 base_url 与凭证
// 就能接一家新供应商，连 driver 都不用新写——这是协议族抽象带来的最大一笔红利。
//
// # 两个端点，一个驱动
//
// chat 与 images 是两个 ProtocolFamily，但共用鉴权、错误体、超时语义，
// 差别只在端点与响应形状，因此放在同一个包里由 Family 字段区分，
// 而不是拆成两个几乎一样的包。
//
// # 状态
//
// 同步调用没有中间状态：HTTP 200 即成功，非 2xx 即失败。statusMap 在这里
// 只有两个条目，形式上与异步驱动保持一致，让"归一化关系有唯一出处"这条
// 在四个驱动里长得一样。
package openaicompat

import (
	"net/http"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/adapter/mapping"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Registry 查找键。chat 与 images 是两个族，共用本驱动实现。
const (
	NameChat   = string(domain.FamilyChat)
	NameImages = string(domain.FamilyImages)
)

// 协议端点，相对 Provider.BaseURL。
const (
	// PathChatCompletions 是对话端点，同步返回。
	PathChatCompletions = "/v1/chat/completions"
	// PathImagesGenerations 是出图端点，同步返回。
	PathImagesGenerations = "/v1/images/generations"
)

// statusMap 把同步调用的结果映射到平台归一化状态。
// 同步调用没有 queued / running，只有成或败。
var statusMap = map[string]adapter.Status{
	"ok":    adapter.StatusSucceeded,
	"error": adapter.StatusFailed,
}

// Driver 是 OpenAI 兼容同步协议的驱动实例。
//
// Family 决定打哪个端点：FamilyChat 走 PathChatCompletions，
// FamilyImages 走 PathImagesGenerations。同一个进程里通常注册两个实例，
// 各自 Family 不同、其余配置相同。
type Driver struct {
	// HTTP 是发起上游调用的客户端。
	HTTP *http.Client
	// Renderer 把 ModelConfig.RequestMapping 渲染成上游请求体。
	Renderer mapping.Renderer
	// ProtocolFamily 区分 chat 与 images。
	ProtocolFamily domain.ProtocolFamily
}
