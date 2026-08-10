package capability

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// templatePlaceholder 匹配 compound 的 DisplayTemplate 占位符，如 "{aspect}"。
var templatePlaceholder = regexp.MustCompile(`\{(\w+)\}`)

// controlRules 声明每种控件**必须携带**哪些字段。
//
// 包注释里那条取舍（ParamSpec 是全控件字段的并集，类型系统不阻止给 toggle 填
// Options）就在这张表里兑现：类型系统放弃的约束，改由这里在落库前拦。
// 写错配置应当在管理后台点保存那一刻就被拒，而不是等某个用户提交任务时才炸。
var controlRules = map[ControlKind]struct {
	needOptions bool
	needRange   bool // Min + Max
	needStep    bool
	needLength  bool
	needFields  bool
}{
	ControlSelect:       {needOptions: true},
	ControlAspectSelect: {needOptions: true},
	ControlStepper:      {needRange: true, needStep: true},
	ControlSlider:       {needRange: true, needStep: true},
	ControlSeed:         {needRange: true},
	ControlTextarea:     {needLength: true},
	ControlToggle:       {},
	ControlCompound:     {needFields: true},
}

// ValidateSchema 校验一份 ModelCapabilitySchema 自身是否自洽。
//
// 与 Validate 一样收集全部错误再返回。FieldError.Key 用 "params[key].field"
// 这类路径而不是裸 key：管理后台编辑的是整份 schema，只给一个 "options"
// 管理员不知道是哪个参数的 options。
func (v validator) ValidateSchema(schema ModelCapabilitySchema) []domain.FieldError {
	var errs []domain.FieldError

	if strings.TrimSpace(schema.ID) == "" {
		errs = append(errs, domain.FieldError{Key: "id", Message: "模型 id 不能为空"})
	}
	if strings.TrimSpace(schema.Name) == "" {
		errs = append(errs, domain.FieldError{Key: "name", Message: "模型名称不能为空"})
	}
	switch schema.Modality {
	case domain.ModalityImage, domain.ModalityVideo, domain.ModalityText:
	default:
		errs = append(errs, domain.FieldError{Key: "modality", Message: "modality 只能是 image、video 或 text"})
	}
	if !schema.Enabled && strings.TrimSpace(schema.DisabledReason) == "" {
		errs = append(errs, domain.FieldError{Key: "disabled_reason", Message: "禁用的模型必须给出 disabled_reason，否则前端只能置灰而说不出原因"})
	}

	slotKeys := map[string]struct{}{}
	for i, slot := range schema.Inputs {
		errs = append(errs, validateSlot(i, slot, slotKeys)...)
	}

	paramKeys := map[string]struct{}{}
	errs = append(errs, validateParamSpecs("params", schema.Params, paramKeys, slotKeys)...)

	errs = append(errs, validatePricingSpec(schema.Pricing, paramKeys, slotKeys)...)

	if schema.ETA.P50Seconds <= 0 {
		errs = append(errs, domain.FieldError{Key: "eta.p50_seconds", Message: "p50_seconds 必须为正"})
	}
	if schema.ETA.P90Seconds < schema.ETA.P50Seconds {
		errs = append(errs, domain.FieldError{Key: "eta.p90_seconds", Message: "p90_seconds 不能小于 p50_seconds"})
	}
	if schema.ETA.ScalesWith != "" {
		if _, ok := paramKeys[schema.ETA.ScalesWith]; !ok {
			errs = append(errs, domain.FieldError{Key: "eta.scales_with", Message: "scales_with 指向了不存在的参数 " + schema.ETA.ScalesWith})
		}
	}

	if schema.Limits.PromptMaxLength <= 0 {
		errs = append(errs, domain.FieldError{Key: "limits.prompt_max_length", Message: "prompt_max_length 必须为正"})
	}
	if schema.Limits.MaxConcurrentPerUser <= 0 {
		errs = append(errs, domain.FieldError{Key: "limits.max_concurrent_per_user", Message: "max_concurrent_per_user 必须为正"})
	}
	return errs
}

