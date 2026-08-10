package capability

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// stepEpsilon 是步长对齐判定的容差。
//
// 需要它是因为浮点：slider 声明 min=0.1 step=0.05，用户选 0.6 时
// (0.6-0.1)/0.05 算出来是 10.000000000000002 而不是 10。
// 容差按步数的相对量给，避免大量程参数（如 seed 的 0..2147483647）被误判。
const stepEpsilon = 1e-6

// FieldErrors 是一组按字段定位的校验错误，实现 error 以便沿调用链向上传递。
//
// HTTP 层拿到它之后直接取 Fields 填进 domain.Error.FieldErrors / TaskError.FieldErrors，
// 不需要解析字符串。**不要把它拍平成一条 message**：前端靠 Key 高亮出错的那枚芯片，
// 拍平等于把这个能力扔掉。
type FieldErrors struct {
	Fields []domain.FieldError
}

// Error 实现 error 接口，仅用于日志，不作为对外文案。
func (e *FieldErrors) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Key+": "+f.Message)
	}
	return "capability: 校验失败 [" + strings.Join(parts, "; ") + "]"
}

// NewFieldErrors 把校验结果包成 error；无错误时返回 nil，
// 让调用方可以直接 `if err := NewFieldErrors(v.Validate(...)); err != nil`。
func NewFieldErrors(fields []domain.FieldError) error {
	if len(fields) == 0 {
		return nil
	}
	return &FieldErrors{Fields: fields}
}

// LeafParams 把 compound 展平成叶子参数。
//
// compound 自身不产生 params key，它的 Fields 各自平铺——这是提交语义的一部分，
// 校验、计价上下文、请求映射三处都要按同一份展平结果办事，所以只写这一遍。
func LeafParams(params []ParamSpec) []ParamSpec {
	out := make([]ParamSpec, 0, len(params))
	for _, p := range params {
		if p.Control == ControlCompound {
			out = append(out, LeafParams(p.Fields)...)
			continue
		}
		out = append(out, p)
	}
	return out
}

// validator 是 Validator 的默认实现。
type validator struct{ eval Evaluator }

// NewValidator 返回默认校验器。传 nil 时用 NewEvaluator()，
// 保证可见性判定与计价、前端渲染用的是同一套条件语义。
func NewValidator(e Evaluator) Validator {
	if e == nil {
		e = NewEvaluator()
	}
	return validator{eval: e}
}

// Validate 校验一次提交，收集**全部**错误后一次返回。
//
// 关于 MIME 与像素约束（InputSlotSpec.Accept / MinPixels / MaxPixels）：
// Submission.Inputs 里只有 upload_id / asset_id，没有文件本身的元信息，
// 这里判不了——判了校验器就不再是纯函数，还要多一次按 id 回查存储的 IO。
// 它们的服务端把关在 HTTP 层：提交任务时本来就要按 id 取一遍素材做归属校验，
// 顺路拿到 MIME 与宽高（见 httpapi.assertInputRefs）。
// 因此本方法只校验**槽的存在性、数量与槽间依赖**。
//
// 关于 EnabledWhen 为假的参数：**不拒绝**。前端把它渲染成禁用态但仍会随表单提交
// （submitParams 只按 VisibleWhen 过滤），拒绝它会把所有带 enabled_when 的模型
// 变成必然失败。禁用态的语义是「不让用户改」，不是「不许出现」。
func (v validator) Validate(schema ModelCapabilitySchema, sub Submission) []domain.FieldError {
	var errs []domain.FieldError

	ctx := EvalContext{Params: sub.Params, Inputs: sub.Inputs}

	if schema.Limits.PromptMaxLength > 0 {
		// 用 rune 数而不是字节数。前端用的是 UTF-16 code unit 数，
		// rune 数恒 ≤ 它，因此服务端只会比前端更宽松，不会出现
		// 「前端放行、服务端打回」这种用户无从修正的情况。
		if n := utf8.RuneCountInString(sub.Prompt); n > schema.Limits.PromptMaxLength {
			errs = append(errs, domain.FieldError{
				Key:     "prompt",
				Message: fmt.Sprintf("提示词最长 %d 字，当前 %d", schema.Limits.PromptMaxLength, n),
			})
		}
	}

	errs = append(errs, v.validateInputs(schema, sub)...)
	errs = append(errs, v.validateParams(schema, sub, ctx)...)
	return errs
}

