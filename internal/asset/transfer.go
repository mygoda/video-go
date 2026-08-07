package asset

import (
	"context"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Transferor 把上游产物转存到本平台存储。
//
// # 硬约束（本层存在的全部理由）
//
// **上游产物 URL 会过期**——Ark 的 content.video_url 只有 24 小时，
// Sora 在响应里直接给一个 expires_at。因此:
//
//	产物在**任务成功那一刻**被下载并重新托管到本平台存储，
//	数据库里存的是**我们自己的 storage key**，永远不存上游 URL。
//
// 这不是优化，是正确性问题。把上游 URL 当长期地址存进 assets 表，
// 用户第二天回来看到的就是一整屏打不开的卡片，而且**不可挽回**——
// 那时上游已经不认那个 URL 了，我们手上没有任何字节可以补救。
// 资产库、血缘、做同款、一键成片全部建立在"产物永远还在"这个前提上，
// 这个前提只能由转存来兑现。
//
// 因此转存失败必须让任务**失败**（并退款），而不是"标记成功但产物暂缺，
// 之后再补" ——之后就没有之后了，URL 已经过期。宁可让用户重跑一次，
// 也不能留一条永远修不好的成功记录。
//
// # 实现要点
//
//   - 全程流式：driver.FetchArtifact 拿到 io.ReadCloser 后直接 io.Copy 进
//     Store.Put，不落中间的完整 []byte。
//   - 转存与 assets 落库要一起成立：先写存储再写库，写库失败要删掉已写入的对象，
//     否则会攒下无主文件（admin 的 storage/cleanup 兜底清理 orphan_files，
//     但那是兜底，不是常规路径）。
//   - 四种 ArtifactKind 都从这里进：URL 一次 GET、Binary 直接消费流、
//     Base64 本地解码、GCSURI 带 Google 凭证再取。差异在 driver 侧消化，
//     Transferor 只看到一个流。
//
// Transfer 处理一件产物；TransferAll 处理一次任务的全部产物（一次出 4 张图），
// 并保持 ArtifactRef.Index 的顺序作为展示顺序。
type Transferor interface {
	Transfer(ctx context.Context, userID, taskID string, ref adapter.ArtifactRef, req adapter.PollRequest) (domain.Asset, error)
	TransferAll(ctx context.Context, userID, taskID string, refs []adapter.ArtifactRef, req adapter.PollRequest) ([]domain.Asset, error)
	// Promote 把一个临时 upload 提升为正式 asset（被任务引用时触发）。
	// upload 是 24h 回收的临时对象，被引用后必须提升，否则回收会把
	// 正在被血缘引用的输入素材删掉。
	Promote(ctx context.Context, uploadID string) (domain.Asset, error)
}
