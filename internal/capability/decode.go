package capability

import (
	"encoding/json"
	"fmt"
)

// DecodeSchema 把 domain.ModelConfig.Capability 那个 any 解成强类型 schema。
//
// domain 层刻意把 Capability 存成 any（让 domain 反过来依赖本包会成环，
// 见 domain/model.go 的注释），代价就是解码这一步必须有人做，就是这里。
//
// 三种来源形态都要接住，因为同一个字段在不同路径上长得不一样：
//   - map[string]any    从 store 走 encoding/json 反序列化后的常见形态
//   - json.RawMessage / []byte / string   直接从 MySQL JSON 列读出的原始字节
//   - ModelCapabilitySchema（或其指针）   进程内已经解好的，直接放行
//
// 不启用 DisallowUnknownFields：契约里明确允许 JSON 携带 $comment
// （docs/contracts/model-schema.examples.json 就在 pricing 里放了一条），
// 且前端契约可能先于 Go 侧加字段，遇到不认识的键就拒绝落库会把发版顺序焊死。
func DecodeSchema(raw any) (ModelCapabilitySchema, error) {
	var out ModelCapabilitySchema
	switch t := raw.(type) {
	case ModelCapabilitySchema:
		return t, nil
	case *ModelCapabilitySchema:
		if t == nil {
			return out, fmt.Errorf("capability: capability 为空")
		}
		return *t, nil
	}
	if err := decodeJSON(raw, &out, "capability"); err != nil {
		return ModelCapabilitySchema{}, err
	}
	return out, nil
}

// decodeJSON 是三种来源形态到目标结构体的统一入口。
// what 只用于拼错误信息，让「哪一列的 JSON 坏了」在日志里一眼可见。
func decodeJSON(raw any, dst any, what string) error {
	var data []byte
	switch t := raw.(type) {
	case nil:
		return fmt.Errorf("capability: %s 为空", what)
	case json.RawMessage:
		data = t
	case []byte:
		data = t
	case string:
		data = []byte(t)
	default:
		// map[string]any 等已解过一层的形态：重新编码再解，
		// 比手写字段搬运可靠——字段一多手搬必然漏。
		b, err := json.Marshal(raw)
		if err != nil {
			return fmt.Errorf("capability: %s 无法重新编码（%T）: %w", what, raw, err)
		}
		data = b
	}
	if len(data) == 0 {
		return fmt.Errorf("capability: %s 为空", what)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("capability: %s JSON 解码失败: %w", what, err)
	}
	return nil
}