func validateSlot(i int, slot InputSlotSpec, seen map[string]struct{}) []domain.FieldError {
	var errs []domain.FieldError
	path := fmt.Sprintf("inputs[%d]", i)
	if strings.TrimSpace(slot.Key) == "" {
		errs = append(errs, domain.FieldError{Key: path + ".key", Message: "输入槽 key 不能为空"})
	} else if _, dup := seen[slot.Key]; dup {
		errs = append(errs, domain.FieldError{Key: path + ".key", Message: "输入槽 key 重复: " + slot.Key})
	} else {
		seen[slot.Key] = struct{}{}
	}
	switch slot.Kind {
	case "image", "video", "audio":
	default:
		errs = append(errs, domain.FieldError{Key: path + ".kind", Message: "kind 只能是 image / video / audio"})
	}
	if slot.MinCount < 0 {
		errs = append(errs, domain.FieldError{Key: path + ".min_count", Message: "min_count 不能为负"})
	}
	if slot.MaxCount < 1 {
		errs = append(errs, domain.FieldError{Key: path + ".max_count", Message: "max_count 至少为 1"})
	} else if slot.MinCount > slot.MaxCount {
		errs = append(errs, domain.FieldError{Key: path + ".min_count", Message: "min_count 不能大于 max_count"})
	}
	if slot.MaxBytes <= 0 {
		errs = append(errs, domain.FieldError{Key: path + ".max_bytes", Message: "max_bytes 必须为正"})
	}
	if len(slot.Accept) == 0 {
		errs = append(errs, domain.FieldError{Key: path + ".accept", Message: "accept 不能为空，否则前端无法预校验文件类型"})
	}
	// 只允许指向**已声明过**的槽：既拦住写错的 key，也让依赖不可能成环。
	// 本槽的 key 在上面已经进了 seen，所以自引用要单独挡一道。
	if slot.RequiresSlot != "" {
		_, known := seen[slot.RequiresSlot]
		if !known || slot.RequiresSlot == slot.Key {
			errs = append(errs, domain.FieldError{
				Key:     path + ".requires_slot",
				Message: "requires_slot 指向了不存在或声明在其后的输入槽 " + slot.RequiresSlot,
			})
		}
	}
	return errs
}

func validateParamSpecs(path string, params []ParamSpec, keys map[string]struct{}, slots map[string]struct{}) []domain.FieldError {
	var errs []domain.FieldError
	for i, spec := range params {
		p := fmt.Sprintf("%s[%d]", path, i)
		if strings.TrimSpace(spec.Key) == "" {
			errs = append(errs, domain.FieldError{Key: p + ".key", Message: "参数 key 不能为空"})
		} else if _, dup := keys[spec.Key]; dup {
			errs = append(errs, domain.FieldError{Key: p + ".key", Message: "参数 key 重复: " + spec.Key})
		} else {
			keys[spec.Key] = struct{}{}
		}
		if strings.TrimSpace(spec.Label) == "" {
			errs = append(errs, domain.FieldError{Key: p + ".label", Message: "参数 label 不能为空"})
		}
		if spec.Group != GroupPrimary && spec.Group != GroupAdvanced {
			errs = append(errs, domain.FieldError{Key: p + ".group", Message: "group 只能是 primary 或 advanced"})
		}
		if spec.EnabledWhen != nil && strings.TrimSpace(spec.DisabledHint) == "" {
			errs = append(errs, domain.FieldError{Key: p + ".disabled_hint", Message: "声明了 enabled_when 就必须给 disabled_hint，否则禁用态说不出原因"})
		}
		errs = append(errs, validateControlShape(p, spec)...)
		errs = append(errs, validateCondition(p+".visible_when", spec.VisibleWhen, slots)...)
		errs = append(errs, validateCondition(p+".enabled_when", spec.EnabledWhen, slots)...)

		if spec.Control == ControlCompound {
			errs = append(errs, validateParamSpecs(p+".fields", spec.Fields, keys, slots)...)
			errs = append(errs, validateTemplate(p, spec)...)
		}
	}
	return errs
}

