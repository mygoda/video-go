package googlelro

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/adapter/internal/httpx"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// New 返回一个 Google LRO 驱动实例。
func New(cfg Driver) adapter.AsyncDriver { return cfg }

// Name 返回 Registry 的查找键。
func (d Driver) Name() string { return Name }

// Family 返回本驱动所属的协议族。
func (d Driver) Family() domain.ProtocolFamily { return Family }

// DefaultPollInterval 返回建议轮询间隔。
//
// 固定而不退避：本协议**不给任何进度信息**，退避只会让终态被发现得更晚，
// 却换不来任何额外信息（有进度的协议退避还能少读几次重复的百分比）。
func (d Driver) DefaultPollInterval() time.Duration {
	if d.PollInterval > 0 {
		return d.PollInterval
	}
	return DefaultPollInterval
}

// Submit 提交任务，返回后续轮询用的 **operation name**。
//
// 注意返回的 UpstreamRef 不是一个 id 而是一个完整的相对路径
// （形如 models/xxx/operations/yyy）。上层把它当不透明字符串存进
// tasks.upstream_ref 再原样传回来，绝不解析——这是 adapter.SubmitResult.UpstreamRef
// 被定义为不透明串的直接原因。
func (d Driver) Submit(ctx context.Context, in adapter.SubmitInput) (adapter.SubmitResult, error) {
	if err := httpx.RequireCredential(in.Credential, in.Provider.CredentialRef); err != nil {
		return adapter.SubmitResult{}, err
	}
	if in.UpstreamModel == "" {
		// 本协议的模型名在 URL 路径里，不在 body 里，因此不能像另外两家那样
		// 靠 request_mapping 兜底——缺了就根本拼不出端点。
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"模型 %s 没有配置 upstream_model，本协议的模型名在 URL 路径里，必填", in.Model.ID)
	}

	body, err := httpx.RenderBody(d.Renderer, in)
	if err != nil {
		return adapter.SubmitResult{}, err
	}

	var resp operation
	url := httpx.URL(in.Provider.BaseURL, fmt.Sprintf(PathSubmitFormat, in.UpstreamModel))
	if err := httpx.PostJSON(ctx, d.HTTP, url, in.Provider, in.Credential, nil, body, &resp); err != nil {
		return adapter.SubmitResult{}, reclassify(err)
	}
	if resp.Error != nil {
		return adapter.SubmitResult{}, upstreamError(0, *resp.Error)
	}
	if resp.Name == "" {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"上游提交成功但没有返回 %s（operation name）", FieldOperationName)
	}

	// 提交刚返回时 done 必为 false。归一化成 queued 而不是 running:
	// 刚提交还没开始跑，报 running 会让前端的"排队中"这一态永远显示不出来。
	return adapter.SubmitResult{
		UpstreamRef: resp.Name,
		Status:      adapter.StatusQueued,
		RawStatus:   RawStatus(false),
	}, nil
}

// Poll 查询一次 operation 状态。
//
// 轮询地址由**响应体决定**（operation name 拼在 base_url 后面），
// 不是把一个 id 塞进固定模板——这是本协议与另外两家最根本的差异。
func (d Driver) Poll(ctx context.Context, req adapter.PollRequest) (adapter.PollResult, error) {
	if err := httpx.RequireCredential(req.Credential, req.Provider.CredentialRef); err != nil {
		return adapter.PollResult{}, err
	}
	if req.UpstreamRef == "" {
		return adapter.PollResult{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"轮询缺少 upstream_ref（本协议里它是 operation name）")
	}

	var resp operation
	url := httpx.URL(req.Provider.BaseURL, fmt.Sprintf(PathPollFormat, strings.TrimLeft(req.UpstreamRef, "/")))
	if err := httpx.GetJSON(ctx, d.HTTP, url, req.Provider, req.Credential, &resp); err != nil {
		return adapter.PollResult{}, reclassify(err)
	}

	raw := RawStatus(resp.Done)
	if !resp.Done {
		// done=false 一律 running。本协议不区分排队与执行中，
		// 硬造一个区分出来只会是猜的。
		return adapter.PollResult{Status: Normalize(raw), RawStatus: raw}, nil
	}

	// done=true 之后还要看 error 字段才能定成功还是失败——
	// 这一步刻意不在 statusMap 里：那张表只表达"done 这个信号本身"的含义。
	if resp.Error != nil {
		return adapter.PollResult{
			Status:    adapter.StatusFailed,
			RawStatus: raw,
			Err:       failureFor(*resp.Error),
		}, nil
	}

	refs, err := artifactRefs(resp)
	if err != nil {
		return adapter.PollResult{}, err
	}
	done := 1.0
	return adapter.PollResult{
		Status:    adapter.StatusSucceeded,
		RawStatus: raw,
		Progress:  &done,
		Artifacts: refs,
	}, nil
}

