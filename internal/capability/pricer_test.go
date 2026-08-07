package capability

import "testing"

func TestPricerAgainstContractExamples(t *testing.T) {
	// 期望值直接抄自 model-schema.examples.json 里 pricing.$comment 的算式，
	// 前端 costBreakdown 跑同一份输入必须得到同一个数。
	examples := loadExamples(t)
	p := NewPricer(nil)

	cases := []struct {
		name   string
		model  string
		params map[string]any
		inputs map[string][]string
		want   int
	}{
		{
			name:   "seedream 默认：base 原样",
			model:  "seedream-5-pro",
			params: map[string]any{"style_model": "none", "aspect": "1:1", "count": float64(1)},
			want:   8,
		},
		{
			name:   "seedream 加价与宽幅叠乘：(8+2)*1.2*2",
			model:  "seedream-5-pro",
			params: map[string]any{"style_model": "film_a24", "aspect": "16:9", "count": float64(2)},
			want:   24,
		},
		{
			name:   "seedream 只命中 add：(8+2)*3",
			model:  "seedream-5-pro",
			params: map[string]any{"style_model": "film_a24", "aspect": "1:1", "count": float64(3)},
			want:   30,
		},
		{
			name:   "seedream 只命中 mul 且需向上取整：8*1.2*1=9.6",
			model:  "seedream-5-pro",
			params: map[string]any{"style_model": "none", "aspect": "9:16", "count": float64(1)},
			want:   10,
		},
		{
			name:   "hailuo 5s 普通 768P = 4×5",
			model:  "hailuo-2-3",
			params: map[string]any{"resolution": "768p", "mode": "standard", "duration": float64(5)},
			want:   20,
		},
		{
			name:   "hailuo 10s 专业 1080P = 4×2×1.5×10",
			model:  "hailuo-2-3",
			params: map[string]any{"resolution": "1080p", "mode": "pro", "duration": float64(10)},
			want:   120,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schema, ok := examples[tc.model]
			if !ok {
				t.Fatalf("契约样例缺少模型 %s", tc.model)
			}
			got, err := p.Estimate(schema.Pricing, EvalContext{Params: tc.params, Inputs: tc.inputs})
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPricerRounding(t *testing.T) {
	cases := []struct {
		name  string
		base  float64
		mode  Rounding
		want  int
		multi string
		param map[string]any
	}{
		{name: "ceil 向上", base: 8.1, mode: RoundCeil, want: 9},
		{name: "ceil 整数不动", base: 8, mode: RoundCeil, want: 8},
		{name: "floor 向下", base: 8.9, mode: RoundFloor, want: 8},
		{name: "round 半数朝上", base: 8.5, mode: RoundRound, want: 9},
		{name: "round 半数以下朝下", base: 8.4, mode: RoundRound, want: 8},
		{name: "round 负半数朝 +∞（对齐 JS Math.round）", base: -1.5, mode: RoundRound, want: -1},
	}
	p := NewPricer(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := PricingSpec{Currency: "credit", Base: tc.base, Rounding: tc.mode, MultiplierParam: tc.multi}
			got, err := p.Estimate(spec, EvalContext{Params: tc.param})
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPricerOrderMatters(t *testing.T) {
	// 先加后乘与先乘后加结果不同，数组顺序是契约的一部分。
	always := Condition{Op: OpEq, Key: "x", Value: float64(1)}
	ctx := EvalContext{Params: map[string]any{"x": float64(1)}}
	p := NewPricer(nil)

	addThenMul := PricingSpec{Currency: "credit", Base: 10, Rounding: RoundCeil, Modifiers: []PricingModifier{
		{When: always, Op: PricingAdd, Value: 2},
		{When: always, Op: PricingMul, Value: 2},
	}}
	mulThenAdd := PricingSpec{Currency: "credit", Base: 10, Rounding: RoundCeil, Modifiers: []PricingModifier{
		{When: always, Op: PricingMul, Value: 2},
		{When: always, Op: PricingAdd, Value: 2},
	}}

	if got, err := p.Estimate(addThenMul, ctx); err != nil || got != 24 {
		t.Fatalf("先加后乘 got %d(%v), want 24", got, err)
	}
	if got, err := p.Estimate(mulThenAdd, ctx); err != nil || got != 22 {
		t.Fatalf("先乘后加 got %d(%v), want 22", got, err)
	}
}

func TestPricerNoIntermediateRounding(t *testing.T) {
	// 中途取整会得到 ceil(1.5)*3 = 6；只在最后取整得到 ceil(4.5) = 5。
	spec := PricingSpec{Currency: "credit", Base: 1.5, Rounding: RoundCeil, MultiplierParam: "n"}
	got, err := NewPricer(nil).Estimate(spec, EvalContext{Params: map[string]any{"n": float64(3)}})
	if err != nil {
		t.Fatal(err)
	}
	if got != 5 {
		t.Fatalf("got %d, want 5（中途取整会得到 6）", got)
	}
}

func TestPricerMultiplierValueForms(t *testing.T) {
	spec := PricingSpec{Currency: "credit", Base: 4, Rounding: RoundCeil, MultiplierParam: "n"}
	p := NewPricer(nil)

	cases := []struct {
		name string
		val  any
		want int
	}{
		{"float64", float64(5), 20},
		{"int", 5, 20},
		{"字符串数值（对齐 JS Number()）", "5", 20},
		{"null 视为 0", nil, 0},
		{"true 视为 1", true, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.Estimate(spec, EvalContext{Params: map[string]any{"n": tc.val}})
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPricerErrors(t *testing.T) {
	p := NewPricer(nil)
	cases := []struct {
		name string
		spec PricingSpec
		ctx  EvalContext
	}{
		{
			name: "乘数参数缺失",
			spec: PricingSpec{Currency: "credit", Base: 4, Rounding: RoundCeil, MultiplierParam: "n"},
			ctx:  EvalContext{Params: map[string]any{}},
		},
		{
			name: "乘数参数不是数值",
			spec: PricingSpec{Currency: "credit", Base: 4, Rounding: RoundCeil, MultiplierParam: "n"},
			ctx:  EvalContext{Params: map[string]any{"n": "many"}},
		},
		{
			name: "未知取整方式",
			spec: PricingSpec{Currency: "credit", Base: 4, Rounding: "nearest"},
		},
		{
			name: "未知计价算子",
			spec: PricingSpec{Currency: "credit", Base: 4, Rounding: RoundCeil, Modifiers: []PricingModifier{
				{When: Condition{Op: OpEq, Key: "x", Value: nil}, Op: "sub", Value: 1},
			}},
		},
		{
			name: "条件求值失败",
			spec: PricingSpec{Currency: "credit", Base: 4, Rounding: RoundCeil, Modifiers: []PricingModifier{
				{When: Condition{Op: "zzz"}, Op: PricingAdd, Value: 1},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.Estimate(tc.spec, tc.ctx); err == nil {
				t.Fatal("期望报错，实际通过")
			}
		})
	}
}
