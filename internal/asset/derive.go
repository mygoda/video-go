package asset

import (
	"context"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Variant 是资产的一个规格。
type Variant string

const (
	// VariantOriginal 是原图 / 原视频。
	VariantOriginal Variant = "original"
	// VariantThumb512 是长边 512px 的缩略图。
	VariantThumb512 Variant = "thumb_512"
	// VariantPoster 是视频封面帧。
	VariantPoster Variant = "poster"
)

// Thumb512MaxEdge 是缩略图的长边像素。
//
// 512 是照着画布的分级加载策略定的：画布缩放 0.4 ≤ k < 1 时用 thumb_512，
// k ≥ 1 才加载原图。同屏 20+ 张卡片全用原图必炸，全用更小的缩略图在
// k 接近 1 时又糊。
const Thumb512MaxEdge = 512

// Deriver 生成派生规格。
//
// 两种派生:
//
//	thumb_512  图片与视频都要。画布 0.4 ≤ k < 1 时加载它而不是原图。
//	poster     只有视频要。<video> 元素挂载前显示它，否则视频卡片在
//	           加载完成前是一块空白，同屏十几个空白块的画布看起来像坏了。
//
// 派生在转存完成后立刻做，与转存同属任务成功的收尾动作。
// 派生失败**不应**让任务失败：原图已经安全落地了，缺个缩略图只是体验降级
// （前端回退到原图），而任务失败要退款、要让用户重跑一次真实生成——
// 代价完全不对等。这与转存失败必须让任务失败正好相反，两者的区别在于
// 原始字节是否还救得回来：派生随时可以重做，上游 URL 过期了就没了。
type Deriver interface {
	// Derive 为一件资产生成所有适用的派生规格，返回生成的规格与其 storage key。
	//
	// 入参是指针：视频的宽高与时长只有解开容器才知道，而解容器与抽封面帧是
	// 同一步，因此顺带补出来的这三项要回填给调用方（见 deriver.Derive）。
	Derive(ctx context.Context, a *domain.Asset) (map[Variant]string, error)
	// Thumbnail 生成长边 Thumb512MaxEdge 的缩略图。
	Thumbnail(ctx context.Context, a domain.Asset) (string, error)
	// Poster 抽取视频封面帧，仅对 AssetTypeVideo 有意义。
	Poster(ctx context.Context, a domain.Asset) (string, error)
	// FillMeta 为一件**已经落库**的资产补上缺失的宽高与时长并写回，
	// 返回是否补上了新东西。供回填任务把历史资产追平到 Derive 现在的产出。
	FillMeta(ctx context.Context, a *domain.Asset) (bool, error)
}