// validateControlShape 兑现「每种 Control 必须携带它要求的字段，
// 且不得携带其他控件专属的字段」。
func validateControlShape(path string, spec ParamSpec) []domain.FieldError {
	var errs []domain.FieldError
	rule, known := controlRules[spec.Control]
	if !known {
		return append(errs, domain.FieldError{Key: path + ".control", Message: "未知控件类型 " + string(spec.Control)})
	}

	if rule.needOptions {
		if len(spec.Options) == 0 {
			errs = append(errs, domain.FieldError{Key: path + ".options", Message: string(spec.Control) + " 必须声明 options"})
		}
		for j, opt := range spec.Options {
			if !isPrimitive(opt.Value) {
				errs = append(errs, domain.FieldError{Key: fmt.Sprintf("%s.options[%d].value", path, j), Message: "选项取值必须是标量"})
			}
			if strings.TrimSpace(opt.Label) == "" {
				errs = append(errs, domain.FieldError{Key: fmt.Sprintf("%s.options[%d].label", path, j), Message: "选项 label 不能为空"})
			}
			if opt.Disabled && strings.TrimSpace(opt.DisabledReason) == "" {
				errs = append(errs, domain.FieldError{Key: fmt.Sprintf("%s.options[%d].disabled_reason", path, j), Message: "禁用的选项必须给出 disabled_reason"})
			}
			if spec.Control == ControlAspectSelect && (opt.RatioW == nil || opt.RatioH == nil) {
				errs = append(errs, domain.FieldError{Key: fmt.Sprintf("%s.options[%d]", path, j), Message: "aspect_select 的选项必须带 ratio_w / ratio_h"})
			}
		}
	} else if len(spec.Options) > 0 {
		errs = append(errs, domain.FieldError{Key: path + ".options", Message: string(spec.Control) + " 不接受 options"})
	}

	if !rule.needOptions && spec.RenderHint != "" {
		errs = append(errs, domain.FieldError{Key: path + ".render_hint", Message: "render_hint 仅 select / aspect_select 可用"})
	}
	if spec.RenderHint != "" && spec.RenderHint != RenderDropdown && spec.RenderHint != RenderSegmented {
		errs = append(errs, domain.FieldError{Key: path + ".render_hint", Message: "render_hint 只能是 dropdown 或 segmented"})
	}

	if rule.needRange {
		switch {
		case spec.Min == nil:
			errs = append(errs, domain.FieldError{Key: path + ".min", Message: string(spec.Control) + " 必须声明 min"})
		case spec.Max == nil:
			errs = append(errs, domain.FieldError{Key: path + ".max", Message: string(spec.Control) + " 必须声明 max"})
		case *spec.Min > *spec.Max:
			errs = append(errs, domain.FieldError{Key: path + ".min", Message: "min 不能大于 max"})
		}
	} else if spec.Min != nil || spec.Max != nil {
		errs = append(errs, domain.FieldError{Key: path + ".min", Message: string(spec.Control) + " 不接受 min / max"})
	}

	if rule.needStep {
		if spec.Step == nil || *spec.Step <= 0 {
			errs = append(errs, domain.FieldError{Key: path + ".step", Message: string(spec.Control) + " 必须声明正的 step"})
		}
	} else if spec.Step != nil {
		errs = append(errs, domain.FieldError{Key: path + ".step", Message: string(spec.Control) + " 不接受 step"})
	}

	if rule.needLength {
		if spec.MaxLength == nil || *spec.MaxLength <= 0 {
			errs = append(errs, domain.FieldError{Key: path + ".max_length", Message: "textarea 必须声明正的 max_length"})
		}
	} else if spec.MaxLength != nil {
		errs = append(errs, domain.FieldError{Key: path + ".max_length", Message: string(spec.Control) + " 不接受 max_length"})
	}

	if spec.Control != ControlTextarea && spec.Placeholder != "" {
		errs = append(errs, domain.FieldError{Key: path + ".placeholder", Message: "placeholder 仅 textarea 可用"})
	}
	if spec.Control != ControlSeed && spec.AllowRandom {
		errs = append(errs, domain.FieldError{Key: path + ".allow_random", Message: "allow_random 仅 seed 可用"})
	}
	if spec.Control != ControlStepper && spec.Control != ControlSlider && spec.Unit != "" {
		errs = append(errs, domain.FieldError{Key: path + ".unit", Message: "unit 仅 stepper / slider 可用"})
	}

	if rule.needFields {
		if len(spec.Fields) == 0 {
			errs = append(errs, domain.FieldError{Key: path + ".fields", Message: "compound 必须声明 fields"})
		}
		if spec.Default != nil {
			errs = append(errs, domain.FieldError{Key: path + ".default", Message: "compound 自身 default 必须为 null，真实默认值在各 field 内"})
		}
	} else {
		if len(spec.Fields) > 0 {
			errs = append(errs, domain.FieldError{Key: path + ".fields", Message: string(spec.Control) + " 不接受 fields"})
		}
		if spec.DisplayTemplate != "" {
			errs = append(errs, domain.FieldError{Key: path + ".display_template", Message: "display_template 仅 compound 可用"})
		}
		// default 必须在自己声明的域内，否则前端一开表单就是非法态、
		// 「旧值非法回落到 default」这条兜底也没了落点。
		if msg := checkValue(spec, spec.Default); msg != "" {
			errs = append(errs, domain.FieldError{Key: path + ".default", Message: "默认值不合法: " + msg})
		}
	}
	return errs
}

func validateTemplate(path string, spec ParamSpec) []domain.FieldError {
	var errs []domain.FieldError
	if strings.TrimSpace(spec.DisplayTemplate) == "" {
		return append(errs, domain.FieldError{Key: path + ".display_template", Message: "compound 必须声明 display_template"})
	}
	own := map[string]struct{}{}
	for _, f := range spec.Fields {
		own[f.Key] = struct{}{}
	}
	for _, m := range templatePlaceholder.FindAllStringSubmatch(spec.DisplayTemplate, -1) {
		if _, ok := own[m[1]]; !ok {
			errs = append(errs, domain.FieldError{Key: path + ".display_template", Message: "占位符 {" + m[1] + "} 不对应任何子字段"})
		}
	}
	return errs
}

