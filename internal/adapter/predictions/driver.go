package predictions

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/adapter/internal/httpx"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Name 返回 Registry 的查找键。同步与异步两个实例共用它。
func (d Driver) Name() string { return Name }

// Family 返回同步实例的协议族。异步实例覆盖它（见 asyncDriver.Family）。
func (d Driver) Family() domain.ProtocolFamily { return SyncFamily }

func (d Driver) settleInterval() time.Duration {
	if d.SettleInterval > 0 {
		return d.SettleInterval
	}
	return SyncSettleInterval
}

// submit 把一次调用发到 POST /predictions。同步与异步走的是同一个端点、
// 同一个请求体形状，差别只在拿到响应之后怎么处理。
func (d Driver) submit(ctx context.Context, in adapter.SubmitInput) (prediction, error) {
	if err := httpx.RequireCredential(in.Credential, in.Provider.CredentialRef); err != nil {
		return prediction{}, err
	}

	body, err := httpx.RenderBody(ctx, d.Renderer, in)
	if err != nil {
		return prediction{}, err
	}
	httpx.EnsureModelField(body, FieldModel, in.UpstreamModel)

	// 全部生成参数都在 input 里。input 为空意味着连 prompt 都没渲染进去——
	// 上游对此只回一句"参数错误"，在这里先拦下来，错误消息能直接指向
	// request_mapping 少了哪条规则。
	if input, ok := body[FieldInput].(map[string]any); !ok || len(input) == 0 {
		return prediction{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"模型 %s 的 request_mapping 没有往 %s 里填任何字段", in.Model.ID, FieldInput)
	}

	headers := map[string]string{}
	if in.IdempotencyKey != "" {
		headers["X-Client-Request-Id"] = in.IdempotencyKey
	}

	var resp prediction
	url := httpx.URL(in.Provider.BaseURL, PathSubmit)
	if err := httpx.PostJSON(ctx, d.HTTP, url, in.Provider, in.Credential, headers, body, &resp); err != nil {
		return prediction{}, reclassify(err)
	}
	return resp, nil
}

// get 查一次 GET /predictions/{id}。它只读，不写任何状态——这是 Poll 幂等的来源。
func (d Driver) get(ctx context.Context, p domain.Provider, credential, ref string) (prediction, error) {
	var resp prediction
	url := httpx.URL(p.BaseURL, fmt.Sprintf(PathPollFormat, ref))
	if err := httpx.GetJSON(ctx, d.HTTP, url, p, credential, &resp); err != nil {
		return prediction{}, reclassify(err)
	}
	return resp, nil
}

// artifactRefs 把 output 里的直链变成产物引用。
//
// typ 由调用方给：同一个字段在出图实例上是图、在出视频实例上是视频，
// 协议本身不区分——它只回一堆 URL。
func artifactRefs(resp prediction, typ domain.AssetType) ([]adapter.ArtifactRef, error) {
	urls, err := ParseOutput(resp.Output)
	if err != nil {
		return nil, adapter.CallError(domain.TaskErrorInternal, err, "上游 output 形态无法识别")
	}
	if len(urls) == 0 {
		return nil, adapter.CallError(domain.TaskErrorInternal, nil,
			"上游状态为 %s 但 output 里没有任何产物地址", statusSucceeded)
	}

	refs := make([]adapter.ArtifactRef, 0, len(urls))
	for i, u := range urls {
		// 上游不给过期时刻，只有固定的 24h 窗口。用"现在 + TTL"是保守估计
		// （真实起点是任务完成时刻，早于现在），宁可算得偏紧也不能偏松。
		expires := time.Now().Add(ArtifactTTL)
		mime := "video/mp4"
		if typ == domain.AssetTypeImage {
			mime = imageMIME(u)
		}
		refs = append(refs, adapter.ArtifactRef{
			Kind:      adapter.KindURL,
			Type:      typ,
			MIME:      mime,
			URL:       u,
			ExpiresAt: &expires,
			Index:     i,
		})
	}
	return refs, nil
}

// fetchURL 下载一个已签名的产物直链。
//
// 直链自带签名，不能再往上叠 Authorization——有的对象存储会因为
// 同时出现两种鉴权而直接拒绝。这里传一个空 Provider 让 httpx 什么都不加。
func (d Driver) fetchURL(ctx context.Context, ref adapter.ArtifactRef) (adapter.ArtifactStream, error) {
	if ref.Kind != adapter.KindURL {
		return adapter.ArtifactStream{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"predictions 只产出 %s 形态的产物，收到 %s", adapter.KindURL, ref.Kind)
	}
	if ref.URL == "" {
		return adapter.ArtifactStream{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"产物引用没有 URL")
	}

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
			"产物直链已失效（有效期 %s，转存必须在任务成功后立刻发生）", ArtifactTTL)
	}
	return err
}

// terminalError 把一个终态失败的响应变成 error 形态。
//
// 同步族没有轮询这一步，失败**必须**以 error 抛出：返回一个
// Status=failed 的 SubmitResult 而不带 error，L2 会当成一次没有产物的成功。
func terminalError(resp prediction) error {
	fail := failureFor(resp)
	return adapter.CallError(fail.Code, nil, "%s", fail.Message)
}

// ── 同步实例（对应 protocol_family='predictions' 的图像模型）────────────

type syncDriver struct{ Driver }

// NewSyncDriver 返回同步实例，出图走它。
func NewSyncDriver(cfg Driver) adapter.SyncDriver { return syncDriver{Driver: cfg} }

