package mysql

import (
	"database/sql"
	"encoding/json"
	"time"
)

// jsonValue 把一个 Go 值编成可直接绑给 JSON 列的参数。
//
// nil 与空 map / 空 slice 都落成 SQL NULL 而不是 'null' 字面量：
// JSON 列里存着一个 JSON null 的行，用 IS NULL 查不出来、用 JSON_EXTRACT
// 取出来又是个 null，两种判空写法结果不一致，迟早咬人。
func jsonValue(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			return nil, nil
		}
	case []string:
		if len(t) == 0 {
			return nil, nil
		}
	case json.RawMessage:
		if len(t) == 0 {
			return nil, nil
		}
		return []byte(t), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if string(b) == "null" {
		return nil, nil
	}
	return b, nil
}

// jsonRequired 与 jsonValue 同理，但用于 NOT NULL 的 JSON 列
// （models.capability、assets.source、task_events.payload、canvas_ops.ops）。
// 那些列宁可存一个 JSON null 也不能给 SQL NULL——插不进去。
func jsonRequired(v any) ([]byte, error) {
	if v == nil {
		return []byte("null"), nil
	}
	return json.Marshal(v)
}

// scanJSON 把一个可空 JSON 列解进 dst。列为 NULL 时 dst 保持零值。
func scanJSON(raw []byte, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// scanJSONAny 把一个 JSON 列解成 any。
//
// domain.ModelConfig.Capability / RequestMapping 在 domain 层刻意是 any
// （强类型在 capability / mapping 包，domain 依赖它们会成环）。这里解成
// encoding/json 的默认形态（对象 → map[string]any），上层再 Marshal 一次
// 重新 Unmarshal 成具体类型时逐字段等价。返回原始 []byte 反而会让上层
// 拿到一个 json.Marshal 后变成 base64 字符串的东西。
func scanJSONAny(raw []byte) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// ── 可空标量的双向转换 ───────────────────────────────────────────────
//
// domain 用指针区分「没有值」和「值为零」（progress=0 与 progress=null
// 对前端是两件事），库里对应 NULL；下面这组函数就是这条边界的翻译层。

func nullString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullStringNonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullTime(p *time.Time) any {
	if p == nil {
		return nil
	}
	return p.UTC()
}

func strPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

func intPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func floatPtr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}

func timePtr(n sql.NullTime) *time.Time {
	if !n.Valid {
		return nil
	}
	v := n.Time.UTC()
	return &v
}
