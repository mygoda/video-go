package mapping

import (
	"strings"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/capability"
)

func param(key string, control capability.ControlKind) capability.ParamSpec {
	return capability.ParamSpec{Key: key, Label: key, Control: control, Group: capability.GroupPrimary}
}

func TestValidateParamCoverage(t *testing.T) {
	cases := []struct {
		name   string
		params []capability.ParamSpec
		rules  []MappingRule
		want   []string // 期望报错的 FieldError.Key，顺序敏感
	}{
		{
			name:   "一一对应",
			params: []capability.ParamSpec{param("duration", capability.ControlStepper), param("ratio", capability.ControlAspectSelect)},
			rules: []MappingRule{
				{From: "model.upstream_model", To: "model"},
				{From: "prompt", To: "input.prompt"},
				{From: "params.duration", To: "input.duration"},
				{From: "params.ratio", To: "input.ratio"},
			},
		},
		{
			name:   "没有参数也没有规则",
			params: nil,
			rules:  []MappingRule{{From: "prompt", To: "input.prompt"}},
		},
		{
			name:   "声明了参数却没有规则：取值到不了上游",
			params: []capability.ParamSpec{param("duration", capability.ControlStepper), param("strength", capability.ControlSlider)},
			rules:  []MappingRule{{From: "params.duration", To: "input.duration"}},
			want:   []string{"params[1].key"},
		},
		{
			name:   "规则指向不存在的参数：字段被静默略过",
			params: []capability.ParamSpec{param("size", capability.ControlSelect)},
			rules: []MappingRule{
				{From: "params.size", To: "input.size"},
				{From: "params.sizee", To: "input.size2"},
			},
			want: []string{"rules[1].from"},
		},
		{
			name: "compound 平铺后按子字段对应",
			params: []capability.ParamSpec{{
				Key: "size_group", Label: "尺寸", Control: capability.ControlCompound, Group: capability.GroupPrimary,
				Fields: []capability.ParamSpec{param("aspect", capability.ControlAspectSelect), param("count", capability.ControlStepper)},
			}},
			rules: []MappingRule{
				{From: "params.aspect", To: "size"},
				{From: "params.count", To: "n"},
			},
		},
		{
			name: "规则指向 compound 自身",
			params: []capability.ParamSpec{{
				Key: "size_group", Label: "尺寸", Control: capability.ControlCompound, Group: capability.GroupPrimary,
				Fields: []capability.ParamSpec{param("aspect", capability.ControlAspectSelect)},
			}},
			rules: []MappingRule{{From: "params.size_group", To: "size"}},
			want:  []string{"rules[0].from", "params[0].key"},
		},
		{
			name:   "同一个参数被多条规则用：不算重复",
			params: []capability.ParamSpec{param("ratio", capability.ControlAspectSelect)},
			rules: []MappingRule{
				{From: "params.ratio", To: "input.ratio"},
				{From: "params.ratio", To: "input.aspect_ratio"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateParamCoverage(
				capability.ModelCapabilitySchema{Params: tc.params},
				RequestMapping{Rules: tc.rules},
			)
			got := make([]string, 0, len(errs))
			for _, e := range errs {
				got = append(got, e.Key)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("错误数量不符\n got=%v\nwant=%v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("第 %d 条错误定位不符\n got=%v\nwant=%v", i, got, tc.want)
				}
			}
		})
	}
}

// TestValidateParamCoverageMessageMentionsKey 保证报错说得出是哪个参数。
// 只给 "rules[3].from" 而不说 key，管理员还得自己数到第 4 条规则。
func TestValidateParamCoverageMessageMentionsKey(t *testing.T) {
	errs := ValidateParamCoverage(
		capability.ModelCapabilitySchema{Params: []capability.ParamSpec{param("seed", capability.ControlSeed)}},
		RequestMapping{Rules: []MappingRule{{From: "params.sed", To: "input.seed"}}},
	)
	if len(errs) != 2 {
		t.Fatalf("want 2 errors, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Message, "sed") || !strings.Contains(errs[1].Message, "seed") {
		t.Fatalf("报错文案没点名参数: %v", errs)
	}
}
