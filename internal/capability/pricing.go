package capability

// PricingOp 是计价修正的算子。
type PricingOp string

const (
	PricingMul PricingOp = "mul"
	PricingAdd PricingOp = "add"
)

// Rounding 是最终取整方式。
type Rounding string

const (
	RoundCeil  Rounding = "ceil"
	RoundRound Rounding = "round"
	RoundFloor Rounding = "floor"
)

// PricingModifier 是一条条件加价规则。按 PricingSpec.Modifiers 的**数组顺序**依次应用。
//
// Label 是展示用的，如 "1080P 加价"；前端在费用估算上悬浮时列出命中的规则，
// 让用户知道钱花在哪，而不是只看到一个变了的数字。
type PricingModifier struct {
	When  Condition `json:"when"`
	Op    PricingOp `json:"op"`
	Value float64   `json:"value"`
	Label string    `json:"label,omitempty"`
}

// PricingSpec 是一个模型的计价声明。
//
// 算法（前后端必须实现得完全一样）：
//
//	subtotal = Base
//	for m in Modifiers:            // 严格按数组顺序
//	    if Eval(m.When) {
//	        subtotal = m.Op == mul ? subtotal * m.Value : subtotal + m.Value
//	    }
//	if MultiplierParam != "" {
//	    subtotal *= Number(params[MultiplierParam])   // 如 count（几张）/ duration（几秒）
//	}
//	cost = Rounding(subtotal)
//
// Currency 恒为 "credit"，保留字段是为了万一以后引入第二种计价单位时
// 老数据仍能自解释。
type PricingSpec struct {
	Currency string  `json:"currency"`
	Base     float64 `json:"base"`
	// Modifiers 按数组顺序依次应用，顺序影响结果（先乘后加与先加后乘不同）。
	Modifiers []PricingModifier `json:"modifiers"`
	// MultiplierParam 指向一个数值参数，最后整体乘上它的值。
	MultiplierParam string   `json:"multiplier_param,omitempty"`
	Rounding        Rounding `json:"rounding"`
}

// Pricer 根据 PricingSpec 与一次提交的参数算出预估积分。
//
// **实现必须与前端的本地计价函数产出完全相同的整数。**
// 前端为了让用户每动一枚芯片就能实时看到价格，在本地跑同一套算法；
// 服务端算出的数字是扣费依据。两者一旦不一致，用户看到的价和实际扣的钱就对不上——
// 这是产品级事故，不是显示瑕疵。所以：
//
//   - 修改计价算法必须前后端同一个 PR 改；
//   - 浮点中间量在最后一步才按 Rounding 取整，不要中途取整（中途取整两端极易分叉）；
//   - 修改 Rounding 的语义等价于修改价格，按破坏契约对待。
//
// 返回的整数即 Task.EstimatedCost，也是 billing.Ledger.Hold 冻结的额度。
type Pricer interface {
	Estimate(spec PricingSpec, ctx EvalContext) (int, error)
}
