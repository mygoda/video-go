// Package mapping declares the declarative platform-params → upstream-body
// translation used by every driver.
//
// # 这是「接一个走已知协议的新模型 = 一条配置，零代码」这条判定线的承载者。
//
// driver 只负责协议骨架：端点长什么样、凭证怎么放、轮询怎么寻址、产物怎么取。
// 剩下的全部差异——同一个协议下 A 模型把分辨率叫 resolution 而 B 叫 size、
// A 收 "1080p" 而 B 收 "1920x1080"、A 的时长是数字而 B 是 "5s" 字符串——
// 都落在 RequestMapping 这份配置里。
//
// 没有它，每接一个新模型都要回 driver 里加一段 if；有了它，新模型是
// admin 后台的一条记录。这就是「零代码」与「每次改代码」的分界线。
//
// # 规则顺序即数组顺序
//
// 追加进多模态数组的规则（To 以 "[]" 结尾）**按 Rules 的先后顺序入数组**。
// 这不是实现细节：Ark 的 content 数组里 text/image/video 各部分的**顺序影响生成语义**
// （首帧图在前和在后是两个意思），所以规则顺序必须被当作契约的一部分保持稳定。
package mapping

import "github.com/aigc-pool/aigc-pool/internal/capability"

// Wrap 是把值追加进多模态数组时的包裹形态。
//
// 上游的 content 数组元素不是裸值，而是 {"type":"text","text":"..."} /
// {"type":"image_url","image_url":{"url":"..."}} 这类带类型标签的对象。
// Wrap 声明用哪种壳子包，避免为每种壳子写一条 body_template。
type Wrap string

const (
	WrapRaw          Wrap = "raw"
	WrapTextPart     Wrap = "text_part"
	WrapImageURLPart Wrap = "image_url_part"
	WrapVideoURLPart Wrap = "video_url_part"

	// WrapUserMessage 包出 {"role":"user","content":"..."}，用于 chat 协议的 messages 数组。
	//
	// 没有它，一个合法的 chat 请求体在本包里根本表达不出来：To 的 "[]" 只能出现在
	// 末尾，descend 也只下钻对象，因此 "messages[0].content" 这种路径无法寻址。
	// 缺这一个壳子的代价是每接一个 chat 模型都要回 driver 里拼 messages，
	// 那正是本包要消灭的东西。
	WrapUserMessage Wrap = "user_message"
)

// Cast 是写入上游请求体前的类型转换。
//
// CastSecondsSuffix 把数值 5 变成字符串 "5s"——这类"只差一个后缀"的差异如果不做成
// 配置，就会变成 driver 里针对某个模型的一行 if，而那正是本包要消灭的东西。
type Cast string

const (
	CastString        Cast = "string"
	CastInt           Cast = "int"
	CastFloat         Cast = "float"
	CastBool          Cast = "bool"
	CastSecondsSuffix Cast = "seconds_suffix"
)

// MappingRule 是一条取值-赋值规则。
type MappingRule struct {
	// From 是取值源，四种前缀:
	//   prompt                 用户提示词
	//   params.<key>           某个平台参数（key 为 ParamSpec.Key，compound 已平铺）
	//   inputs.<slot>          某个输入槽（值为该槽的素材，配合 Wrap 用）
	//   model.upstream_model   ModelConfig.UpstreamModel
	// From 与 Const 二选一。
	From string `json:"from,omitempty"`

	// Const 是字面量，用于往上游请求体里塞固定值（如某模型必须带的 watermark:false）。
	Const any `json:"const,omitempty"`

	// To 是上游请求体中的目标路径，点号分隔；以 "[]" 结尾表示**追加进数组**。
	// 例: "parameters.resolution"、"content[]"。
	To string `json:"to"`

	// ValueMap 是平台取值 → 上游取值的映射，例 {"768p": "720p"}。
	// 它让平台对外的参数取值域保持统一，而各家上游各叫各的。
	ValueMap map[string]any `json:"value_map,omitempty"`

	// Wrap 仅在 To 以 "[]" 结尾时有意义。
	Wrap Wrap `json:"wrap,omitempty"`

	// Cast 在写入前做类型转换。
	Cast Cast `json:"cast,omitempty"`

	// When 为假时跳过本条规则。它复用 L1 的 Condition，与参数可见性、计价
	// 用的是同一套求值语义——同一个 has_input 条件既控制前端是否渲染重绘强度、
	// 又控制是否往上游请求体里塞 strength，两处不会分叉。
	When *capability.Condition `json:"when,omitempty"`

	// OmitWhenEmpty 为 true（默认）时，取到空值就不写这个字段，
	// 而不是往上游塞一个 null——不少上游对显式 null 和字段缺失的处理不一样。
	OmitWhenEmpty *bool `json:"omit_when_empty,omitempty"`
}

// RequestMapping 是一个模型的完整请求映射配置，存在 ModelConfig.RequestMapping 里。
type RequestMapping struct {
	// BodyTemplate 是上游请求体的静态骨架，Rules 渲染在它之上。
	// 骨架里放的是与参数无关的固定结构，Rules 负责把动态值填进去。
	BodyTemplate map[string]any `json:"body_template,omitempty"`
	Rules        []MappingRule  `json:"rules,omitempty"`
}

// RenderContext 是一次渲染的输入，与 capability.EvalContext 的内容一致再加上
// 提示词、上游模型名与输入槽的可访问地址。
type RenderContext struct {
	Prompt        string
	Params        map[string]any
	UpstreamModel string
	// InputURLs 的 key 是 InputSlotSpec.Key，值是该槽素材的可访问地址，
	// 供 inputs.<slot> 取值。
	InputURLs map[string][]string
}

// Renderer 把 RequestMapping 渲染成上游请求体。
//
// 实现约定:
//   - 从 BodyTemplate 的深拷贝开始，绝不就地修改配置对象（同一份 ModelConfig
//     会被并发的多个任务共用）；
//   - 严格按 Rules 顺序应用，数组追加的顺序即规则顺序（见包注释，Ark 依赖它）；
//   - When 求值用与 L1 同一个 capability.Evaluator，不另起一套；
//   - 渲染结果**不含任何凭证**——鉴权由 driver 放进 header，不经过 mapping。
//     这条保证了 admin 的 probe 接口可以把 RenderedRequest 原样回显给管理员看。
type Renderer interface {
	Render(m RequestMapping, ctx RenderContext) (map[string]any, error)
}