// FetchArtifact 把两种产物形态都变成一个流。
//
//	KindBase64  本地解码，不打网络
//	KindGCSURI  用 Google 凭证再取一次
//
// 上层（asset.Transferor）只看到一个 io.ReadCloser，不知道这两种的存在。
func (d Driver) FetchArtifact(ctx context.Context, ref adapter.ArtifactRef, req adapter.PollRequest) (adapter.ArtifactStream, error) {
	switch ref.Kind {
	case adapter.KindBase64:
		raw, err := base64.StdEncoding.DecodeString(ref.Base64)
		if err != nil {
			return adapter.ArtifactStream{}, adapter.CallError(domain.TaskErrorInternal, err,
				"内联产物 base64 解码失败")
		}
		return adapter.ArtifactStream{
			Body:  io.NopCloser(strings.NewReader(string(raw))),
			MIME:  ref.MIME,
			Bytes: int64(len(raw)),
		}, nil

	case adapter.KindGCSURI:
		if err := httpx.RequireCredential(req.Credential, req.Provider.CredentialRef); err != nil {
			return adapter.ArtifactStream{}, err
		}
		url, err := gcsDownloadURL(ref.GCSURI)
		if err != nil {
			return adapter.ArtifactStream{}, err
		}
		// GCS 的 JSON API 与生成端点用同一套 Bearer 凭证，
		// 因此把 Provider 原样传下去让 httpx 按它的 AuthStyle 注入。
		stream, err := httpx.GetStream(ctx, d.HTTP, url, req.Provider, req.Credential, "")
		if err != nil {
			return adapter.ArtifactStream{}, reclassify(err)
		}
		if stream.MIME == "" {
			stream.MIME = ref.MIME
		}
		return stream, nil
	}

	return adapter.ArtifactStream{}, adapter.CallError(domain.TaskErrorInternal, nil,
		"google_lro 只产出 %s / %s 形态的产物，收到 %s", adapter.KindBase64, adapter.KindGCSURI, ref.Kind)
}

// GCSDownloadHost 是 GCS 对象下载端点的主机。
const GCSDownloadHost = "https://storage.googleapis.com"

// gcsDownloadURL 把 gs://bucket/object 变成一个可 GET 的 HTTPS 地址。
//
// object 部分要整体转义（它可能含斜杠与中文），因此不能简单地拼字符串；
// 用 JSON API 的 media 下载形式而不是 XML API，是因为前者与生成端点
// 共用同一套 Bearer 凭证，不需要再引入一套签名逻辑。
func gcsDownloadURL(uri string) (string, error) {
	if !strings.HasPrefix(uri, GCSURIScheme) {
		return "", adapter.CallError(domain.TaskErrorInternal, nil,
			"产物 URI %q 不是 %s 形态", uri, GCSURIScheme)
	}
	rest := strings.TrimPrefix(uri, GCSURIScheme)
	bucket, object, ok := strings.Cut(rest, "/")
	if !ok || bucket == "" || object == "" {
		return "", adapter.CallError(domain.TaskErrorInternal, nil,
			"GCS URI %q 缺少 bucket 或 object", uri)
	}
	return fmt.Sprintf("%s/storage/v1/b/%s/o/%s?alt=media",
		GCSDownloadHost, escapePathSegment(bucket), escapePathSegment(object)), nil
}
