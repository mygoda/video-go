package mock

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// AsyncFamily 是异步 mock 驱动对外声明的协议族。
//
// 它是 video 而不是 mock，因为种子数据里 mock-video-1 就是
// protocol_family='video' + video_protocol='mock'——Family() 必须
// 如实反映模型配置里的那一列，否则 L2 按 Family 分同步/异步两条路时
// 会把它送错方向。查找键仍然是 Name（"mock"），见 adapter.DriverName。
const AsyncFamily = domain.FamilyVideo

// MockVideoDurationMS 是内嵌样片的时长，与 fixtures/sample.mp4 一致。
const MockVideoDurationMS = 2000

// 内嵌样片的画幅，与 fixtures/sample.mp4 一致。
const (
	MockVideoWidth  = 256
	MockVideoHeight = 144
)

// Name 返回驱动标识。
func (d Driver) Name() string { return Name }

// Family 返回同步 mock 的协议族。
func (d Driver) Family() domain.ProtocolFamily { return Family }

func (d Driver) delay() time.Duration {
	if d.SimulatedDelay > 0 {
		return d.SimulatedDelay
	}
	return DefaultSimulatedDelay
}

func (d Driver) pollInterval() time.Duration {
	if d.PollInterval > 0 {
		return d.PollInterval
	}
	return DefaultPollInterval
}

func (d Driver) failureCode() domain.TaskErrorCode {
	if d.FailureCode != "" {
		return d.FailureCode
	}
	return domain.TaskErrorInternal
}

// normalize 把 mock 的内部状态串映射成平台状态。
//
// 它存在的意义不在于映射本身（两边字面量一样），而在于**形状**:
// mock 和三个真实驱动一样有一张 statusMap、一样在这里做归一化，
// 因此 L2 没有任何办法从行为上分辨出哪个是 mock。
func normalize(raw string) adapter.Status {
	if s, ok := statusMap[raw]; ok {
		return s
	}
	// 未知上游状态按 running 处理而不是报错：真实上游随时可能加一个新状态，
	// 把在途任务判死比多轮询几次代价大得多。
	return adapter.StatusRunning
}

// ── 同步驱动（对应 protocol_family='mock' 的图像模型）──────────────────

type syncDriver struct{ Driver }

// NewSyncDriver 返回同步 mock 驱动，对应种子数据里的 mock-image-1
// （protocol_family='mock'）。
//
// cfg 里只有 FailureRate / FailureCode / PlaceholderRoot 对它有意义，
// SimulatedDelay 与 PollInterval 是异步路径才用得上的东西。
func NewSyncDriver(cfg Driver) adapter.SyncDriver { return syncDriver{Driver: cfg} }

// Invoke 就地生成一张真 PNG 并直接返回，不做任何网络调用。
//
// 产物以 KindBase64 交付而不是 KindBinary:KindBinary 的取字节动作在
// adapter.AsyncDriver.FetchArtifact 上，同步驱动没有那个方法。
// base64 内联既是零网络的，也正是真实 OpenAI 兼容出图接口
// （b64_json）的形态，L3 走的是同一条解码路径。
func (d syncDriver) Invoke(ctx context.Context, in adapter.SubmitInput) (adapter.SubmitResult, error) {
	if err := ctx.Err(); err != nil {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, err, "调用已取消")
	}

	// 失败注入走在生成之前：真实上游拒绝请求时也不会先出图再报错。
	if decideOutcome(in.TaskID, d.FailureRate) == outcomeFailed {
		code := d.failureCode()
		return adapter.SubmitResult{
				Status:    adapter.StatusFailed,
				RawStatus: "failed",
			}, adapter.CallError(code, nil,
				"mock 注入失败（FailureRate=%.2f）", d.FailureRate)
	}

	count := imageCount(in.Params)
	refs := make([]adapter.ArtifactRef, 0, count)
	for i := 0; i < count; i++ {
		png, w, h, err := renderPlaceholderPNG(in.Params, in.TaskID, i)
		if err != nil {
			return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, err, "mock 出图失败")
		}
		if err := d.dumpPlaceholder(in.TaskID, i, "png", png); err != nil {
			return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, err, "mock 占位图落盘失败")
		}
		refs = append(refs, adapter.ArtifactRef{
			Kind:   adapter.KindBase64,
			Type:   domain.AssetTypeImage,
			MIME:   PlaceholderImageMIME,
			Base64: encodeBase64(png),
			Index:  i,
			Width:  &w,
			Height: &h,
			Bytes:  int64(len(png)),
		})
	}

	// 同步族的 UpstreamRef 恒为空，L2 据此不为它安排轮询。
	return adapter.SubmitResult{
		Status:    adapter.StatusSucceeded,
		RawStatus: "succeeded",
		Inline:    refs,
	}, nil
}

