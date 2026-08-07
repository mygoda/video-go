package capability

import (
	"fmt"
	"math"
)

// pricer 是 Pricer 的默认实现。它只依赖 Evaluator，无自身状态，并发安全。
type pricer struct{ eval Evaluator }

// NewPricer 返回默认计价器。传 nil 时用 NewEvaluator()，
// 保证「两端同一套条件语义」这条约束不会因为忘了注入而被绕开。
func NewPricer(e Evaluator) Pricer {
	if e == nil {
		e = NewEvaluator()
	}
	return pricer{eval: e}
}

// Estimate 按 PricingSpec 声明的顺序算出积分。
//
// 全程用 float64 累积、只在最后一步取整：中途取整两端极易分叉（见 Pricer 注释）。
//
// 两处与前端 costBreakdown 的行为需要说明:
//
//  1. MultiplierParam 指向的 key **不在** Params 里时，前端是 Number(undefined)=NaN
//     → 跳过乘法（显示 base 价）；这里返回 error。前端跳过是为了不让展示崩掉，
//     服务端跳过则意味着 10s 的视频按 5s 收费——少收的钱没人会来投诉，
//     所以宁可拒绝这次提交。key 存在但值为 null 时两端都按 Number(null)=0 走，
//     保持逐位一致。
//  2. 取整用 floor(x+0.5) 而不是 math.Round：JS 的 Math.round 在 .5 上朝
//     +∞ 取整（Math.round(-1.5) === -1），math.Round 朝远离零取整（-2）。
//     负数计价虽然罕见，但两端在同一个输入上给出不同的数就是事故。
func (p pricer) Estimate(spec PricingSpec, ctx EvalContext) (int, error) {
	subtotal := spec.Base

	for i, m := range spec.Modifiers {
		hit, err := p.eval.Eval(m.When, ctx)
		if err != nil {
			return 0, fmt.Errorf("capability: 计价规则 #%d(%s) 求值失败: %w", i, m.Label, err)
		}
		if !hit {
			continue
		}
		switch m.Op {
		case PricingMul:
			subtotal *= m.Value
		case PricingAdd:
			subtotal += m.Value
		default:
			return 0, fmt.Errorf("capability: 计价规则 #%d 的算子 %q 未知", i, m.Op)
		}
	}

	if spec.MultiplierParam != "" {
		v, present := ctx.Params[spec.MultiplierParam]
		if !present {
			return 0, fmt.Errorf("capability: 计价乘数参数 %q 缺失", spec.MultiplierParam)
		}
		n, ok := numberOf(v)
		if !ok {
			return 0, fmt.Errorf("capability: 计价乘数参数 %q 的值 %v 不是数值", spec.MultiplierParam, v)
		}
		subtotal *= n
	}

	return roundCost(subtotal, spec.Rounding)
}

func roundCost(subtotal float64, mode Rounding) (int, error) {
	if math.IsNaN(subtotal) || math.IsInf(subtotal, 0) {
		return 0, fmt.Errorf("capability: 计价中间量非有限值(%v)，检查 base 与 modifiers", subtotal)
	}
	var cost float64
	switch mode {
	case RoundCeil:
		cost = math.Ceil(subtotal)
	case RoundFloor:
		cost = math.Floor(subtotal)
	case RoundRound:
		cost = math.Floor(subtotal + 0.5)
	default:
		return 0, fmt.Errorf("capability: 未知取整方式 %q", mode)
	}
	if cost > math.MaxInt32 || cost < math.MinInt32 {
		return 0, fmt.Errorf("capability: 计价结果 %v 超出合理区间", cost)
	}
	return int(cost), nil
}