func validatePricingSpec(spec PricingSpec, params, slots map[string]struct{}) []domain.FieldError {
	var errs []domain.FieldError
	if spec.Currency != "credit" {
		errs = append(errs, domain.FieldError{Key: "pricing.currency", Message: "currency 目前只支持 credit"})
	}
	if spec.Base < 0 {
		errs = append(errs, domain.FieldError{Key: "pricing.base", Message: "base 不能为负"})
	}
	switch spec.Rounding {
	case RoundCeil, RoundRound, RoundFloor:
	default:
		errs = append(errs, domain.FieldError{Key: "pricing.rounding", Message: "rounding 只能是 ceil / round / floor"})
	}
	for i, m := range spec.Modifiers {
		p := fmt.Sprintf("pricing.modifiers[%d]", i)
		if m.Op != PricingMul && m.Op != PricingAdd {
			errs = append(errs, domain.FieldError{Key: p + ".op", Message: "计价算子只能是 mul 或 add"})
		}
		errs = append(errs, validateCondition(p+".when", &m.When, slots)...)
	}
	if spec.MultiplierParam != "" {
		if _, ok := params[spec.MultiplierParam]; !ok {
			errs = append(errs, domain.FieldError{Key: "pricing.multiplier_param", Message: "multiplier_param 指向了不存在的参数 " + spec.MultiplierParam})
		}
	}
	return errs
}

// validateCondition 递归检查条件表达式的结构自洽，并核对 has_input 的 slot 真实存在。
// slot 写错是最容易犯的错（复制粘贴另一个模型的配置），而它的后果是隐式分流静默失效：
// 用户传了图，但那条 when 永不命中，于是走了文生图路径且没有任何报错。
func validateCondition(path string, cond *Condition, slots map[string]struct{}) []domain.FieldError {
	if cond == nil {
		return nil
	}
	var errs []domain.FieldError
	switch cond.Op {
	case OpEq, OpNe:
		if cond.Key == "" {
			errs = append(errs, domain.FieldError{Key: path, Message: string(cond.Op) + " 条件必须声明 key"})
		}
		if !isPrimitive(cond.Value) {
			errs = append(errs, domain.FieldError{Key: path, Message: string(cond.Op) + " 条件的 value 必须是标量"})
		}
	case OpGt, OpLt:
		if cond.Key == "" {
			errs = append(errs, domain.FieldError{Key: path, Message: string(cond.Op) + " 条件必须声明 key"})
		}
		if _, ok := rawNumber(cond.Value); !ok {
			errs = append(errs, domain.FieldError{Key: path, Message: string(cond.Op) + " 条件的 value 必须是数值"})
		}
	case OpIn, OpNin:
		if cond.Key == "" {
			errs = append(errs, domain.FieldError{Key: path, Message: string(cond.Op) + " 条件必须声明 key"})
		}
		list, ok := cond.Value.([]any)
		if !ok {
			errs = append(errs, domain.FieldError{Key: path, Message: string(cond.Op) + " 条件的 value 必须是数组"})
			break
		}
		for _, item := range list {
			if !isPrimitive(item) {
				errs = append(errs, domain.FieldError{Key: path, Message: string(cond.Op) + " 条件的数组元素必须是标量"})
				break
			}
		}
	case OpHasInput:
		if cond.Slot == "" {
			errs = append(errs, domain.FieldError{Key: path, Message: "has_input 条件必须声明 slot"})
			break
		}
		if _, ok := slots[cond.Slot]; !ok {
			errs = append(errs, domain.FieldError{Key: path, Message: "has_input 指向了不存在的输入槽 " + cond.Slot})
		}
	case OpAnd, OpOr:
		if len(cond.Of) == 0 {
			errs = append(errs, domain.FieldError{Key: path, Message: string(cond.Op) + " 条件必须至少有一个子条件"})
		}
		for i := range cond.Of {
			errs = append(errs, validateCondition(fmt.Sprintf("%s.of[%d]", path, i), &cond.Of[i], slots)...)
		}
	case OpNot:
		if len(cond.Of) != 1 {
			errs = append(errs, domain.FieldError{Key: path, Message: "not 条件必须且只能有一个子条件"})
			break
		}
		errs = append(errs, validateCondition(path+".of[0]", &cond.Of[0], slots)...)
	default:
		errs = append(errs, domain.FieldError{Key: path, Message: "未知条件算子 " + string(cond.Op)})
	}
	return errs
}
