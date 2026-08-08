package openaivideo

import (
	"context"
	"fmt"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/adapter/internal/httpx"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// New 返回一个 OpenAI video 驱动实例。
func New(cfg Driver) adapter.AsyncDriver { return cfg }

// Name 返回 Registry 的查找键。
func (d Driver) Name() string { return Name }

// Family 返回本驱动所属的协议族。
func (d Driver) Family() domain.ProtocolFamily { return Family }

// DefaultPollInterval 返回建议的起始轮询间隔。
func (d Driver) DefaultPollInterval() time.Duration {
	if d.PollInterval > 0 {
		return d.PollInterval
	}
	return DefaultPollInterval
}

// MaxPollInterval 是指数退避的上限。
//
// 退避的理由不是省流量，是省配额：视频要跑几分钟，前 30 秒每 15 秒问一次
// 与第 5 分钟每 15 秒问一次，信息量完全不同，而后者会白白吃掉上游的 QPS 额度。
const MaxPollInterval = 30 * time.Second

// PollIntervalAt 按轮询次数算出本次该等多久（指数退避，封顶 MaxPollInterval）。
//
// 它导出是因为 L2 的 PollScheduler 要用它算 next_poll_at——
// "这个协议怎么退避"属于协议知识，必须留在驱动里，
// 让 L2 自己写一套退避就等于把协议差异搬到了 L0 之上。
func (d Driver) PollIntervalAt(attempt int) time.Duration {
	base := d.DefaultPollInterval()
	interval := base
	for i := 0; i < attempt && interval < MaxPollInterval; i++ {
		interval *= 2
	}
	if interval > MaxPollInterval {
		return MaxPollInterval
	}
	return interval
}

// Submit 提交任务，返回后续轮询用的 video id。
func (d Driver) Submit(ctx context.Context, in adapter.SubmitInput) (adapter.SubmitResult, error) {
	if err := httpx.RequireCredential(in.Credential, in.Provider.CredentialRef); err != nil {
		return adapter.SubmitResult{}, err
	}

	body, err := httpx.RenderBody(ctx, d.Renderer, in)
	if err != nil {
		return adapter.SubmitResult{}, err
	}
	httpx.EnsureModelField(body, "model", in.UpstreamModel)

	headers := map[string]string{}
	if in.IdempotencyKey != "" {
		// 视频提交是分钟级且计费不菲，重发一次就是重复计费。
		// 这个协议原生支持 Idempotency-Key，必须带上。
		headers["Idempotency-Key"] = in.IdempotencyKey
	}

	var resp videoResponse
	url := httpx.URL(in.Provider.BaseURL, PathSubmit)
	if err := httpx.PostJSON(ctx, d.HTTP, url, in.Provider, in.Credential, headers, body, &resp); err != nil {
		return adapter.SubmitResult{}, reclassify(err)
	}
	if resp.Error != nil {
		return adapter.SubmitResult{}, upstreamError(0, *resp.Error)
	}
	if resp.ID == "" {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"上游提交成功但没有返回 video id")
	}

	raw := resp.Status
	if raw == "" {
		raw = statusQueued
	}
	return adapter.SubmitResult{
		UpstreamRef: resp.ID,
		Status:      Normalize(raw),
		RawStatus:   raw,
	}, nil
}

// Poll 查询一次任务状态。
func (d Driver) Poll(ctx context.Context, req adapter.PollRequest) (adapter.PollResult, error) {
	if err := httpx.RequireCredential(req.Credential, req.Provider.CredentialRef); err != nil {
		return adapter.PollResult{}, err
	}
	if req.UpstreamRef == "" {
		return adapter.PollResult{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"轮询缺少 upstream_ref")
	}

	var resp videoResponse
	url := httpx.URL(req.Provider.BaseURL, fmt.Sprintf(PathPollFormat, req.UpstreamRef))
	if err := httpx.GetJSON(ctx, d.HTTP, url, req.Provider, req.Credential, &resp); err != nil {
		return adapter.PollResult{}, reclassify(err)
	}

	raw := resp.Status
	status := Normalize(raw)
	out := adapter.PollResult{Status: status, RawStatus: raw}

	if resp.Progress > 0 {
		p := float64(resp.Progress) / 100
		out.Progress = &p
	}

	switch status {
	case adapter.StatusFailed:
		out.Err = failureFor(resp)
	case adapter.StatusSucceeded:
		out.Artifacts = []adapter.ArtifactRef{d.artifactRef(req.UpstreamRef, resp)}
		done := 1.0
		out.Progress = &done
	}
	return out, nil
}

// artifactRef 造出取产物用的引用。
//
// **它没有 URL**：这个协议的产物要打 /content 端点、带鉴权、流式拿裸字节。
// 因此 Kind 是 KindBinary，寻址信息（video id）放在 PollRequest.UpstreamRef 里，
// FetchArtifact 从那里取——这正是 adapter.ArtifactRef 必须有 KindBinary
// 这个形态的原因（见包注释）。
func (d Driver) artifactRef(videoID string, resp videoResponse) adapter.ArtifactRef {
	ref := adapter.ArtifactRef{
		Kind:  adapter.KindBinary,
		Type:  domain.AssetTypeVideo,
		MIME:  ArtifactMIME,
		Index: 0,
	}
	if resp.ExpiresAt > 0 {
		// 与 Ark 的固定 24h 不同，这里是上游给的显式时刻，照抄即可。
		t := time.Unix(resp.ExpiresAt, 0).UTC()
		ref.ExpiresAt = &t
	}
	if w, h, ok := parseSize(resp.Size); ok {
		ref.Width, ref.Height = &w, &h
	}
	if resp.Seconds > 0 {
		ms := int(resp.Seconds * 1000)
		ref.DurationMS = &ms
	}
	return ref
}

// FetchArtifact 带鉴权流式拉取 mp4 裸字节。
//
// 必须流式：一段视频几十上百 MB，读进内存再落盘几个并发就能把进程撑爆。
// httpx.GetStream 直接把 resp.Body 交出去，调用方 io.Copy 到存储。
func (d Driver) FetchArtifact(ctx context.Context, ref adapter.ArtifactRef, req adapter.PollRequest) (adapter.ArtifactStream, error) {
	if ref.Kind != adapter.KindBinary {
		return adapter.ArtifactStream{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"openai_video 只产出 %s 形态的产物，收到 %s", adapter.KindBinary, ref.Kind)
	}
	if err := httpx.RequireCredential(req.Credential, req.Provider.CredentialRef); err != nil {
		return adapter.ArtifactStream{}, err
	}
	if req.UpstreamRef == "" {
		return adapter.ArtifactStream{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"取产物缺少 upstream_ref（本协议靠它拼 content 端点）")
	}

	url := httpx.URL(req.Provider.BaseURL, fmt.Sprintf(PathContentFormat, req.UpstreamRef))
	stream, err := httpx.GetStream(ctx, d.HTTP, url, req.Provider, req.Credential, ArtifactMIME)
	if err != nil {
		return adapter.ArtifactStream{}, reclassify(err)
	}
	if stream.MIME == "" {
		stream.MIME = ArtifactMIME
	}
	return stream, nil
}
