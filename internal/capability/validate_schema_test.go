package capability

import (
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

func intPtr(i int) *int { return &i }

// baseSchema 是一份最小的合法 schema，各用例只改动它的一处，
// 这样断言出的错误一定来自被改动的那一处。
func baseSchema() ModelCapabilitySchema {
	return ModelCapabilitySchema{
		ID: "m", Name: "M", Vendor: "v", Modality: domain.ModalityImage, Enabled: true, Order: 1,
		Inputs: []InputSlotSpec{{
			Key: "ref", Label: "参考图", Kind: "image", Required: false,
			MinCount: 0, MaxCount: 3, Accept: []string{"image/png"}, MaxBytes: 1024,
		}},
		Params: []ParamSpec{{
			Key: "count", Label: "数量", Control: ControlStepper, Group: GroupPrimary, Order: 10,
			Default: float64(1), Min: ptrF(1), Max: ptrF(4), Step: ptrF(1),
		}},
		Pricing: PricingSpec{Currency: "credit", Base: 8, Rounding: RoundCeil, MultiplierParam: "count"},
		ETA:     EtaSpec{P50Seconds: 10, P90Seconds: 30},
		Limits:  LimitSpec{MaxConcurrentPerUser: 4, PromptMaxLength: 2000, QueuePositionAvailable: true},
	}
}

func TestValidateSchema(t *testing.T) {
	v := NewValidator(nil)

	cases := []struct {
		name   string
		mutate func(*ModelCapabilitySchema)
		want   []string
	}{
		{name: "基线合法", mutate: func(*ModelCapabilitySchema) {}},
		{
			name:   "id 为空",
			mutate: func(s *ModelCapabilitySchema) { s.ID = " " },
			want:   []string{"id"},
		},
		{
			name:   "modality 非法",
			mutate: func(s *ModelCapabilitySchema) { s.Modality = "audio" },
			want:   []string{"modality"},
		},
		{
			name:   "禁用但没给理由",
			mutate: func(s *ModelCapabilitySchema) { s.Enabled = false },
			want:   []string{"disabled_reason"},
		},
		{
			name:   "输入槽 kind 非法",
			mutate: func(s *ModelCapabilitySchema) { s.Inputs[0].Kind = "pdf" },
			want:   []string{"inputs[0].kind"},
		},
		{
			name:   "输入槽 accept 为空",
			mutate: func(s *ModelCapabilitySchema) { s.Inputs[0].Accept = nil },
			want:   []string{"inputs[0].accept"},
		},
		{
			name:   "输入槽 min 大于 max",
			mutate: func(s *ModelCapabilitySchema) { s.Inputs[0].MinCount = 5 },
			want:   []string{"inputs[0].min_count"},
		},
		{
			name: "输入槽 key 重复",
			mutate: func(s *ModelCapabilitySchema) {
				s.Inputs = append(s.Inputs, s.Inputs[0])
			},
			want: []string{"inputs[1].key"},
		},
		{
			name:   "requires_slot 指向不存在的槽",
			mutate: func(s *ModelCapabilitySchema) { s.Inputs[0].RequiresSlot = "nope" },
			want:   []string{"inputs[0].requires_slot"},
		},
		{
			name:   "requires_slot 指向自己",
			mutate: func(s *ModelCapabilitySchema) { s.Inputs[0].RequiresSlot = "ref" },
			want:   []string{"inputs[0].requires_slot"},
		},
		{
			// 只允许向前依赖，因此依赖不可能成环。
			name: "requires_slot 指向声明在其后的槽",
			mutate: func(s *ModelCapabilitySchema) {
				s.Inputs[0].RequiresSlot = "later"
				s.Inputs = append(s.Inputs, InputSlotSpec{
					Key: "later", Label: "后", Kind: "image", MaxCount: 1,
					Accept: []string{"image/png"}, MaxBytes: 1024,
				})
			},
			want: []string{"inputs[0].requires_slot"},
		},
		{
			name: "requires_slot 指向声明在其前的槽",
			mutate: func(s *ModelCapabilitySchema) {
				s.Inputs = append(s.Inputs, InputSlotSpec{
					Key: "later", Label: "后", Kind: "image", MaxCount: 1,
					Accept: []string{"image/png"}, MaxBytes: 1024, RequiresSlot: "ref",
				})
			},
		},
		{
			name: "toggle 却带了 options",
			mutate: func(s *ModelCapabilitySchema) {
				s.Params[0] = ParamSpec{
					Key: "count", Label: "开关", Control: ControlToggle, Group: GroupPrimary, Default: false,
					Options: []OptionItem{{Value: "a", Label: "A"}},
				}
			},
			want: []string{"params[0].options"},
		},
		{
			name: "select 没有 options",
			mutate: func(s *ModelCapabilitySchema) {
				s.Params[0] = ParamSpec{Key: "count", Label: "选择", Control: ControlSelect, Group: GroupPrimary, Default: nil}
			},
			// options 缺失 + default 不在（空）选项内
			want: []string{"params[0].default", "params[0].options"},
		},
		{
			name:   "stepper 缺 step",
			mutate: func(s *ModelCapabilitySchema) { s.Params[0].Step = nil },
			want:   []string{"params[0].step"},
		},
		{
			name:   "stepper 的 min 大于 max",
			mutate: func(s *ModelCapabilitySchema) { s.Params[0].Min = ptrF(9) },
			want:   []string{"params[0].default", "params[0].min"},
		},
		{
			name:   "默认值越界",
			mutate: func(s *ModelCapabilitySchema) { s.Params[0].Default = float64(99) },
			want:   []string{"params[0].default"},
		},
		{
			name: "textarea 缺 max_length",
			mutate: func(s *ModelCapabilitySchema) {
				s.Params[0] = ParamSpec{Key: "count", Label: "文本", Control: ControlTextarea, Group: GroupPrimary, Default: ""}
			},
			want: []string{"params[0].max_length"},
		},
		{
			name:   "非 textarea 带了 max_length",
			mutate: func(s *ModelCapabilitySchema) { s.Params[0].MaxLength = intPtr(10) },
			want:   []string{"params[0].max_length"},
		},
		{
			name:   "非 seed 带了 allow_random",
			mutate: func(s *ModelCapabilitySchema) { s.Params[0].AllowRandom = true },
			want:   []string{"params[0].allow_random"},
		},
		{
			name:   "未知控件",
			mutate: func(s *ModelCapabilitySchema) { s.Params[0].Control = "color_picker" },
			want:   []string{"params[0].control"},
		},
		{
			name: "声明 enabled_when 却没给 disabled_hint",
			mutate: func(s *ModelCapabilitySchema) {
				s.Params[0].EnabledWhen = &Condition{Op: OpHasInput, Slot: "ref"}
			},
			want: []string{"params[0].disabled_hint"},
		},
		{
			name: "has_input 指向不存在的槽",
			mutate: func(s *ModelCapabilitySchema) {
				s.Params[0].VisibleWhen = &Condition{Op: OpHasInput, Slot: "mask"}
			},
			want: []string{"params[0].visible_when"},
		},
		{
			name: "not 条件带了两个子条件",
			mutate: func(s *ModelCapabilitySchema) {
				s.Params[0].VisibleWhen = &Condition{Op: OpNot, Of: []Condition{
					{Op: OpHasInput, Slot: "ref"}, {Op: OpHasInput, Slot: "ref"},
				}}
			},
			want: []string{"params[0].visible_when"},
		},
		{
			name: "in 条件的 value 不是数组",
			mutate: func(s *ModelCapabilitySchema) {
				s.Params[0].VisibleWhen = &Condition{Op: OpIn, Key: "count", Value: "1"}
			},
			want: []string{"params[0].visible_when"},
		},
		{
			name: "compound 自身 default 非 null",
			mutate: func(s *ModelCapabilitySchema) {
				s.Params = []ParamSpec{{
					Key: "grp", Label: "组", Control: ControlCompound, Group: GroupPrimary,
					Default: "x", DisplayTemplate: "{count}张",
					Fields: []ParamSpec{baseSchema().Params[0]},
				}}
			},
			want: []string{"params[0].default"},
		},
		{
			name: "compound 模板占位符对不上子字段",
			mutate: func(s *ModelCapabilitySchema) {
				s.Params = []ParamSpec{{
					Key: "grp", Label: "组", Control: ControlCompound, Group: GroupPrimary,
					DisplayTemplate: "{aspect} · {count}张",
					Fields:          []ParamSpec{baseSchema().Params[0]},
				}}
			},
			want: []string{"params[0].display_template"},
		},
		{
			name: "非 compound 带了 fields",
			mutate: func(s *ModelCapabilitySchema) {
				s.Params[0].Fields = []ParamSpec{baseSchema().Params[0]}
			},
			want: []string{"params[0].fields"},
		},
		{
			name:   "计价乘数指向不存在的参数",
			mutate: func(s *ModelCapabilitySchema) { s.Pricing.MultiplierParam = "duration" },
			want:   []string{"pricing.multiplier_param"},
		},
		{
			name:   "取整方式非法",
			mutate: func(s *ModelCapabilitySchema) { s.Pricing.Rounding = "bankers" },
			want:   []string{"pricing.rounding"},
		},
		{
			name: "计价算子非法",
			mutate: func(s *ModelCapabilitySchema) {
				s.Pricing.Modifiers = []PricingModifier{{When: Condition{Op: OpEq, Key: "count", Value: float64(1)}, Op: "sub", Value: 1}}
			},
			want: []string{"pricing.modifiers[0].op"},
		},
		{
			name: "计价条件里的 slot 不存在",
			mutate: func(s *ModelCapabilitySchema) {
				s.Pricing.Modifiers = []PricingModifier{{When: Condition{Op: OpHasInput, Slot: "mask"}, Op: PricingAdd, Value: 1}}
			},
			want: []string{"pricing.modifiers[0].when"},
		},
		{
			name:   "eta.scales_with 指向不存在的参数",
			mutate: func(s *ModelCapabilitySchema) { s.ETA.ScalesWith = "duration" },
			want:   []string{"eta.scales_with"},
		},
		{
			name:   "p90 小于 p50",
			mutate: func(s *ModelCapabilitySchema) { s.ETA.P90Seconds = 1 },
			want:   []string{"eta.p90_seconds"},
		},
		{
			name:   "prompt_max_length 非正",
			mutate: func(s *ModelCapabilitySchema) { s.Limits.PromptMaxLength = 0 },
			want:   []string{"limits.prompt_max_length"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := baseSchema()
			tc.mutate(&s)
			got := errKeys(v.ValidateSchema(s))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
