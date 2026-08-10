// Package capability is the L1 capability-declaration layer: the Go mirror of
// docs/contracts/capability-schema.ts (and of the ModelCapabilitySchema family
// in docs/openapi.yaml, which is itself that file's transport-layer restatement).
//
// L1 存在的意义是把「这个模型能调什么参数、多少钱、多久出」变成**数据**而不是代码。
// 接一个走已知协议的新模型 = 后台加一条带 ModelCapabilitySchema 的配置记录，
// 前端零改动、后端零发版。任何把某个具体模型的参数写死在 Go 代码里的做法，
// 都是在把 L1 变回硬编码分支。
//
// # 关于 ParamSpec：一个刻意的取舍
//
// TypeScript 侧 ParamSpec 是判别联合（SelectSpec | AspectSelectSpec | ...），
// openapi 侧是带 discriminator 的 oneOf。**Go 没有 sum type**，硬要模拟只有三条路：
// 每个变体一个接口实现（丢失 JSON 直接反序列化的能力）、每个变体一个字段的
// 大结构体（更啰嗦且同样不封闭）、或者一个结构体带判别字段。
//
// 这里选第三条：**唯一一个 ParamSpec 结构体，携带 Control 判别字段 + 全部变体
// 字段的超集**，变体独有的字段声明为可选（指针或 omitempty）。代价是类型系统
// 不再阻止你给 toggle 填 Options——这个约束改由 Validator 在运行时保证。
//
// 这是**明确的取舍，不是疏漏**。选它的理由是它保持了与 TS/JSON 的一一对应：
// 一份 capability JSON 从数据库读出来，encoding/json 直接就能 Unmarshal 成
// ParamSpec，不需要手写 UnmarshalJSON 按 control 分派，也就不存在「TS 加了个
// 变体、Go 侧的分派函数忘了加分支」这类静默漂移。契约同步的成本比类型安全更值钱。
//
// # 两端一致性
//
// L1 的 Evaluator 与 Pricer 在语义上必须与前端的同名纯函数**逐位一致**。
// 前端本地实时算价用于展示，服务端算价用于扣费；两者一旦有偏差，
// 用户看到的价和实际扣的钱就对不上，这是产品级事故而不是显示瑕疵。
package capability

import "github.com/aigc-pool/aigc-pool/internal/domain"

// ControlKind 是控件类型。这 8 种覆盖了实测的全部参数芯片。
//
// **新增枚举值 = 一次前端改动**，因此新增前先确认现有控件真的表达不了。
// 前端遇到未知 control 必须降级渲染，不允许白屏。
type ControlKind string

const (
	ControlSelect       ControlKind = "select"
	ControlAspectSelect ControlKind = "aspect_select"
	ControlCompound     ControlKind = "compound"
	ControlStepper      ControlKind = "stepper"
	ControlSlider       ControlKind = "slider"
	ControlToggle       ControlKind = "toggle"
	ControlSeed         ControlKind = "seed"
	ControlTextarea     ControlKind = "textarea"
)

// ParamGroup 决定参数渲染在哪里：primary 进主参数芯片条，advanced 收进 ⚙ 折叠面板。
type ParamGroup string

const (
	GroupPrimary  ParamGroup = "primary"
	GroupAdvanced ParamGroup = "advanced"
)

// RenderHint 是 select 的渲染建议：选项 ≤ 4 时前端默认渲染成一排分段按钮，
// 否则渲染成下拉；后端可用本字段强制指定。
type RenderHint string

const (
	RenderDropdown  RenderHint = "dropdown"
	RenderSegmented RenderHint = "segmented"
)

// PixelSize 是像素尺寸约束。
type PixelSize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// InputSlotSpec 是一个输入槽的声明。
//
// 输入槽决定「传了图走图生图 / 没传走文生」这条**隐式分流**：前端不发 mode 字段，
// 只发 prompt + inputs，由服务端根据 inputs 是否为空路由。
// Required=false 的槽 = 可选输入 = 隐式分流点。
type InputSlotSpec struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Kind     string `json:"kind"` // image | video | audio
	Required bool   `json:"required"`
	MinCount int    `json:"min_count"`
	MaxCount int    `json:"max_count"`
	// Accept 是 MIME 白名单，直接喂给 <input accept> 并在前端预校验。
	Accept    []string   `json:"accept"`
	MaxBytes  int64      `json:"max_bytes"`
	MinPixels *PixelSize `json:"min_pixels,omitempty"`
	MaxPixels *PixelSize `json:"max_pixels,omitempty"`
	Hint      string     `json:"hint,omitempty"`

	// RequiresSlot 声明本槽只有在另一个槽也有素材时才成立，空串表示无依赖。
	//
	// 它存在是因为有些上游把多个语义槽压成**一个按位置定序的数组**：Seedance 的
	// images[] 收 1~2 张，第 1 张是首帧、第 2 张是尾帧，没有任何字段能表达
	// 「只给尾帧」。那种提交渲染出来是 images[0]=尾帧，被上游当首帧用——
	// 产出一条看起来成功、语义却相反的片子。声明依赖让它在提交那一刻被拒，
	// 而不是变成一次沉默的坏产物。
	//
	// 被依赖的槽必须**声明在本槽之前**，由 ValidateSchema 保证，因此不可能成环。
	RequiresSlot string `json:"requires_slot,omitempty"`
}

