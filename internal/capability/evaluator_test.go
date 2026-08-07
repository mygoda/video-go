package capability

import "testing"

func ptrF(f float64) *float64 { return &f }

func TestEvaluatorOperators(t *testing.T) {
	ctx := EvalContext{
		Params: map[string]any{
			"resolution": "1080p",
			"count":      float64(3),
			"pro":        true,
			"seed":       nil,
			"duration":   float64(5),
			"label":      "5",
		},
		Inputs: map[string][]string{
			"reference_images": {"up_1", "up_2"},
			"first_frame":      {},
		},
	}

	cases := []struct {
		name string
		cond Condition
		want bool
	}{
		{"eq 字符串命中", Condition{Op: OpEq, Key: "resolution", Value: "1080p"}, true},
		{"eq 字符串不命中", Condition{Op: OpEq, Key: "resolution", Value: "768p"}, false},
		{"eq 数值跨 int/float", Condition{Op: OpEq, Key: "count", Value: 3}, true},
		{"eq 数值与字符串不等（对齐 JS ===）", Condition{Op: OpEq, Key: "label", Value: 5}, false},
		{"eq 布尔", Condition{Op: OpEq, Key: "pro", Value: true}, true},
		{"eq null", Condition{Op: OpEq, Key: "seed", Value: nil}, true},
		{"eq 缺失的 key 视同 null", Condition{Op: OpEq, Key: "missing", Value: nil}, true},
		{"ne 命中", Condition{Op: OpNe, Key: "resolution", Value: "768p"}, true},
		{"ne 不命中", Condition{Op: OpNe, Key: "resolution", Value: "1080p"}, false},
		{"gt 命中", Condition{Op: OpGt, Key: "count", Value: float64(2)}, true},
		{"gt 边界不命中", Condition{Op: OpGt, Key: "count", Value: float64(3)}, false},
		{"gt 字符串数值参与比较", Condition{Op: OpGt, Key: "label", Value: float64(4)}, true},
		{"lt 命中", Condition{Op: OpLt, Key: "count", Value: float64(4)}, true},
		{"lt 不命中", Condition{Op: OpLt, Key: "count", Value: float64(3)}, false},
		{"in 命中", Condition{Op: OpIn, Key: "resolution", Value: []any{"768p", "1080p"}}, true},
		{"in 不命中", Condition{Op: OpIn, Key: "resolution", Value: []any{"512p"}}, false},
		{"in 数值元素", Condition{Op: OpIn, Key: "duration", Value: []any{5, 10}}, true},
		{"nin 命中", Condition{Op: OpNin, Key: "resolution", Value: []any{"512p"}}, true},
		{"nin 不命中", Condition{Op: OpNin, Key: "resolution", Value: []any{"1080p"}}, false},
		{"has_input 有文件", Condition{Op: OpHasInput, Slot: "reference_images"}, true},
		{"has_input 空数组", Condition{Op: OpHasInput, Slot: "first_frame"}, false},
		{"has_input 槽不存在", Condition{Op: OpHasInput, Slot: "nope"}, false},
		{"and 全真", Condition{Op: OpAnd, Of: []Condition{
			{Op: OpEq, Key: "resolution", Value: "1080p"},
			{Op: OpHasInput, Slot: "reference_images"},
		}}, true},
		{"and 有假", Condition{Op: OpAnd, Of: []Condition{
			{Op: OpEq, Key: "resolution", Value: "1080p"},
			{Op: OpHasInput, Slot: "first_frame"},
		}}, false},
		{"and 空数组为真", Condition{Op: OpAnd}, true},
		{"or 有真", Condition{Op: OpOr, Of: []Condition{
			{Op: OpEq, Key: "resolution", Value: "512p"},
			{Op: OpHasInput, Slot: "reference_images"},
		}}, true},
		{"or 全假", Condition{Op: OpOr, Of: []Condition{
			{Op: OpEq, Key: "resolution", Value: "512p"},
			{Op: OpHasInput, Slot: "first_frame"},
		}}, false},
		{"or 空数组为假", Condition{Op: OpOr}, false},
		{"not 取反", Condition{Op: OpNot, Of: []Condition{{Op: OpHasInput, Slot: "first_frame"}}}, true},
		{"not 嵌套", Condition{Op: OpNot, Of: []Condition{
			{Op: OpNot, Of: []Condition{{Op: OpHasInput, Slot: "reference_images"}}},
		}}, true},
	}

	e := NewEvaluator()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.Eval(tc.cond, ctx)
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEvaluatorErrors(t *testing.T) {
	ctx := EvalContext{Params: map[string]any{"resolution": "1080p", "obj": map[string]any{}}}

	cases := []struct {
		name string
		cond Condition
	}{
		{"未知算子", Condition{Op: "between", Key: "count", Value: 1}},
		{"gt 的 value 非数值", Condition{Op: OpGt, Key: "count", Value: "3"}},
		{"gt 的参数值不可转数值", Condition{Op: OpGt, Key: "resolution", Value: float64(3)}},
		{"gt 的参数值是对象", Condition{Op: OpGt, Key: "obj", Value: float64(3)}},
		{"in 的 value 非数组", Condition{Op: OpIn, Key: "resolution", Value: "1080p"}},
		{"has_input 缺 slot", Condition{Op: OpHasInput}},
		{"not 缺子条件", Condition{Op: OpNot}},
		{"and 的子条件出错", Condition{Op: OpAnd, Of: []Condition{{Op: "zzz"}}}},
		{"or 的子条件出错", Condition{Op: OpOr, Of: []Condition{{Op: "zzz"}}}},
	}

	e := NewEvaluator()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := e.Eval(tc.cond, ctx); err == nil {
				t.Fatal("期望报错，实际通过")
			}
		})
	}
}

func TestEvalOptionalNilIsTrue(t *testing.T) {
	ok, err := EvalOptional(NewEvaluator(), nil, EvalContext{})
	if err != nil || !ok {
		t.Fatalf("nil 条件应恒真，got %v %v", ok, err)
	}
}
