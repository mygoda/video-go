package capability

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// examplesPath 指向前后端共用的契约样例。用它做测试输入而不是另写一份 fixture，
// 是为了让契约一改测试就红——两端不一致的事故必须在这里被拦住。
const examplesPath = "../../docs/contracts/model-schema.examples.json"

func loadExamples(t *testing.T) map[string]ModelCapabilitySchema {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash(examplesPath))
	if err != nil {
		t.Fatalf("读取契约样例失败: %v", err)
	}
	var file struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("契约样例不是合法 JSON: %v", err)
	}
	out := make(map[string]ModelCapabilitySchema, len(file.Models))
	for _, raw := range file.Models {
		// 走 map[string]any 这条路，顺带覆盖 DecodeSchema 的"已解过一层"分支。
		var generic map[string]any
		if err := json.Unmarshal(raw, &generic); err != nil {
			t.Fatalf("解析样例失败: %v", err)
		}
		schema, err := DecodeSchema(generic)
		if err != nil {
			t.Fatalf("DecodeSchema 失败: %v", err)
		}
		out[schema.ID] = schema
	}
	return out
}

func TestDecodeSchemaShapes(t *testing.T) {
	raw := `{"id":"m1","name":"M1","vendor":"v","modality":"image","enabled":true,"order":1,
	"inputs":[{"key":"ref","label":"参考图","kind":"image","required":false,"min_count":0,"max_count":2,"accept":["image/png"],"max_bytes":100}],
	"params":[{"key":"count","label":"数量","control":"stepper","group":"primary","order":10,"default":1,"min":1,"max":4,"step":1}],
	"pricing":{"currency":"credit","base":8,"modifiers":[],"multiplier_param":"count","rounding":"ceil"},
	"eta":{"p50_seconds":10,"p90_seconds":20},
	"limits":{"max_concurrent_per_user":4,"prompt_max_length":100,"queue_position_available":true}}`

	var asMap map[string]any
	if err := json.Unmarshal([]byte(raw), &asMap); err != nil {
		t.Fatal(err)
	}
	typed, err := DecodeSchema(asMap)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		in   any
	}{
		{"map[string]any", asMap},
		{"json.RawMessage", json.RawMessage(raw)},
		{"[]byte", []byte(raw)},
		{"string", raw},
		{"已是强类型", typed},
		{"强类型指针", &typed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeSchema(tc.in)
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if got.ID != "m1" || len(got.Params) != 1 || got.Params[0].Min == nil || *got.Params[0].Min != 1 {
				t.Fatalf("解码结果不对: %+v", got)
			}
			if got.Modality != "image" || got.Pricing.MultiplierParam != "count" {
				t.Fatalf("解码结果不对: %+v", got)
			}
		})
	}
}

func TestDecodeSchemaErrors(t *testing.T) {
	cases := []struct {
		name string
		in   any
	}{
		{"nil", nil},
		{"空字节", []byte{}},
		{"空串", ""},
		{"坏 JSON", `{"id":`},
		{"类型不匹配", `{"id":123}`},
		{"空指针", (*ModelCapabilitySchema)(nil)},
		{"不可编码", map[string]any{"id": make(chan int)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeSchema(tc.in); err == nil {
				t.Fatal("期望报错，实际通过")
			}
		})
	}
}

func TestContractExamplesAreSelfConsistent(t *testing.T) {
	v := NewValidator(nil)
	for id, schema := range loadExamples(t) {
		if errs := v.ValidateSchema(schema); len(errs) > 0 {
			t.Fatalf("契约样例 %s 未通过 ValidateSchema: %+v", id, errs)
		}
	}
}
