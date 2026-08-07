package capability

import "fmt"

// evaluator 是 Evaluator 的默认实现，无状态因而并发安全，
// 一个进程共用一个实例即可。
type evaluator struct{}

// NewEvaluator 返回默认的条件求值器。
func NewEvaluator() Evaluator { return evaluator{} }

// Eval 对一个 Condition 求值。
//
// 与前端 evaluate 的差异只有一处且是刻意的：**未知算子在这里报错，前端当恒真**。
// 前端当恒真是为了「后端上了新算子而前端还没发版时不阻断渲染」，
// 服务端没有这个顾虑——服务端就是新算子的来源，见到不认识的算子只能是配置写错了，
// 静默取真会让一条本该拦截的规则消失。
//
// gt/lt 作用于非数值同理报错而不是取 false：包注释里说过，
// 静默取 false 会让一条本该命中的加价规则消失，用户少付钱而平台不知情。
func (e evaluator) Eval(cond Condition, ctx EvalContext) (bool, error) {
	switch cond.Op {
	case OpEq:
		return strictEquals(ctx.Params[cond.Key], cond.Value), nil
	case OpNe:
		return !strictEquals(ctx.Params[cond.Key], cond.Value), nil
	case OpGt, OpLt:
		return e.compare(cond, ctx)
	case OpIn, OpNin:
		return e.member(cond, ctx)
	case OpHasInput:
		if cond.Slot == "" {
			return false, fmt.Errorf("capability: %s 条件缺少 slot", cond.Op)
		}
		return len(ctx.Inputs[cond.Slot]) > 0, nil
	case OpAnd:
		for _, sub := range cond.Of {
			ok, err := e.Eval(sub, ctx)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	case OpOr:
		for _, sub := range cond.Of {
			ok, err := e.Eval(sub, ctx)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case OpNot:
		if len(cond.Of) == 0 {
			return false, fmt.Errorf("capability: not 条件缺少子条件")
		}
		ok, err := e.Eval(cond.Of[0], ctx)
		if err != nil {
			return false, err
		}
		return !ok, nil
	default:
		return false, fmt.Errorf("capability: 未知条件算子 %q", cond.Op)
	}
}

func (e evaluator) compare(cond Condition, ctx EvalContext) (bool, error) {
	threshold, ok := rawNumber(cond.Value)
	if !ok {
		return false, fmt.Errorf("capability: %s 条件的 value 必须是数值，得到 %T", cond.Op, cond.Value)
	}
	// 前端写的是 Number(values[key]) > cond.value，所以 "5" 和 true 也参与比较；
	// 这里跟着走 numberOf 而不是 rawNumber，否则两端在字符串数值上分叉。
	actual, ok := numberOf(ctx.Params[cond.Key])
	if !ok {
		return false, fmt.Errorf("capability: 参数 %q 的值 %v 无法作为数值参与 %s 比较", cond.Key, ctx.Params[cond.Key], cond.Op)
	}
	if cond.Op == OpGt {
		return actual > threshold, nil
	}
	return actual < threshold, nil
}

func (e evaluator) member(cond Condition, ctx EvalContext) (bool, error) {
	list, ok := cond.Value.([]any)
	if !ok {
		return false, fmt.Errorf("capability: %s 条件的 value 必须是数组，得到 %T", cond.Op, cond.Value)
	}
	actual := ctx.Params[cond.Key]
	for _, want := range list {
		if strictEquals(actual, want) {
			return cond.Op == OpIn, nil
		}
	}
	return cond.Op == OpNin, nil
}

// EvalOptional 对可选条件求值：nil 条件视为满足。
//
// VisibleWhen / EnabledWhen / MappingRule.When 都是 *Condition，
// 三处都要「没声明就是永远成立」，与前端 evaluate(undefined) 返回 true 对齐。
// 抽成函数是为了让这条约定只写一遍。
func EvalOptional(e Evaluator, cond *Condition, ctx EvalContext) (bool, error) {
	if cond == nil {
		return true, nil
	}
	return e.Eval(*cond, ctx)
}