func (v validator) validateInputs(schema ModelCapabilitySchema, sub Submission) []domain.FieldError {
	var errs []domain.FieldError

	slots := make(map[string]InputSlotSpec, len(schema.Inputs))
	for _, s := range schema.Inputs {
		slots[s.Key] = s
	}

	for _, key := range sortedKeysOfStrings(sub.Inputs) {
		if _, ok := slots[key]; !ok {
			errs = append(errs, domain.FieldError{Key: key, Message: "该模型没有名为 " + key + " 的输入槽"})
		}
	}

	for _, slot := range schema.Inputs {
		n := len(sub.Inputs[slot.Key])
		switch {
		case slot.Required && n < max(1, slot.MinCount):
			errs = append(errs, domain.FieldError{
				Key:     slot.Key,
				Message: fmt.Sprintf("%s为必填，至少 %d 个", slot.Label, max(1, slot.MinCount)),
			})
		case !slot.Required && n > 0 && n < slot.MinCount:
			errs = append(errs, domain.FieldError{
				Key:     slot.Key,
				Message: fmt.Sprintf("%s至少 %d 个", slot.Label, slot.MinCount),
			})
		case slot.MaxCount > 0 && n > slot.MaxCount:
			errs = append(errs, domain.FieldError{
				Key:     slot.Key,
				Message: fmt.Sprintf("%s最多 %d 个", slot.Label, slot.MaxCount),
			})
		}
		if n > 0 && slot.RequiresSlot != "" && len(sub.Inputs[slot.RequiresSlot]) == 0 {
			errs = append(errs, domain.FieldError{
				Key:     slot.Key,
				Message: fmt.Sprintf("%s需要同时提供%s", slot.Label, slotLabel(schema, slot.RequiresSlot)),
			})
		}
	}
	return errs
}

// slotLabel 把槽 key 换成给人看的名字；找不到就退回 key——
// 依赖指向不存在的槽由 ValidateSchema 负责报，这里不该再报一次。
func slotLabel(schema ModelCapabilitySchema, key string) string {
	for _, s := range schema.Inputs {
		if s.Key == key {
			return s.Label
		}
	}
	return key
}

func (v validator) validateParams(schema ModelCapabilitySchema, sub Submission, ctx EvalContext) []domain.FieldError {
	var errs []domain.FieldError

	leaves := LeafParams(schema.Params)
	known := make(map[string]struct{}, len(leaves))
	for _, spec := range leaves {
		known[spec.Key] = struct{}{}
	}
	// compound 自身不产生 key，提交里出现它就是前端展平逻辑坏了，必须报出来
	// 而不是当未知 key 一笔带过——两种错误的修法完全不同。
	compounds := map[string]struct{}{}
	collectCompoundKeys(schema.Params, compounds)

	for _, key := range sortedKeysOfAny(sub.Params) {
		if _, ok := compounds[key]; ok {
			errs = append(errs, domain.FieldError{Key: key, Message: "复合控件本身不接受取值，应提交其子字段"})
			continue
		}
		if _, ok := known[key]; !ok {
			errs = append(errs, domain.FieldError{Key: key, Message: "该模型没有名为 " + key + " 的参数"})
		}
	}

	for _, spec := range leaves {
		visible, err := EvalOptional(v.eval, spec.VisibleWhen, ctx)
		if err != nil {
			errs = append(errs, domain.FieldError{Key: spec.Key, Message: "可见性条件求值失败: " + err.Error()})
			continue
		}
		value, present := sub.Params[spec.Key]

		if !visible {
			if present {
				errs = append(errs, domain.FieldError{
					Key:     spec.Key,
					Message: spec.Label + "在当前条件下不适用，不应提交",
				})
			}
			continue
		}
		if !present {
			errs = append(errs, domain.FieldError{Key: spec.Key, Message: spec.Label + "为必填"})
			continue
		}
		if msg := checkValue(spec, value); msg != "" {
			errs = append(errs, domain.FieldError{Key: spec.Key, Message: msg})
		}
	}
	return errs
}

