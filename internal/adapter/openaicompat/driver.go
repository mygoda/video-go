package openaicompat

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/adapter/internal/httpx"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// New 返回一个 OpenAI 兼容同步驱动。
//
// family 决定打哪个端点，也决定 Name() 返回什么——一个进程里通常注册两个
// 实例（chat 与 images），二者除 family 外配置相同。
func New(cfg Driver) adapter.SyncDriver {
	if cfg.ProtocolFamily == "" {
		cfg.ProtocolFamily = domain.FamilyImages
	}
	return cfg
}

// Name 返回 Registry 的查找键，取决于本实例服务的是 chat 还是 images。
func (d Driver) Name() string { return string(d.family()) }

// Family 返回本实例的协议族。
func (d Driver) Family() domain.ProtocolFamily { return d.family() }

func (d Driver) family() domain.ProtocolFamily {
	if d.ProtocolFamily == "" {
		return domain.FamilyImages
	}
	return d.ProtocolFamily
}

func (d Driver) path() string {
	if d.family() == domain.FamilyChat {
		return PathChatCompletions
	}
	return PathImagesGenerations
}

func (d Driver) assetType() domain.AssetType {
	if d.family() == domain.FamilyChat {
		return domain.AssetTypeText
	}
	return domain.AssetTypeImage
}

// Invoke 发一次同步请求并把产物放进 SubmitResult.Inline。
//
// UpstreamRef 恒为空：同步族没有后续轮询，L2 据此不会给它排 poll。
func (d Driver) Invoke(ctx context.Context, in adapter.SubmitInput) (adapter.SubmitResult, error) {
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
		// 同步出图同样可能重试（网络抖动导致的超时），带幂等键让上游
		// 不至于因为一次超时重发就计费两次。
		headers["Idempotency-Key"] = in.IdempotencyKey
	}

	var resp response
	url := httpx.URL(in.Provider.BaseURL, d.path())
	if err := httpx.PostJSON(ctx, d.HTTP, url, in.Provider, in.Credential, headers, body, &resp); err != nil {
		return adapter.SubmitResult{}, reclassify(err)
	}

	// 有的兼容实现回 200 但在 body 里带 error 对象。只看状态码会把
	// 一次失败当成成功、再在解析产物时报一个不知所云的"没有产物"。
	if resp.Error != nil {
		return adapter.SubmitResult{
			Status:    normalize(rawError),
			RawStatus: rawError,
		}, upstreamError(http.StatusOK, *resp.Error)
	}

	refs, err := d.artifacts(resp)
	if err != nil {
		return adapter.SubmitResult{}, err
	}

	return adapter.SubmitResult{
		Status:    normalize(rawOK),
		RawStatus: rawOK,
		Inline:    refs,
	}, nil
}

// 上游原始状态串。同步族的"原始状态"是我们自己造的两个值——
// 上游根本不给状态字段，但 tasks.upstream_status_raw 这一列对所有族都要有值，
// 排障时"这条同步任务上游到底成没成"得能从库里看出来。
const (
	rawOK    = "ok"
	rawError = "error"
)

func normalize(raw string) adapter.Status {
	if s, ok := statusMap[raw]; ok {
		return s
	}
	return adapter.StatusFailed
}

// artifacts 把响应体里的产物抽成 ArtifactRef。
//
// images 端点有两种交付形态，由请求里的 response_format 决定:
// url（一个有时效的直链）与 b64_json（内联 base64）。两种都要认，
// 因为 response_format 是 request_mapping 里的一条配置，driver 说了不算。
func (d Driver) artifacts(resp response) ([]adapter.ArtifactRef, error) {
	if d.family() == domain.FamilyChat {
		return d.chatArtifacts(resp)
	}

	if len(resp.Data) == 0 {
		return nil, adapter.CallError(domain.TaskErrorInternal, nil, "上游返回 200 但 data 为空")
	}

	mime := imageMIME(resp.OutputFormat)
	refs := make([]adapter.ArtifactRef, 0, len(resp.Data))
	for i, item := range resp.Data {
		ref := adapter.ArtifactRef{
			Type:  domain.AssetTypeImage,
			MIME:  mime,
			Index: i,
		}
		switch {
		case item.B64JSON != "":
			ref.Kind = adapter.KindBase64
			ref.Base64 = item.B64JSON
			// base64 的字节数可以直接算出来，省得 L3 再解一遍才知道大小。
			ref.Bytes = int64(base64.StdEncoding.DecodedLen(len(item.B64JSON)))
		case item.URL != "":
			ref.Kind = adapter.KindURL
			ref.URL = item.URL
		default:
			return nil, adapter.CallError(domain.TaskErrorInternal, nil,
				"上游第 %d 件产物既没有 url 也没有 b64_json", i)
		}
		if w, h, ok := parseSize(resp.Size); ok {
			ref.Width, ref.Height = &w, &h
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// chatArtifacts 把 chat 的文本回复包成一件 text 产物。
//
// 文本也走 ArtifactRef 而不是另开一条返回通道：L3 的转存、血缘、
// 计费都挂在 asset 上，让文本绕过去意味着 chat 产物没有资产记录、
// 做同款和血缘对它失效。
func (d Driver) chatArtifacts(resp response) ([]adapter.ArtifactRef, error) {
	if len(resp.Choices) == 0 {
		return nil, adapter.CallError(domain.TaskErrorInternal, nil, "上游返回 200 但 choices 为空")
	}
	refs := make([]adapter.ArtifactRef, 0, len(resp.Choices))
	for i, c := range resp.Choices {
		text := c.Message.Content
		if text == "" {
			continue
		}
		refs = append(refs, adapter.ArtifactRef{
			Kind:   adapter.KindBase64,
			Type:   domain.AssetTypeText,
			MIME:   "text/plain; charset=utf-8",
			Base64: base64.StdEncoding.EncodeToString([]byte(text)),
			Index:  i,
			Bytes:  int64(len(text)),
		})
	}
	if len(refs) == 0 {
		return nil, adapter.CallError(domain.TaskErrorInternal, nil, "上游返回的 choices 里没有文本内容")
	}
	return refs, nil
}

func imageMIME(outputFormat string) string {
	switch strings.ToLower(strings.TrimSpace(outputFormat)) {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	}
	return "image/png"
}

// parseSize 解析 "1024x1024" 形态的尺寸串。
func parseSize(size string) (int, int, bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(size)), "x")
	if len(parts) != 2 {
		return 0, 0, false
	}
	var w, h int
	if _, err := fmt.Sscanf(parts[0], "%d", &w); err != nil || w <= 0 {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &h); err != nil || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// DefaultTimeout 是同步出图的建议超时，供 Provider.TimeoutMS 未配置时参考。
// 出图比对话慢得多，10s 的通用超时会把正常请求掐断。
const DefaultTimeout = 120 * time.Second