// ── 异步驱动（对应 protocol_family='video' + video_protocol='mock'）────

type asyncDriver struct{ Driver }

// NewAsyncDriver 返回异步 mock 驱动，对应种子数据里的 mock-video-1
// （protocol_family='video'、video_protocol='mock'）。
func NewAsyncDriver(cfg Driver) adapter.AsyncDriver { return asyncDriver{Driver: cfg} }

// Family 返回 video——见 AsyncFamily 的注释。
func (d asyncDriver) Family() domain.ProtocolFamily { return AsyncFamily }

// Submit 立刻返回一个引用，不阻塞。
//
// 任务的结局（成功还是失败）**在这里就定死并编进引用**，之后的每次 Poll
// 只是把时间代进一个纯函数。这让 Poll 天然幂等——webhook 与定时轮询
// 同时打进来、或同一个状态被处理两次，结果必然一致。
func (d asyncDriver) Submit(ctx context.Context, in adapter.SubmitInput) (adapter.SubmitResult, error) {
	if err := ctx.Err(); err != nil {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, err, "提交已取消")
	}

	ref := upstreamRef{
		TaskID:              in.TaskID,
		SubmittedAtUnixNano: time.Now().UnixNano(),
		DelayNanos:          int64(d.delay()),
		Outcome:             decideOutcome(in.TaskID, d.FailureRate),
		Width:               MockVideoWidth,
		Height:              MockVideoHeight,
		DurationMS:          MockVideoDurationMS,
	}
	if ref.Outcome == outcomeFailed {
		ref.FailureCode = d.failureCode()
	}

	encoded, err := encodeRef(ref)
	if err != nil {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, err, "mock 提交失败")
	}

	eta := d.delay().Seconds()
	return adapter.SubmitResult{
		UpstreamRef: encoded,
		Status:      adapter.StatusQueued,
		RawStatus:   "queued",
		ETASeconds:  &eta,
	}, nil
}

// Poll 由引用里的提交时刻推出当前状态。它不写任何状态，因此天然幂等。
func (d asyncDriver) Poll(ctx context.Context, req adapter.PollRequest) (adapter.PollResult, error) {
	if err := ctx.Err(); err != nil {
		return adapter.PollResult{}, adapter.CallError(domain.TaskErrorInternal, err, "轮询已取消")
	}

	ref, err := decodeRef(req.UpstreamRef)
	if err != nil {
		return adapter.PollResult{}, adapter.CallError(domain.TaskErrorInternal, err, "mock 轮询失败")
	}

	now := time.Now()
	raw, progress := ref.stateAt(now)
	status := normalize(raw)

	out := adapter.PollResult{Status: status, RawStatus: raw}

	switch status {
	case adapter.StatusQueued:
		eta := ref.etaAt(now)
		out.ETASeconds = &eta
		out.Progress = &progress
	case adapter.StatusRunning:
		eta := ref.etaAt(now)
		out.ETASeconds = &eta
		out.Progress = &progress
	case adapter.StatusFailed:
		code := ref.FailureCode
		if code == "" {
			code = domain.TaskErrorInternal
		}
		out.Err = adapter.Failure(code, "mock 注入的失败（用于验证退款与失败三分类呈现）")
	case adapter.StatusSucceeded:
		out.Progress = &progress
		w, h, ms := ref.Width, ref.Height, ref.DurationMS
		out.Artifacts = []adapter.ArtifactRef{{
			// KindBinary 与 Sora 同形：产物没有 URL，要靠 FetchArtifact 取字节。
			// mock 走这一种正是为了把那条代码路径也真的跑上。
			Kind:       adapter.KindBinary,
			Type:       domain.AssetTypeVideo,
			MIME:       PlaceholderVideoMIME,
			Index:      0,
			Width:      &w,
			Height:     &h,
			DurationMS: &ms,
			Bytes:      int64(len(sampleMP4)),
		}}
	}
	return out, nil
}