// checkValue 判定单个取值是否落在 ParamSpec 声明的域内，返回空串表示合法。
func checkValue(spec ParamSpec, value any) string {
	switch spec.Control {
	case ControlSelect, ControlAspectSelect:
		for _, opt := range spec.Options {
			if !strictEquals(value, opt.Value) {
				continue
			}
			if opt.Disabled {
				if opt.DisabledReason != "" {
					return opt.DisabledReason
				}
				return "选项「" + opt.Label + "」当前不可用"
			}
			return ""
		}
		return spec.Label + "的取值不在可选项内"

	case ControlToggle:
		if _, ok := value.(bool); !ok {
			return spec.Label + "必须是布尔值"
		}
		return ""

	case ControlTextarea:
		s, ok := value.(string)
		if !ok {
			return spec.Label + "必须是文本"
		}
		if spec.MaxLength != nil && utf8.RuneCountInString(s) > *spec.MaxLength {
			return fmt.Sprintf("%s最长 %d 字", spec.Label, *spec.MaxLength)
		}
		return ""

	case ControlSeed:
		// Default 为 nil 表示每次随机，因此 null 是 seed 的合法取值。
		if value == nil {
			if spec.AllowRandom {
				return ""
			}
			return spec.Label + "不接受空值"
		}
		return checkNumeric(spec, value, true)

	case ControlStepper:
		return checkNumeric(spec, value, true)

	case ControlSlider:
		return checkNumeric(spec, value, false)
	}
	// 到这里说明 Control 是 ValidateSchema 认可之外的值；配置层已经拦过一道，
	// 提交期不再二次判定取值，透传给 mapping。
	return ""
}

func checkNumeric(spec ParamSpec, value any, wantInteger bool) string {
	n, ok := rawNumber(value)
	if !ok {
		return spec.Label + "必须是数值"
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return spec.Label + "必须是有限数值"
	}
	if spec.Min != nil && n < *spec.Min {
		return fmt.Sprintf("%s不能小于 %s", spec.Label, formatNumber(*spec.Min))
	}
	if spec.Max != nil && n > *spec.Max {
		return fmt.Sprintf("%s不能大于 %s", spec.Label, formatNumber(*spec.Max))
	}
	if wantInteger && n != math.Trunc(n) {
		return spec.Label + "必须是整数"
	}
	if spec.Step != nil && *spec.Step > 0 {
		base := 0.0
		if spec.Min != nil {
			base = *spec.Min
		}
		steps := (n - base) / *spec.Step
		if math.Abs(steps-math.Round(steps)) > stepEpsilon*math.Max(1, math.Abs(steps)) {
			return fmt.Sprintf("%s必须按 %s 步进", spec.Label, formatNumber(*spec.Step))
		}
	}
	return ""
}

func collectCompoundKeys(params []ParamSpec, out map[string]struct{}) {
	for _, p := range params {
		if p.Control != ControlCompound {
			continue
		}
		out[p.Key] = struct{}{}
		collectCompoundKeys(p.Fields, out)
	}
}

// sortedKeysOfAny / sortedKeysOfStrings 让错误列表的顺序稳定。
// map 遍历顺序随机会让同一份错误提交每次返回不同顺序的 FieldError，
// 前端的高亮顺序跟着抖，测试也没法断言。
func sortedKeysOfAny(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeysOfStrings(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
