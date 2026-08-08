package httpx

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/adapter/mapping"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// RenderBody 把 ModelConfig.RequestMapping 渲染成上游请求体。
//
// 四个驱动这一步逐字相同：解码配置 → 按 SubmitInput 组渲染上下文 → 渲染。
// 差异（哪个字段叫什么、要不要加后缀）全在配置里，这正是 mapping 包的用意；
// 驱动侧要做的只是把结果套进各自的协议骨架。
//
// **绝不在这里往 body 里塞凭证**：鉴权走 header，渲染结果要能原样回显给
// admin 的 probe 接口看（见 mapping.Renderer 的实现约定）。
func RenderBody(ctx context.Context, r mapping.Renderer, in adapter.SubmitInput) (map[string]any, error) {
	m, err := mapping.DecodeMapping(in.Model.RequestMapping)
	if err != nil {
		return nil, adapter.CallError(domain.TaskErrorInternal, err,
			"模型 %s 的 request_mapping 无法解码", in.Model.ID)
	}
	if r == nil {
		r = mapping.NewRenderer(nil)
	}
	rc, err := RenderContext(ctx, in, m)
	if err != nil {
		return nil, err
	}
	body, err := r.Render(m, rc)
	if err != nil {
		// 渲染失败是模型配置写错了，不是用户参数错了——但错误必须能定位到
		// 是哪个模型，否则管理员面对一堆任务失败无从下手。
		return nil, adapter.CallError(domain.TaskErrorInternal, err,
			"模型 %s 的 request_mapping 渲染失败", in.Model.ID)
	}
	return body, nil
}

// MaxInlineBytes 是单份内联素材的上限。
//
// 内联走 base64，体积再涨三分之一，而整个请求体要在内存里拼一次、
// 在 HTTP 栈里再走一次。没有上限的话，一份用户上传的大文件就能让一次生成
// 把进程内存打穿——而这是用户可控的输入。
const MaxInlineBytes = 8 << 20

// RenderContext 把 SubmitInput 转成渲染上下文。
//
// InputURLs 按 InputRef.Slot 分组，且**保持 in.Inputs 的原始顺序**——
// 多张参考图的顺序对生成结果有影响，重排等于悄悄改了用户的输入。
//
// **只有被 inline 规则引用到的槽才会去读字节。** 读一份素材是一次存储往返
// 加一份内存拷贝，而绝大多数上游给地址就够了；无差别地读会让每一次生成
// 都替一个少数派上游买单。
func RenderContext(ctx context.Context, in adapter.SubmitInput, m mapping.RequestMapping) (mapping.RenderContext, error) {
	urls := make(map[string][]string, len(in.Inputs))
	for _, ref := range in.Inputs {
		urls[ref.Slot] = append(urls[ref.Slot], ref.URL)
	}

	inline := inlineSlots(m)
	var dataURLs map[string][]string
	for _, ref := range in.Inputs {
		if _, want := inline[ref.Slot]; !want {
			continue
		}
		du, err := dataURL(ctx, in.Blobs, ref)
		if err != nil {
			return mapping.RenderContext{}, err
		}
		if dataURLs == nil {
			dataURLs = make(map[string][]string, len(inline))
		}
		dataURLs[ref.Slot] = append(dataURLs[ref.Slot], du)
	}

	return mapping.RenderContext{
		Prompt:        in.Prompt,
		Params:        in.Params,
		UpstreamModel: in.UpstreamModel,
		InputURLs:     urls,
		InputDataURLs: dataURLs,
	}, nil
}

func inlineSlots(m mapping.RequestMapping) map[string]struct{} {
	const prefix = "inputs."
	var slots map[string]struct{}
	for _, rule := range m.Rules {
		if !rule.Inline || !strings.HasPrefix(rule.From, prefix) {
			continue
		}
		if slots == nil {
			slots = map[string]struct{}{}
		}
		slots[strings.TrimPrefix(rule.From, prefix)] = struct{}{}
	}
	return slots
}

func dataURL(ctx context.Context, blobs adapter.BlobReader, ref adapter.InputRef) (string, error) {
	if blobs == nil {
		return "", adapter.CallError(domain.TaskErrorInternal, nil,
			"输入槽 %s 需要内联素材，但没有注入 BlobReader", ref.Slot)
	}
	// 先按登记的大小挡一道，避免为了拒绝一份超大素材而先把它读进内存。
	// 归到 invalid_param 而不是 internal：素材是用户选的、换一张小图就能过，
	// 报成内部错误会让用户以为平台坏了而反复重试同一份大文件。
	if ref.Bytes > MaxInlineBytes {
		return "", adapter.CallError(domain.TaskErrorInvalidParam, nil,
			"输入素材 %d 字节，超过内联上限 %d 字节", ref.Bytes, MaxInlineBytes)
	}
	raw, err := blobs.ReadBlob(ctx, ref.StorageKey)
	if err != nil {
		return "", adapter.CallError(domain.TaskErrorInternal, err,
			"读取输入素材 %s 失败", ref.StorageKey)
	}
	if len(raw) > MaxInlineBytes {
		return "", adapter.CallError(domain.TaskErrorInvalidParam, nil,
			"输入素材 %d 字节，超过内联上限 %d 字节", len(raw), MaxInlineBytes)
	}
	mime := ref.MIME
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

// EnsureModelField 在渲染结果没有携带上游模型名时补上。
//
// 上游模型名是协议骨架的一部分（打哪个模型是端点语义，不是可选参数），
// 但各家放的位置不同：ark / openai 放在 body 的 model 字段，google_lro
// 放在 URL 路径里。放在 body 的那几家如果 request_mapping 忘了写这条规则，
// 上游会回一个语焉不详的 400；这里补一手，让"少写一条 mapping 规则"
// 不至于变成一次线上排查。
func EnsureModelField(body map[string]any, field, upstreamModel string) {
	if upstreamModel == "" {
		return
	}
	if v, ok := body[field]; ok && v != nil && v != "" {
		return
	}
	body[field] = upstreamModel
}
