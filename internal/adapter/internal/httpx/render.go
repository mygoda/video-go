package httpx

import (
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
func RenderBody(r mapping.Renderer, in adapter.SubmitInput) (map[string]any, error) {
	m, err := mapping.DecodeMapping(in.Model.RequestMapping)
	if err != nil {
		return nil, adapter.CallError(domain.TaskErrorInternal, err,
			"模型 %s 的 request_mapping 无法解码", in.Model.ID)
	}
	if r == nil {
		r = mapping.NewRenderer(nil)
	}
	body, err := r.Render(m, RenderContext(in))
	if err != nil {
		// 渲染失败是模型配置写错了，不是用户参数错了——但错误必须能定位到
		// 是哪个模型，否则管理员面对一堆任务失败无从下手。
		return nil, adapter.CallError(domain.TaskErrorInternal, err,
			"模型 %s 的 request_mapping 渲染失败", in.Model.ID)
	}
	return body, nil
}

// RenderContext 把 SubmitInput 转成渲染上下文。
//
// InputURLs 按 InputRef.Slot 分组，且**保持 in.Inputs 的原始顺序**——
// 多张参考图的顺序对生成结果有影响，重排等于悄悄改了用户的输入。
func RenderContext(in adapter.SubmitInput) mapping.RenderContext {
	urls := make(map[string][]string, len(in.Inputs))
	for _, ref := range in.Inputs {
		urls[ref.Slot] = append(urls[ref.Slot], ref.URL)
	}
	return mapping.RenderContext{
		Prompt:        in.Prompt,
		Params:        in.Params,
		UpstreamModel: in.UpstreamModel,
		InputURLs:     urls,
	}
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
