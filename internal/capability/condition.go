package capability

// ConditionOp 是条件表达式的算子。
type ConditionOp string

const (
	// 取值比较：作用于 params[key]。
	OpEq  ConditionOp = "eq"
	OpNe  ConditionOp = "ne"
	OpGt  ConditionOp = "gt"
	OpLt  ConditionOp = "lt"
	OpIn  ConditionOp = "in"
	OpNin ConditionOp = "nin"

	// OpHasInput 判断某输入槽当前是否有文件。**隐式分流的载体**：
	// 「重绘强度只在传了参考图时出现」写成
	// {op: has_input, slot: reference_images}，而不是前端写死一个 if。
	OpHasInput ConditionOp = "has_input"

	// 逻辑组合，作用于 Of。
	OpAnd ConditionOp = "and"
	OpOr  ConditionOp = "or"
	OpNot ConditionOp = "not"
)

// Condition 是参数联动、隐式分流与计价规则的声明式表达。
//
// 与 ParamSpec 同理，TS 侧是判别联合、openapi 侧是 oneOf，Go 侧是**一个带 Op
// 判别字段的递归结构体**：
//
//	eq/ne/gt/lt/in/nin → 用 Key + Value
//	has_input          → 用 Slot
//	and/or             → 用 Of（多个子条件）
//	not                → 用 Of（约定只取第一个元素，使 not 与 and/or 共用一个字段，
//	                     避免为单目算子再开一个 *Condition 字段）
//
// 前端的对应实现是纯函数 evaluate(cond, formValues, inputs) bool。
// **两端语义必须逐位一致**，否则前端算的价和后端扣的钱会对不上。
type Condition struct {
	Op ConditionOp `json:"op"`

	// Key 是 params 里的参数 key，用于 eq/ne/gt/lt/in/nin。
	Key string `json:"key,omitempty"`
	// Value 对 eq/ne 是标量，对 gt/lt 是数值，对 in/nin 是数组。
	Value any `json:"value,omitempty"`

	// Slot 是 InputSlotSpec.Key，用于 has_input。
	Slot string `json:"slot,omitempty"`

	// Of 是子条件。and/or 取全部，not 取第一个。
	Of []Condition `json:"of,omitempty"`
}

// EvalContext 是求值上下文：一次提交里用户填的参数与挂的输入。
//
// Params 的 key 是 ParamSpec.Key（compound 已平铺）。
// Inputs 的 key 是 InputSlotSpec.Key，值是 upload_id / asset_id 数组；
// has_input 判断的就是对应槽的数组非空。
type EvalContext struct {
	Params map[string]any
	Inputs map[string][]string
}

// Evaluator 对一个 Condition 求值。
//
// 实现必须与前端的 evaluate 纯函数**逐位一致**：同样的 Condition + 同样的
// EvalContext 必须得到同样的布尔值。它同时被三处使用——
// 参数可见性（VisibleWhen）、参数可用性（EnabledWhen）、计价规则命中（PricingModifier.When）
// 以及 mapping 规则的 When——因此任何语义偏差都会同时污染表单、价格和上游请求体。
//
// 对语义上无法判定的输入（如 gt 作用于非数值），实现应返回 error 而不是静默取 false：
// 静默取 false 会让一条本该命中的加价规则消失，用户少付钱而平台不知情。
type Evaluator interface {
	Eval(cond Condition, ctx EvalContext) (bool, error)
}
