package capability

import (
	"math"
	"strconv"
	"strings"
)

// 这一组取值助手存在的唯一理由是**与前端逐位对齐**。
//
// 前端的 evaluate / costBreakdown 跑在 JS 上，比较用 ===、转数值用 Number()。
// Go 侧如果按自己的直觉写（比如用 reflect.DeepEqual 比较、用类型断言取数），
// 在 `5` 与 `5.0`、`"5"` 与 `5`、`null` 与缺键这几处会与前端分叉。
// 分叉的后果是用户看到 8 积分、被扣 10 积分，属于产品级事故。
// 所以这里把 JS 的语义显式抄一遍，而不是让每个调用点各自即兴发挥。

// numberOf 复刻 JS 的 Number()，只覆盖 encoding/json 能产出的取值域
// 再加上手写 map 可能塞进来的原生整型。
//
// 第二个返回值为 false 表示 JS 侧会得到 NaN。
func numberOf(v any) (float64, bool) {
	switch t := v.(type) {
	case nil:
		// Number(null) === 0。这条看着别扭，但前端就是这么算的。
		return 0, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return rawNumber(v)
}

// rawNumber 只认「本来就是数字」的取值，不做字符串/布尔的隐式转换。
// eq/in 一类的比较必须用它而不是 numberOf：JS 里 "5" === 5 是 false。
func rawNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int8:
		return float64(t), true
	case int16:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint:
		return float64(t), true
	case uint8:
		return float64(t), true
	case uint16:
		return float64(t), true
	case uint32:
		return float64(t), true
	case uint64:
		return float64(t), true
	}
	return 0, false
}

// strictEquals 复刻 JS 的 ===。
//
// 数值一侧统一升到 float64 再比，这样从数据库读出来的 float64(5) 与
// Go 代码里手写的 int(5) 不会被判成不等——encoding/json 只产 float64，
// 但 Submission.Params 也可能由测试或内部调用直接构造。
func strictEquals(a, b any) bool {
	if an, ok := rawNumber(a); ok {
		bn, ok := rawNumber(b)
		return ok && an == bn
	}
	if _, ok := rawNumber(b); ok {
		return false
	}
	switch at := a.(type) {
	case nil:
		return b == nil
	case string:
		bt, ok := b.(string)
		return ok && at == bt
	case bool:
		bt, ok := b.(bool)
		return ok && at == bt
	}
	// 数组/对象：JS 的 === 是引用比较，两个字面量恒不等。
	return false
}

// stringify 复刻 JS 的 String()，用于 ValueMap 的键查找与 CastString。
//
// 数字必须走 'f',-1 而不是 %v：%v 会把 float64(5) 印成 "5"、把 1e21 印成
// "1e+21"，前者恰好对但后者与 JS 一致纯属巧合。显式指定格式才是可依赖的。
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	}
	if f, ok := rawNumber(v); ok {
		return formatNumber(f)
	}
	return ""
}

func formatNumber(f float64) string {
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if math.IsNaN(f) {
		return "NaN"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// isPrimitive 判断取值是否为 JsonPrimitive。ValidateSchema 用它挡住
// 「给 select 的 option value 填了个对象」这类配置错误。
func isPrimitive(v any) bool {
	switch v.(type) {
	case nil, string, bool:
		return true
	}
	_, ok := rawNumber(v)
	return ok
}
