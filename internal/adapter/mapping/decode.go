package mapping

import (
	"encoding/json"
	"fmt"
)

// DecodeMapping 把 domain.ModelConfig.RequestMapping 那个 any 解成强类型配置。
//
// 与 capability.DecodeSchema 同因同形：domain 不能依赖本包（会成环），
// 所以那一列在 domain 里是 any，解码这一步落在这里。
//
// RequestMapping 允许为空——mock driver 与不需要翻译的模型就没有这一列，
// 因此 nil 返回零值而不是报错，与 capability 那边「空即错」相反。
func DecodeMapping(raw any) (RequestMapping, error) {
	var out RequestMapping
	switch t := raw.(type) {
	case nil:
		return out, nil
	case RequestMapping:
		return t, nil
	case *RequestMapping:
		if t == nil {
			return out, nil
		}
		return *t, nil
	}

	var data []byte
	switch t := raw.(type) {
	case json.RawMessage:
		data = t
	case []byte:
		data = t
	case string:
		data = []byte(t)
	default:
		b, err := json.Marshal(raw)
		if err != nil {
			return RequestMapping{}, fmt.Errorf("mapping: request_mapping 无法重新编码（%T）: %w", raw, err)
		}
		data = b
	}
	if len(data) == 0 || string(data) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return RequestMapping{}, fmt.Errorf("mapping: request_mapping JSON 解码失败: %w", err)
	}
	return out, nil
}
