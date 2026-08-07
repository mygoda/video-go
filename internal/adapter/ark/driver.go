package ark

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/adapter/internal/httpx"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// New 返回一个 Ark 驱动实例。
func New(cfg Driver) adapter.AsyncDriver { return cfg }

// Name 返回 Registry 的查找键。
func (d Driver) Name() string { return Name }

// Family 返回本驱动所属的协议族。
func (d Driver) Family() domain.ProtocolFamily { return Family }

// DefaultPollInterval 返回建议轮询间隔。
func (d Driver) DefaultPollInterval() time.Duration {
	if d.PollInterval > 0 {
		return d.PollInterval
	}
	return DefaultPollInterval
}

// Submit 提交任务，返回后续轮询用的上游 task id。
func (d Driver) Submit(ctx context.Context, in adapter.SubmitInput) (adapter.SubmitResult, error) {
	if err := httpx.RequireCredential(in.Credential, in.Provider.CredentialRef); err != nil {
		return adapter.SubmitResult{}, err
	}

	body, err := httpx.RenderBody(d.Renderer, in)
	if err != nil {
		return adapter.SubmitResult{}, err
	}
	httpx.EnsureModelField(body, FieldModel, in.UpstreamModel)

	// content 必须存在且非空，否则上游回一个只说"参数错误"的 400。
	// 在这里先拦下来，错误消息能直接指向 request_mapping 少了哪条规则。
	if parts, ok := body[FieldContent].([]any); !ok || len(parts) == 0 {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"模型 %s 的 request_mapping 没有往 %s[] 里填任何内容", in.Model.ID, FieldContent)
	}

	headers := map[string]string{}
	if in.IdempotencyKey != "" {
		headers["X-Client-Request-Id"] = in.IdempotencyKey
	}

	var resp submitResponse
	url := httpx.URL(in.Provider.BaseURL, PathSubmit)
	if err := httpx.PostJSON(ctx, d.HTTP, url, in.Provider, in.Credential, headers, body, &resp); err != nil {
		return adapter.SubmitResult{}, reclassify(err)
	}
	if resp.Error != nil {
		return adapter.SubmitResult{}, upstreamError(0, *resp.Error)
	}
	if resp.ID == "" {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"上游提交成功但没有返回 task id")
	}

	// 提交响应可能已经带一个状态（通常是 queued）。没有就按 queued 处理——
	// 提交成功而状态未知时，队列中是唯一安全的假设。
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

	var resp pollResponse
	url := httpx.URL(req.Provider.BaseURL, fmt.Sprintf(PathPollFormat, req.UpstreamRef))
	if err := httpx.GetJSON(ctx, d.HTTP, url, req.Provider, req.Credential, &resp); err != nil {
		return adapter.PollResult{}, reclassify(err)
	}

	raw := resp.Status
	status := Normalize(raw)
	out := adapter.PollResult{Status: status, RawStatus: raw}

	switch status {
	case adapter.StatusFailed:
		out.Err = failureFor(raw, resp)
	case adapter.StatusSucceeded:
		ref, err := artifactRef(resp)
		if err != nil {
			return adapter.PollResult{}, err
		}
		out.Artifacts = []adapter.ArtifactRef{ref}
		done := 1.0
		out.Progress = &done
	}
	return out, nil
}

// FetchArtifact 下载产物直链。
//
// 这里的 URL **只有 24 小时有效期**（见 ArtifactTTL）。它之所以能直接 GET
// 而不带鉴权，是因为上游给的是一个已签名的直链——但也正因如此它随时会失效，
// 上层必须在拿到它的那一刻就转存。
func (d Driver) FetchArtifact(ctx context.Context, ref adapter.ArtifactRef, req adapter.PollRequest) (adapter.ArtifactStream, error) {
	if ref.Kind != adapter.KindURL {
		return adapter.ArtifactStream{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"ark 只产出 %s 形态的产物，收到 %s", adapter.KindURL, ref.Kind)
	}
	if ref.URL == "" {
		return adapter.ArtifactStream{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"产物引用没有 URL")
	}

	// 直链自带签名，不能再往上叠 Authorization——有的对象存储会因为
	// 同时出现两种鉴权而直接拒绝。这里传一个空 Provider 让 httpx 什么都不加。
	stream, err := httpx.GetStream(ctx, d.HTTP, ref.URL, domain.Provider{}, "", "")
	if err != nil {
		return adapter.ArtifactStream{}, expiredHint(err)
	}
	if stream.MIME == "" {
		stream.MIME = ref.MIME
	}
	return stream, nil
}

// expiredHint 把取产物时的 403/404 翻译成一句能指出根因的话。
//
// 拿到 URL 却下载不下来，绝大多数情况是转存跑在了 24h 过期之后。
// 不加这句提示，排查的人看到的只是一个孤零零的 403，会去查权限配置——
// 而实际该查的是转存为什么延迟了。
func expiredHint(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "403") || strings.Contains(msg, "404") {
		return adapter.CallError(domain.TaskErrorInternal, err,
			"产物直链已失效（Ark 直链有效期 %s，转存必须在任务成功后立刻发生）", ArtifactTTL)
	}
	return err
}

// artifactRef 从成功响应里抽出产物。
func artifactRef(resp pollResponse) (adapter.ArtifactRef, error) {
	url := resp.Content.VideoURL
	if url == "" {
		return adapter.ArtifactRef{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"上游状态为 succeeded 但 content.video_url 为空")
	}

	// 上游不给过期时刻，只有固定的 24h 窗口。用"现在 + TTL"是保守估计
	// （真实起点是任务完成时刻，早于现在），宁可算得偏紧也不能偏松。
	expires := time.Now().Add(ArtifactTTL)
	ref := adapter.ArtifactRef{
		Kind:      adapter.KindURL,
		Type:      domain.AssetTypeVideo,
		MIME:      "video/mp4",
		URL:       url,
		ExpiresAt: &expires,
		Index:     0,
	}
	if resp.Usage.TotalTokens > 0 {
		// 用量不属于产物元信息，不往 ArtifactRef 上挂——这里只是显式说明
		// 我们看到了它但刻意不用，免得以后有人以为漏读了。
		_ = resp.Usage
	}
	return ref, nil
}
