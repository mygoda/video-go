package mapping

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/capability"
)

// 取值源前缀。写成常量而不是散在 switch 里的字面量，是因为它们是对外契约的一部分：
// 管理后台填 request_mapping 时照着这几个前缀写，改动等于改契约。
const (
	sourcePrompt        = "prompt"
	sourceParamsPrefix  = "params."
	sourceInputsPrefix  = "inputs."
	sourceUpstreamModel = "model.upstream_model"
)

// renderer 是 Renderer 的默认实现。它只持有一个 Evaluator，无可变状态，
// 因此同一个实例可以被并发的多个任务共用（driver 就是这么用的）。
type renderer struct{ eval capability.Evaluator }

// NewRenderer 返回默认渲染器。传 nil 时用 capability.NewEvaluator()，
// 兑现「When 求值用与 L1 同一个 Evaluator，不另起一套」这条约定。
func NewRenderer(e capability.Evaluator) Renderer {
	if e == nil {
		e = capability.NewEvaluator()
	}
	return renderer{eval: e}
}

// Render 把 RequestMapping 渲染成上游请求体。
func (r renderer) Render(m RequestMapping, ctx RenderContext) (map[string]any, error) {
	body := deepCopyMap(m.BodyTemplate)

	evalCtx := capability.EvalContext{Params: ctx.Params, Inputs: ctx.InputURLs}

	for i, rule := range m.Rules {
		if err := r.applyRule(body, rule, ctx, evalCtx); err != nil {
			return nil, fmt.Errorf("mapping: 规则 #%d(to=%s) 渲染失败: %w", i, rule.To, err)
		}
	}
	return body, nil
}

