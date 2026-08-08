// Package compose is the L0 driver for local sequence composition:
// it turns an ordered set of already-produced assets into one "分镜序列" manifest.
//
// # 它是降级形态，而且是有意的
//
// 真正的一键成片要把 N 段视频按顺序转码拼接成一条新视频，那需要 ffmpeg
// 或一个上游转码服务。本项目明确不引入外部二进制依赖，而现有的任何上游
// 都没有声明"多段视频输入 → 一条视频输出"的槽位。于是这里退一步：
// **产出一份有序的产物清单**（片段顺序、每段的可访问地址、时长、画幅），
// 前端拿到它可以按顺序连播，也可以交给未来真正的拼接实现直接消费。
// 缺的是转码，不是数据——清单里已经有拼接所需的全部信息。
//
// # 为什么仍然做成一个 driver 而不是在 handler 里就地生成
//
// 因为要的是一条**真任务**：任务表、queued→running→succeeded 状态机、
// 失败退款、SSE 推送、任务监控里能看到，一样不能少。而进入那条链路的
// 唯一入口就是 driver。把清单在 HTTP handler 里拼出来直接返回，就又回到了
// "前端拿到的不是一个可查的任务"——那正是这次要修掉的 501 的另一种形态。
//
// 换成真拼接时改动只在本包：家族名、槽位约定、任务链路全都不用动。
package compose

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Name 是 Registry 的查找键，同时也是 models.protocol_family 的取值。
const Name = string(domain.FamilyCompose)

// Family 是本驱动的协议族。
const Family = domain.FamilyCompose

// SlotSegments 是片段输入槽的 key。
//
// 它同时是三处约定的交汇点：models.capability 里声明的输入槽、httpapi 提交
// 任务时填的 inputs、以及 executor 记血缘时识别 composed_from 边的依据。
// 取名与 domain.LineageComposedFrom 一致不是巧合——这个槽的语义就是那条边。
const SlotSegments = string(domain.LineageComposedFrom)

// ManifestMIME 是清单的媒体类型。它决定了资产落盘的扩展名与 assets.type
// （application/json 归到 text），前端据此知道拿到的是清单而不是视频。
const ManifestMIME = "application/json"

// ManifestKind 写在清单里，供消费方判定这份 JSON 是什么。
const ManifestKind = "storyboard_sequence"

// ManifestVersion 是清单结构的版本号。真拼接落地后清单大概率要加字段
// （转码参数、转场），带上版本号让那时的消费方能分辨手里这份是老的。
const ManifestVersion = 1

// Segment 是清单里的一段。
//
// URL 指向本平台的 /api/assets/{id}/content，不是上游地址——上游地址会过期，
// 而清单是要长期存着的资产。
type Segment struct {
	Index int    `json:"index"`
	URL   string `json:"url"`
	MIME  string `json:"mime"`
	Bytes int64  `json:"bytes,omitempty"`
}

// Manifest 是「分镜序列」资产的全部内容。
type Manifest struct {
	Kind    string    `json:"kind"`
	Version int       `json:"version"`
	TaskID  string    `json:"task_id"`
	Title   string    `json:"title,omitempty"`
	Count   int       `json:"segment_count"`
	Created time.Time `json:"created_at"`
	// Degraded 与 DegradedReason 是写给消费方的实话：这不是一条拼好的视频。
	// 前端据此展示"顺序连播"而不是一个播放器，也不会有人把它当成品导出。
	Degraded       bool      `json:"degraded"`
	DegradedReason string    `json:"degraded_reason"`
	Segments       []Segment `json:"segments"`
}

// Driver 是本驱动的配置。它没有配置项——本地合成不需要端点、凭证或超时。
// 保留这个结构体是为了与其余 driver 的构造形态一致（New(Driver{...})）。
type Driver struct {
	// Now 可在测试里注入固定时钟，为 nil 时取 time.Now。
	Now func() time.Time
}

type driver struct{ Driver }

// New 返回一个本地合成驱动。
func New(cfg Driver) adapter.SyncDriver { return driver{Driver: cfg} }

func (d driver) Name() string                  { return Name }
func (d driver) Family() domain.ProtocolFamily { return Family }

func (d driver) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Invoke 把输入槽里的片段按顺序编成清单并直接返回。
//
// 同步而不是异步：本地编一份 JSON 没有"等上游"这件事，做成异步只会让
// 用户对着一个必然在下一次轮询就完成的任务转圈。它仍然是一条真任务——
// L2 走的是与 chat / images 完全相同的同步收尾（转存 → 血缘 → 结算 → SSE）。
//
// 片段少于 2 段直接报参数错误：一段的"合成"没有意义，而让它成功会产出一份
// 骗人的单段清单。这里归到 invalid_param 而不是 internal——是提交方填错了。
func (d driver) Invoke(ctx context.Context, in adapter.SubmitInput) (adapter.SubmitResult, error) {
	if err := ctx.Err(); err != nil {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, err, "合成已取消")
	}

	segments := make([]Segment, 0, len(in.Inputs))
	for _, ref := range in.Inputs {
		if ref.Slot != SlotSegments {
			continue
		}
		segments = append(segments, Segment{
			Index: len(segments),
			URL:   ref.URL,
			MIME:  ref.MIME,
			Bytes: ref.Bytes,
		})
	}
	if len(segments) < 2 {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInvalidParam, nil,
			"合成至少需要 2 个片段，收到 %d 个", len(segments))
	}

	manifest := Manifest{
		Kind:           ManifestKind,
		Version:        ManifestVersion,
		TaskID:         in.TaskID,
		Title:          in.Prompt,
		Count:          len(segments),
		Created:        d.now().UTC(),
		Degraded:       true,
		DegradedReason: "本地合成只编排顺序，不做转码拼接；真正的成片需要转码能力（ffmpeg 或上游拼接接口）",
		Segments:       segments,
	}

	body, err := json.Marshal(manifest)
	if err != nil {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, err, "编排分镜序列失败")
	}

	// 与 mock / openaicompat 同形：同步族用 KindBase64 内联交付产物，
	// L3 走的是同一条解码 → 落盘路径，本驱动不需要 FetchArtifact。
	return adapter.SubmitResult{
		Status:    adapter.StatusSucceeded,
		RawStatus: "succeeded",
		Inline: []adapter.ArtifactRef{{
			Kind:   adapter.KindBase64,
			Type:   domain.AssetTypeText,
			MIME:   ManifestMIME,
			Base64: base64.StdEncoding.EncodeToString(body),
			Bytes:  int64(len(body)),
		}},
	}, nil
}

var _ adapter.SyncDriver = driver{}
