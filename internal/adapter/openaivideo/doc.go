// Package openaivideo implements the L0 driver for the OpenAI video protocol
// (Sora 2).
//
// # 协议骨架
//
//	提交  POST {base_url}/v1/videos
//	轮询  GET  {base_url}/v1/videos/{video_id}
//	取物  GET  {base_url}/v1/videos/{video_id}/content
//	鉴权  Authorization: Bearer <key>
//
// 轮询按提交响应里的 video id 寻址。
//
// # 产物取法：与另外两家的根本差异
//
// **content 端点直接流式返回 mp4 裸字节，不返回 URL。**
//
// 这一条决定了 adapter.ArtifactRef 必须有 KindBinary 这个形态。如果 L0 抽象
// 强行规定"产物一律是一个 URL"，这条路就只能先把字节落到临时文件、再编一个
// 假 URL 给上层，凭空多一轮读写，而且那个假 URL 还得有人负责清理。
// 让抽象承认三种取法，比让实现绕过抽象要便宜。
//
// FetchArtifact 在本驱动里是**带鉴权的流式 GET**：必须带 Authorization 头，
// 且响应体要 io.Copy 直通存储，不能读进内存——一段视频几十上百 MB，
// 并发几个就能把进程撑爆。
//
// # 过期
//
// 提交/轮询响应里带 expires_at。与 Ark 的固定 24h 不同，这里是上游给的显式时刻，
// driver 应把它填进 ArtifactRef.ExpiresAt。结论仍然一样：产物必须在任务成功
// 那一刻立刻转存，库里存我们自己的 storage key。
//
// # 状态
//
// 上游四个状态，其中 in_progress 对应平台的 running——名字不同，
// 这类纯字符串差异就是 statusMap 存在的全部理由。上游没有独立的 canceled 状态，
// 取消由平台侧标记。
package openaivideo

import (
	"net/http"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/adapter/mapping"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Name 是 Registry 的查找键，与 domain.VideoProtocolOpenAIVideo 一致。
const Name = string(domain.VideoProtocolOpenAIVideo)

// Family 是本驱动所属的协议族。
const Family = domain.FamilyVideo

// 协议端点，相对 Provider.BaseURL。
const (
	// PathSubmit 是提交端点。
	PathSubmit = "/v1/videos"
	// PathPollFormat 是轮询端点模板，占位符为上游 video id。
	PathPollFormat = "/v1/videos/%s"
	// PathContentFormat 是取产物端点模板，占位符为上游 video id。
	// 它**流式返回 mp4 裸字节**，不是 JSON、也不是一个 URL。
	PathContentFormat = "/v1/videos/%s/content"
)

// ArtifactMIME 是 content 端点返回的内容类型。
const ArtifactMIME = "video/mp4"

// DefaultPollInterval 是本协议的建议起始轮询间隔，之后指数退避。
const DefaultPollInterval = 15 * time.Second

// statusMap 是上游状态到平台归一化状态的映射，是这份归一化关系的唯一出处。
//
// in_progress → running: 语义一致，只是叫法不同。
// 上游无 canceled 状态，取消是平台侧的动作，不出现在本表里。
var statusMap = map[string]adapter.Status{
	"queued":      adapter.StatusQueued,
	"in_progress": adapter.StatusRunning,
	"completed":   adapter.StatusSucceeded,
	"failed":      adapter.StatusFailed,
}

// Driver 是 OpenAI video 协议的驱动实例。无状态，可被多个 provider 共用。
type Driver struct {
	// HTTP 是发起上游调用的客户端。取产物时走流式读，不要给它套会缓冲整个响应的 transport。
	HTTP *http.Client
	// Renderer 把 ModelConfig.RequestMapping 渲染成上游请求体。
	Renderer mapping.Renderer
	// PollInterval 覆盖 DefaultPollInterval，零值表示用默认。
	PollInterval time.Duration
}