func (r renderer) applyRule(body map[string]any, rule MappingRule, ctx RenderContext, evalCtx capability.EvalContext) error {
	ok, err := capability.EvalOptional(r.eval, rule.When, evalCtx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	path, appendMode, err := parseTarget(rule.To)
	if err != nil {
		return err
	}

	values, err := sourceValues(rule, ctx)
	if err != nil {
		return err
	}

	// 取到空值就不写这个字段，而不是往上游塞 null——
	// 不少上游对显式 null 和字段缺失的处理不一样。
	if omitWhenEmpty(rule) {
		values = dropEmpty(values)
		if len(values) == 0 {
			return nil
		}
	}

	transformed := make([]any, 0, len(values))
	for _, v := range values {
		v = applyValueMap(rule.ValueMap, v)
		v, err = applyCast(rule.Cast, v)
		if err != nil {
			return err
		}
		v, err = applyWrap(rule.Wrap, v)
		if err != nil {
			return err
		}
		transformed = append(transformed, v)
	}

	if appendMode {
		// 追加顺序即规则顺序（见包注释：Ark 的 content 数组顺序影响生成语义）。
		return appendPath(body, path, transformed)
	}

	// 标量目标遇到多值来源:报错而不是取第一个。
	// 静默丢掉第 2、3 张参考图会让用户看着三张图提交、拿回一张图的结果，
	// 而日志里什么都没有。这属于 mapping 配置写错（该用 "xxx[]"），
	// 在渲染期炸掉比在生成结果里体现要好排查得多。
	if len(transformed) > 1 {
		return fmt.Errorf("来源 %s 有 %d 个值，但目标 %q 不是数组（数组目标需以 [] 结尾）", rule.From, len(transformed), rule.To)
	}
	return setPath(body, path, transformed[0])
}

// sourceValues 取出规则的源值。返回切片是因为 inputs.<slot> 天然是多值，
// 其余来源恒为单值。
func sourceValues(rule MappingRule, ctx RenderContext) ([]any, error) {
	if rule.From == "" {
		if rule.Const == nil {
			return nil, fmt.Errorf("规则必须声明 from 或 const")
		}
		return []any{rule.Const}, nil
	}
	if rule.Const != nil {
		return nil, fmt.Errorf("from 与 const 只能二选一")
	}

	switch {
	case rule.From == sourcePrompt:
		return []any{ctx.Prompt}, nil
	case rule.From == sourceUpstreamModel:
		return []any{ctx.UpstreamModel}, nil
	case strings.HasPrefix(rule.From, sourceParamsPrefix):
		key := strings.TrimPrefix(rule.From, sourceParamsPrefix)
		if key == "" {
			return nil, fmt.Errorf("params. 后必须跟参数名")
		}
		// 缺键返回 nil 而不是报错：VisibleWhen 为假的参数本来就不在提交体里，
		// 配 OmitWhenEmpty 就是「该参数不适用时不往上游塞」的正常路径。
		return []any{ctx.Params[key]}, nil
	case strings.HasPrefix(rule.From, sourceInputsPrefix):
		slot := strings.TrimPrefix(rule.From, sourceInputsPrefix)
		if slot == "" {
			return nil, fmt.Errorf("inputs. 后必须跟输入槽名")
		}
		urls := ctx.InputURLs[slot]
		out := make([]any, 0, len(urls))
		for _, u := range urls {
			out = append(out, u)
		}
		if len(out) == 0 {
			// 让空槽走与空参数相同的 OmitWhenEmpty 路径。
			return []any{nil}, nil
		}
		return out, nil
	}
	return nil, fmt.Errorf("未知取值源 %q（支持 prompt / params.<key> / inputs.<slot> / model.upstream_model）", rule.From)
}

func omitWhenEmpty(rule MappingRule) bool {
	if rule.OmitWhenEmpty == nil {
		return true
	}
	return *rule.OmitWhenEmpty
}

// dropEmpty 剔除空值。**0 与 false 不算空**——它们是合法取值，
// 把 seed=0 或 watermark=false 当空丢掉是实打实的语义错误。
func dropEmpty(values []any) []any {
	out := values[:0:0]
	for _, v := range values {
		switch t := v.(type) {
		case nil:
			continue
		case string:
			if t == "" {
				continue
			}
		case []any:
			if len(t) == 0 {
				continue
			}
		case map[string]any:
			if len(t) == 0 {
				continue
			}
		}
		out = append(out, v)
	}
	return out
}

// applyValueMap 做平台取值 → 上游取值的替换。
//
// 键按 JSON 的字符串形态查找，因此 {"5": "5s"} 能命中数值 5——
// JSON 对象的键只能是字符串，而平台侧的 duration 是数字，
// 不这样查这条映射永远写不出来。
func applyValueMap(vm map[string]any, v any) any {
	if len(vm) == 0 {
		return v
	}
	if mapped, ok := vm[stringify(v)]; ok {
		return mapped
	}
	return v
}

func applyCast(c Cast, v any) (any, error) {
	switch c {
	case "":
		return v, nil
	case CastString:
		return stringify(v), nil
	case CastInt:
		n, ok := toNumber(v)
		if !ok {
			return nil, fmt.Errorf("值 %v 无法转为整数", v)
		}
		return int64(math.Trunc(n)), nil
	case CastFloat:
		n, ok := toNumber(v)
		if !ok {
			return nil, fmt.Errorf("值 %v 无法转为浮点数", v)
		}
		return n, nil
	case CastBool:
		return toBool(v)
	case CastSecondsSuffix:
		n, ok := toNumber(v)
		if !ok {
			return nil, fmt.Errorf("值 %v 无法转为秒数", v)
		}
		return formatNumber(n) + "s", nil
	}
	return nil, fmt.Errorf("未知 cast %q", c)
}

// applyWrap 套上多模态数组元素的类型壳子。
//
// 契约文字说 Wrap「仅在 To 以 [] 结尾时有意义」，但这里对标量目标同样生效:
// Wrap 本质是一个值变换，按目标形态区别对待只会制造「配了没生效且不报错」
// 这种最难查的情况。
func applyWrap(w Wrap, v any) (any, error) {
	switch w {
	case "", WrapRaw:
		return v, nil
	case WrapTextPart:
		return map[string]any{"type": "text", "text": stringify(v)}, nil
	case WrapImageURLPart:
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": stringify(v)}}, nil
	case WrapVideoURLPart:
		return map[string]any{"type": "video_url", "video_url": map[string]any{"url": stringify(v)}}, nil
	case WrapUserMessage:
		return map[string]any{"role": "user", "content": stringify(v)}, nil
	}
	return nil, fmt.Errorf("未知 wrap %q", w)
}

// parseTarget 拆解 To：点号分隔的路径 + 末尾 "[]" 表示追加进数组。
func parseTarget(to string) ([]string, bool, error) {
	if to == "" {
		return nil, false, fmt.Errorf("规则必须声明 to")
	}
	appendMode := strings.HasSuffix(to, "[]")
	trimmed := strings.TrimSuffix(to, "[]")
	if strings.Contains(trimmed, "[]") {
		return nil, false, fmt.Errorf("to 中的 [] 只能出现在末尾")
	}
	parts := strings.Split(trimmed, ".")
	for _, p := range parts {
		if p == "" {
			return nil, false, fmt.Errorf("to 路径中出现空段")
		}
	}
	return parts, appendMode, nil
}

func setPath(body map[string]any, path []string, value any) error {
	parent, err := descend(body, path)
	if err != nil {
		return err
	}
	parent[path[len(path)-1]] = value
	return nil
}

func appendPath(body map[string]any, path []string, values []any) error {
	parent, err := descend(body, path)
	if err != nil {
		return err
	}
	leaf := path[len(path)-1]
	existing, present := parent[leaf]
	if !present || existing == nil {
		parent[leaf] = append([]any{}, values...)
		return nil
	}
	arr, ok := existing.([]any)
	if !ok {
		return fmt.Errorf("路径 %s 上已存在非数组值（%T），无法追加", strings.Join(path, "."), existing)
	}
	parent[leaf] = append(arr, values...)
	return nil
}

// descend 走到倒数第二段，沿途缺失的中间对象按需创建。
func descend(body map[string]any, path []string) (map[string]any, error) {
	cur := body
	for i, seg := range path[:len(path)-1] {
		next, present := cur[seg]
		if !present || next == nil {
			child := map[string]any{}
			cur[seg] = child
			cur = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("路径 %s 上已存在非对象值（%T），无法继续下钻", strings.Join(path[:i+1], "."), next)
		}
		cur = child
	}
	return cur, nil
}

// deepCopyMap 保证渲染绝不就地修改配置对象:
// 同一份 ModelConfig 会被并发的多个任务共用，就地改一次就是全局污染。
func deepCopyMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = deepCopyValue(v)
	}
	return dst
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopyValue(e)
		}
		return out
	}
	return v
}

// stringify / toNumber / toBool / formatNumber 与 capability 包里的同名私有助手
// 语义一致（都复刻 JS 的 String() / Number()）。没有共用一份是因为那些是
// L1 与前端逐位对齐的实现细节，导出它们会把一条内部约定变成跨包契约。
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	}
	if f, ok := toNumber(v); ok {
		return formatNumber(f)
	}
	return fmt.Sprint(v)
}

func formatNumber(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func toNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint:
		return float64(t), true
	case uint32:
		return float64(t), true
	case uint64:
		return float64(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// toBool 只认无歧义的取值。刻意不抄 JS 的 truthy——JS 里 "false" 是真，
// 而 mapping 配置里出现字符串 "false" 时作者想表达的一定是假。
func toBool(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case nil:
		return false, nil
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		if err != nil {
			return false, fmt.Errorf("字符串 %q 无法转为布尔值", t)
		}
		return b, nil
	}
	n, ok := toNumber(v)
	if !ok {
		return false, fmt.Errorf("值 %v 无法转为布尔值", v)
	}
	return n != 0, nil
}