// FetchArtifact 把内嵌样片作为字节流交出去。
//
// 用 io.NopCloser 包一个 bytes.Reader 而不是返回 []byte:调用方
// （asset.Transferor）走的是 io.Copy 到存储那条路，接口形态必须与真实驱动
// 一致，否则 mock 验证不到转存的流式实现。
func (d asyncDriver) FetchArtifact(ctx context.Context, ref adapter.ArtifactRef, req adapter.PollRequest) (adapter.ArtifactStream, error) {
	if err := ctx.Err(); err != nil {
		return adapter.ArtifactStream{}, adapter.CallError(domain.TaskErrorInternal, err, "取产物已取消")
	}
	if ref.Kind != adapter.KindBinary {
		return adapter.ArtifactStream{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"mock 只产出 %s 形态的产物，收到 %s", adapter.KindBinary, ref.Kind)
	}
	if err := d.dumpPlaceholder(req.TaskID, ref.Index, "mp4", sampleMP4); err != nil {
		return adapter.ArtifactStream{}, adapter.CallError(domain.TaskErrorInternal, err, "mock 占位视频落盘失败")
	}
	return adapter.ArtifactStream{
		Body:  io.NopCloser(bytes.NewReader(sampleMP4)),
		MIME:  PlaceholderVideoMIME,
		Bytes: int64(len(sampleMP4)),
	}, nil
}

// DefaultPollInterval 返回本驱动的建议轮询间隔。
func (d asyncDriver) DefaultPollInterval() time.Duration { return d.pollInterval() }

// ── 辅助 ───────────────────────────────────────────────────────────

// imageCount 读平台的 count 参数决定出几张图。
// 一次出多张是 mock 必须覆盖的形态：ArtifactRef.Index 决定展示顺序，
// 只出一张的话 TransferAll 的排序逻辑在本地永远走不到。
func imageCount(params map[string]any) int {
	n, ok := intParam(params, "count")
	if !ok || n < 1 {
		return 1
	}
	if n > 8 {
		return 8
	}
	return n
}

// dumpPlaceholder 在配置了 PlaceholderRoot 时额外把产物写一份到磁盘。
//
// 它**不是**产物交付路径（交付走 ArtifactRef），只是给开发者一个
// 不启动前端也能直接双击打开看一眼的出口。没配就什么都不做。
func (d Driver) dumpPlaceholder(taskID string, index int, ext string, data []byte) error {
	if d.PlaceholderRoot == "" {
		return nil
	}
	if err := os.MkdirAll(d.PlaceholderRoot, 0o755); err != nil {
		return fmt.Errorf("创建目录 %s 失败: %w", d.PlaceholderRoot, err)
	}
	name := fmt.Sprintf("%s-%d.%s", sanitizeFilename(taskID), index, ext)
	path := filepath.Join(d.PlaceholderRoot, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return nil
}

// sanitizeFilename 把任务 id 里可能出现的路径分隔符换掉，
// 防止 PlaceholderRoot 之外的地方被写入。
func sanitizeFilename(s string) string {
	if s == "" {
		return "task"
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

var (
	_ adapter.SyncDriver  = syncDriver{}
	_ adapter.AsyncDriver = asyncDriver{}
)
