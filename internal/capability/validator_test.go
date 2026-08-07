package capability

import (
	"sort"
	"strings"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

func errKeys(errs []domain.FieldError) []string {
	keys := make([]string, 0, len(errs))
	for _, e := range errs {
		keys = append(keys, e.Key)
	}
	sort.Strings(keys)
	return keys
}

func TestValidateSubmission(t *testing.T) {
	schema := loadExamples(t)["seedream-5-pro"]
	v := NewValidator(nil)

	// 提交里必须带全部可见参数；base 是「传了参考图」那一支的合法基线。
	withRef := func(overrides map[string]any, drop ...string) Submission {
		params := map[string]any{
			"style_model":     "none",
			"aspect":          "1:1",
			"count":           float64(1),
			"strength":        0.6,
			"negative_prompt": "",
			"seed":            nil,
		}
		for k, val := range overrides {
			params[k] = val
		}
		for _, k := range drop {
			delete(params, k)
		}
		return Submission{
			ModelID: "seedream-5-pro",
			Prompt:  "一只猫",
			Inputs:  map[string][]string{"reference_images": {"up_1"}},
			Params:  params,
		}
	}

	cases := []struct {
		name string
		sub  Submission
		want []string
	}{
		{name: "合法提交（带参考图）", sub: withRef(nil)},
		{
			name: "合法提交（不带参考图，strength 不可见故不提交）",
			sub: Submission{
				Prompt: "一只猫",
				Inputs: map[string][]string{},
				Params: map[string]any{
					"style_model": "none", "aspect": "1:1", "count": float64(1),
					"negative_prompt": "", "seed": nil,
				},
			},
		},
		{
			name: "不可见参数被提交",
			sub: Submission{
				Prompt: "一只猫",
				Params: map[string]any{
					"style_model": "none", "aspect": "1:1", "count": float64(1),
					"negative_prompt": "", "seed": nil, "strength": 0.6,
				},
			},
			want: []string{"strength"},
		},
		{name: "可见参数缺失", sub: withRef(nil, "aspect"), want: []string{"aspect"}},
		{name: "select 取值不在选项内", sub: withRef(map[string]any{"style_model": "ghibli"}), want: []string{"style_model"}},
		{name: "aspect_select 取值不在选项内", sub: withRef(map[string]any{"aspect": "21:9"}), want: []string{"aspect"}},
		{name: "stepper 越上界", sub: withRef(map[string]any{"count": float64(9)}), want: []string{"count"}},
		{name: "stepper 越下界", sub: withRef(map[string]any{"count": float64(0)}), want: []string{"count"}},
		{name: "stepper 非整数", sub: withRef(map[string]any{"count": 1.5}), want: []string{"count"}},
		{name: "stepper 非数值", sub: withRef(map[string]any{"count": "两张"}), want: []string{"count"}},
		{name: "slider 越界", sub: withRef(map[string]any{"strength": 1.5}), want: []string{"strength"}},
		{name: "slider 不对齐 step", sub: withRef(map[string]any{"strength": 0.63}), want: []string{"strength"}},
		{name: "slider 对齐 step 的浮点边角（0.1+0.05*10）", sub: withRef(map[string]any{"strength": 0.6})},
		{name: "textarea 超长", sub: withRef(map[string]any{"negative_prompt": strings.Repeat("字", 501)}), want: []string{"negative_prompt"}},
		{name: "textarea 恰好等于上限", sub: withRef(map[string]any{"negative_prompt": strings.Repeat("字", 500)})},
		{name: "textarea 类型错", sub: withRef(map[string]any{"negative_prompt": float64(1)}), want: []string{"negative_prompt"}},
		{name: "seed 为 null 合法（allow_random）", sub: withRef(map[string]any{"seed": nil})},
		{name: "seed 越界", sub: withRef(map[string]any{"seed": float64(-1)}), want: []string{"seed"}},
		{name: "未知参数 key", sub: withRef(map[string]any{"cfg_scale": float64(7)}), want: []string{"cfg_scale"}},
		{name: "提交了 compound 自身的 key", sub: withRef(map[string]any{"size_group": "1:1"}), want: []string{"size_group"}},
		{
			name: "未知输入槽",
			sub: func() Submission {
				s := withRef(nil)
				s.Inputs["mask"] = []string{"up_9"}
				return s
			}(),
			want: []string{"mask"},
		},
		{
			name: "输入槽超上限",
			sub: func() Submission {
				s := withRef(nil)
				s.Inputs["reference_images"] = []string{"a", "b", "c", "d"}
				return s
			}(),
			want: []string{"reference_images"},
		},
		{
			name: "提示词超长",
			sub: func() Submission {
				s := withRef(nil)
				s.Prompt = strings.Repeat("字", 2001)
				return s
			}(),
			want: []string{"prompt"},
		},
		{
			name: "多个错误一次全报",
			sub:  withRef(map[string]any{"count": float64(9), "style_model": "ghibli", "strength": 5}),
			want: []string{"count", "strength", "style_model"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := errKeys(v.Validate(schema, tc.sub))
			if len(got) != len(tc.want) {
				t.Fatalf("错误字段 %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("错误字段 %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestValidateRequiredSlotAndDisabledOption(t *testing.T) {
	schema := ModelCapabilitySchema{
		ID: "m", Name: "M", Modality: domain.ModalityVideo, Enabled: true,
		Inputs: []InputSlotSpec{{
			Key: "first_frame", Label: "首帧", Kind: "image", Required: true,
			MinCount: 1, MaxCount: 1, Accept: []string{"image/png"}, MaxBytes: 1,
		}},
		Params: []ParamSpec{{
			Key: "quality", Label: "画质", Control: ControlSelect, Group: GroupPrimary, Default: "sd",
			Options: []OptionItem{
				{Value: "sd", Label: "标清"},
				{Value: "hd", Label: "高清", Disabled: true, DisabledReason: "高清暂不可用"},
			},
		}},
		Pricing: PricingSpec{Currency: "credit", Base: 1, Rounding: RoundCeil},
		ETA:     EtaSpec{P50Seconds: 1, P90Seconds: 2},
		Limits:  LimitSpec{MaxConcurrentPerUser: 1, PromptMaxLength: 10},
	}
	v := NewValidator(nil)

	errs := v.Validate(schema, Submission{Params: map[string]any{"quality": "hd"}})
	keys := errKeys(errs)
	if len(keys) != 2 || keys[0] != "first_frame" || keys[1] != "quality" {
		t.Fatalf("got %v, want [first_frame quality]", keys)
	}
	for _, e := range errs {
		if e.Key == "quality" && e.Message != "高清暂不可用" {
			t.Fatalf("禁用选项应回显 disabled_reason，got %q", e.Message)
		}
	}
}

func TestValidateEnabledWhenFalseIsAccepted(t *testing.T) {
	// enabled_when 为假 = 前端渲染成禁用态但仍随表单提交，服务端不应拒。
	schema := loadExamples(t)["hailuo-2-3"]
	sub := Submission{
		Prompt: "海边",
		Params: map[string]any{
			"resolution": "768p", "mode": "standard", "duration": float64(5),
			"camera_motion": "auto", "seed": nil,
		},
	}
	if errs := NewValidator(nil).Validate(schema, sub); len(errs) != 0 {
		t.Fatalf("enabled_when 为假的参数不应被拒: %+v", errs)
	}
}

func TestFieldErrorsWrapper(t *testing.T) {
	if err := NewFieldErrors(nil); err != nil {
		t.Fatalf("空错误列表应返回 nil，got %v", err)
	}
	err := NewFieldErrors([]domain.FieldError{{Key: "count", Message: "越界"}})
	if err == nil {
		t.Fatal("非空错误列表应返回非 nil")
	}
	var fe *FieldErrors
	fe, ok := err.(*FieldErrors)
	if !ok || len(fe.Fields) != 1 || fe.Fields[0].Key != "count" {
		t.Fatalf("类型断言失败: %#v", err)
	}
	if !strings.Contains(err.Error(), "count: 越界") {
		t.Fatalf("Error() 应包含字段信息，got %q", err.Error())
	}
}

func TestLeafParamsFlattensCompound(t *testing.T) {
	schema := loadExamples(t)["seedream-5-pro"]
	keys := make([]string, 0)
	for _, p := range LeafParams(schema.Params) {
		keys = append(keys, p.Key)
	}
	want := []string{"style_model", "aspect", "count", "strength", "negative_prompt", "seed"}
	if len(keys) != len(want) {
		t.Fatalf("got %v, want %v", keys, want)
	}
	for i := range keys {
		if keys[i] != want[i] {
			t.Fatalf("got %v, want %v", keys, want)
		}
	}
}