// OptionItem 是 select / aspect_select 的一个选项。
type OptionItem struct {
	Value any    `json:"value"`
	Label string `json:"label"`
	// Badge 是选项右侧小标签，如 "+2 积分" / "慢"。
	Badge          string `json:"badge,omitempty"`
	Disabled       bool   `json:"disabled,omitempty"`
	DisabledReason string `json:"disabled_reason,omitempty"`

	// RatioW / RatioH / Pixels 仅 aspect_select 使用，用于画出可视比例框
	// 与展示该比例的实际出图像素。
	RatioW *float64   `json:"ratio_w,omitempty"`
	RatioH *float64   `json:"ratio_h,omitempty"`
	Pixels *PixelSize `json:"pixels,omitempty"`
}

// ParamSpec 是一个参数的完整声明。
//
// 见包注释：这是所有 8 种控件的**并集**结构，Control 是判别字段。
// 字段按适用的控件分组标注；不适用的字段在该控件下必须为零值，
// 这条约束由 Validator 保证而不是类型系统。
type ParamSpec struct {
	// ── 全控件通用 ────────────────────────────────────────────────
	Key     string      `json:"key"`
	Label   string      `json:"label"`
	Control ControlKind `json:"control"`
	Group   ParamGroup  `json:"group"`
	Order   int         `json:"order"`
	// Default 是初始值。切换模型时若旧值在新 schema 下非法，回落到这里。
	// compound 的 Default 恒为 nil，真实默认值在各 field 内。
	Default any    `json:"default"`
	Help    string `json:"help,omitempty"`
	// VisibleWhen 不满足时该参数整体不渲染，且提交时不带该 key。
	VisibleWhen *Condition `json:"visible_when,omitempty"`
	// EnabledWhen 不满足时渲染为禁用态，附 DisabledHint。
	EnabledWhen  *Condition `json:"enabled_when,omitempty"`
	DisabledHint string     `json:"disabled_hint,omitempty"`

	// ── select / aspect_select ───────────────────────────────────
	Options    []OptionItem `json:"options,omitempty"`
	RenderHint RenderHint   `json:"render_hint,omitempty"`

	// ── stepper / slider / seed ──────────────────────────────────
	Min  *float64 `json:"min,omitempty"`
	Max  *float64 `json:"max,omitempty"`
	Step *float64 `json:"step,omitempty"`
	Unit string   `json:"unit,omitempty"`

	// ── seed ─────────────────────────────────────────────────────
	// AllowRandom 为 true 时芯片上出现 🎲；Default 为 nil 表示每次随机。
	AllowRandom bool `json:"allow_random,omitempty"`

	// ── textarea ─────────────────────────────────────────────────
	MaxLength   *int   `json:"max_length,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`

	// ── compound ─────────────────────────────────────────────────
	// Fields 提交时各自作为独立 key **平铺**进 params，compound 本身不产生 key。
	Fields []ParamSpec `json:"fields,omitempty"`
	// DisplayTemplate 是芯片标签模板，用 {fieldKey} 占位，如 "{aspect} · {count}张"。
	DisplayTemplate string `json:"display_template,omitempty"`
}

// EtaSpec 是耗时预估。
//
// 供应商大多不给真实百分比，前端靠这里做乐观进度推进（上限卡 90%）。
// ScalesWith 指向某数值参数时，eta 按 该参数值 / 该参数默认值 线性缩放。
type EtaSpec struct {
	P50Seconds float64 `json:"p50_seconds"`
	P90Seconds float64 `json:"p90_seconds"`
	ScalesWith string  `json:"scales_with,omitempty"`
}

// LimitSpec 是该模型的使用限制。
//
// MaxConcurrentPerUser 让前端在达到上限时禁用提交按钮并提示，
// 而不是让用户提交完才被 429 拒掉。
// QueuePositionAvailable 为 false 时前端不显示排队位次（该模型后端无队列信息）。
type LimitSpec struct {
	MaxConcurrentPerUser   int  `json:"max_concurrent_per_user"`
	PromptMaxLength        int  `json:"prompt_max_length"`
	QueuePositionAvailable bool `json:"queue_position_available"`
}

// ModelCapabilitySchema 是一个模型对前端的完整能力声明，
// 逐字段镜像 capability-schema.ts 的同名 interface。
//
// Vendor 仅用于展示与故障提示（「该供应商暂时不可用」）。**前端禁止按它分支**——
// 一旦前端出现 if vendor === 'ark'，「接新模型前端零改动」这条验收线就破了。
type ModelCapabilitySchema struct {
	// ID 是稳定标识，提交任务时原样回传。前端只做透传，绝不解析其内容。
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Vendor         string          `json:"vendor"`
	Modality       domain.Modality `json:"modality"`
	Enabled        bool            `json:"enabled"`
	DisabledReason string          `json:"disabled_reason,omitempty"`
	// Order 是同 modality 下的排序权重，小的在前；相同则按 Name。
	Order       int    `json:"order"`
	Description string `json:"description,omitempty"`
	PreviewURL  string `json:"preview_url,omitempty"`

	Inputs  []InputSlotSpec `json:"inputs"`
	Params  []ParamSpec     `json:"params"`
	Pricing PricingSpec     `json:"pricing"`
	ETA     EtaSpec         `json:"eta"`
	Limits  LimitSpec       `json:"limits"`
}