// Invoke 提交并等到终态，一次调用就把产物交出来。
//
// 之所以要"等"而不是提交完就返回：这个协议本身是异步形状的，上游有权
// 先回一个 processing（实测出图约 10 秒，多数时候一次往返就是终态，但不保证）。
// 而同步族的 UpstreamRef 恒为空，L2 不会为它安排任何一次轮询——
// 这里不等，任务就永远停在 running。
//
// 等待的上限是 ctx 的截止时间，即 Provider.TimeoutMS。
func (d syncDriver) Invoke(ctx context.Context, in adapter.SubmitInput) (adapter.SubmitResult, error) {
	resp, err := d.submit(ctx, in)
	if err != nil {
		return adapter.SubmitResult{}, err
	}

	for !terminal(Normalize(resp.Status)) {
		if resp.ID == "" {
			return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, nil,
				"上游回了非终态 %q 却没有给 prediction id，无从继续查询", resp.Status)
		}
		select {
		case <-ctx.Done():
			return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, ctx.Err(),
				"等待上游出图超时，最后状态 %q", resp.Status)
		case <-time.After(d.settleInterval()):
		}
		if resp, err = d.get(ctx, in.Provider, in.Credential, resp.ID); err != nil {
			return adapter.SubmitResult{}, err
		}
	}

	if Normalize(resp.Status) != adapter.StatusSucceeded {
		return adapter.SubmitResult{}, terminalError(resp)
	}

	refs, err := artifactRefs(resp, domain.AssetTypeImage)
	if err != nil {
		return adapter.SubmitResult{}, err
	}
	// 同步族的 UpstreamRef 恒为空，L2 据此不为它安排轮询。
	return adapter.SubmitResult{
		Status:    adapter.StatusSucceeded,
		RawStatus: resp.Status,
		Inline:    refs,
	}, nil
}

func terminal(s adapter.Status) bool {
	return s == adapter.StatusSucceeded || s == adapter.StatusFailed || s == adapter.StatusCanceled
}

// ── 异步实例（对应 protocol_family='video' + video_protocol='predictions'）──

type asyncDriver struct{ Driver }

// NewAsyncDriver 返回异步实例，出视频走它。
func NewAsyncDriver(cfg Driver) adapter.AsyncDriver { return asyncDriver{Driver: cfg} }

// Family 返回 video——见 AsyncFamily 的注释。
func (d asyncDriver) Family() domain.ProtocolFamily { return AsyncFamily }

// DefaultPollInterval 返回建议轮询间隔。
func (d asyncDriver) DefaultPollInterval() time.Duration {
	if d.PollInterval > 0 {
		return d.PollInterval
	}
	return DefaultPollInterval
}

// Submit 提交任务，返回后续轮询用的上游 prediction id。
func (d asyncDriver) Submit(ctx context.Context, in adapter.SubmitInput) (adapter.SubmitResult, error) {
	resp, err := d.submit(ctx, in)
	if err != nil {
		return adapter.SubmitResult{}, err
	}

	// 提交这一次就直接回终态失败是有的（实测 duration 非法即如此）。
	// 此时必须以 error 抛出：给一个 id 让 L2 去轮询，轮到的还是这同一句错。
	if Normalize(resp.Status) == adapter.StatusFailed {
		return adapter.SubmitResult{}, terminalError(resp)
	}
	if resp.ID == "" {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"上游提交成功但没有返回 prediction id")
	}

	// 提交响应可能不带状态。没有就按 starting 处理——提交成功而状态未知时，
	// 队列中是唯一安全的假设。
	raw := resp.Status
	if raw == "" {
		raw = statusStarting
	}
	return adapter.SubmitResult{
		UpstreamRef: resp.ID,
		Status:      Normalize(raw),
		RawStatus:   raw,
	}, nil
}

// Poll 查询一次任务状态。
//
// 它只发一个 GET、不写任何状态，因此天然幂等——webhook 与定时轮询
// 是推进同一个状态机的两条同源路径，任何一条被调用多次结果都一致。
func (d asyncDriver) Poll(ctx context.Context, req adapter.PollRequest) (adapter.PollResult, error) {
	if err := httpx.RequireCredential(req.Credential, req.Provider.CredentialRef); err != nil {
		return adapter.PollResult{}, err
	}
	if req.UpstreamRef == "" {
		return adapter.PollResult{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"轮询缺少 upstream_ref")
	}

	resp, err := d.get(ctx, req.Provider, req.Credential, req.UpstreamRef)
	if err != nil {
		return adapter.PollResult{}, err
	}

	raw := resp.Status
	status := Normalize(raw)
	out := adapter.PollResult{Status: status, RawStatus: raw}

	switch status {
	case adapter.StatusFailed:
		out.Err = failureFor(resp)
	case adapter.StatusSucceeded:
		refs, err := artifactRefs(resp, domain.AssetTypeVideo)
		if err != nil {
			return adapter.PollResult{}, err
		}
		out.Artifacts = refs
		done := 1.0
		out.Progress = &done
	}
	return out, nil
}

// FetchArtifact 下载产物直链。两种模态都是一次不带鉴权的 GET。
func (d asyncDriver) FetchArtifact(ctx context.Context, ref adapter.ArtifactRef, req adapter.PollRequest) (adapter.ArtifactStream, error) {
	return d.fetchURL(ctx, ref)
}

var (
	_ adapter.SyncDriver  = syncDriver{}
	_ adapter.AsyncDriver = asyncDriver{}
)
