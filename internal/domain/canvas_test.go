package domain

import (
	"encoding/json"
	"testing"
	"time"
)

// TestCardJSONContract 钉住 Card 下发给前端的那层 JSON 形状。
//
// 它存在的理由是一次真实事故：title 在前端契约里存在、前端也一直在发，
// 但后端结构体没有这个字段，于是它在 JSON 解码那一步被静默丢弃——
// 没有报错，没有日志，只有用户刷新后标题消失。字段的有无是接口的一部分，
// 而 Go 的结构体标签改一个字就能让它悄悄消失，所以这里用序列化后的
// 原始 map 来断言，而不是断言 Go 侧的字段。
func TestCardJSONContract(t *testing.T) {
	at := time.Date(2026, 8, 8, 14, 30, 5, 0, time.UTC)
	raw := marshalToMap(t, Card{
		ID: "card_1", Kind: CardKindText, Title: "分镜 1",
		Refs:      []string{},
		History:   []CardVersion{},
		CreatedAt: at,
	})

	// 这三个键前端直接读（title 取值渲染，refs / history 直接读 .length），
	// 缺一个就是一次白屏，所以它们都不带 omitempty。
	for _, key := range []string{"title", "refs", "history"} {
		v, ok := raw[key]
		if !ok {
			t.Errorf("key %q missing from the card JSON; the frontend reads it unconditionally", key)
			continue
		}
		if key != "title" && v == nil {
			t.Errorf("key %q serialized as null; it must always be an array", key)
		}
	}
	if raw["title"] != "分镜 1" {
		t.Errorf("title = %v, want 分镜 1", raw["title"])
	}

	// 空值也必须是空数组而不是缺失或 null——上面那份卡片的 Refs / History
	// 都是零长切片，这正是新建卡片的常态。
	if arr, ok := raw["refs"].([]any); !ok || len(arr) != 0 {
		t.Errorf("refs = %#v, want an empty array", raw["refs"])
	}
	if arr, ok := raw["history"].([]any); !ok || len(arr) != 0 {
		t.Errorf("history = %#v, want an empty array", raw["history"])
	}
}

// TestCardTimestampsAreISO8601 钉住「时间一律是 RFC3339 字符串」这条全局约定。
//
// 前端曾把 history[].at 当成 unix 毫秒数（number）读，而后端这里从来都是
// time.Time，序列化出来是字符串。这个测试把服务端这一侧固定下来，
// 免得为了迁就某一处调用方而改成数字，进而让同一个响应里出现两种时间表示。
func TestCardTimestampsAreISO8601(t *testing.T) {
	at := time.Date(2026, 8, 8, 14, 30, 5, 0, time.UTC)
	raw := marshalToMap(t, Card{
		ID: "card_1", Kind: CardKindText,
		Refs:      []string{},
		History:   []CardVersion{{AssetID: "asset_1", Prompt: "a cat", At: at}},
		CreatedAt: at,
	})

	created, ok := raw["created_at"].(string)
	if !ok {
		t.Fatalf("created_at = %#v, want an RFC3339 string", raw["created_at"])
	}
	if _, err := time.Parse(time.RFC3339, created); err != nil {
		t.Errorf("created_at %q does not parse as RFC3339: %v", created, err)
	}

	history, ok := raw["history"].([]any)
	if !ok || len(history) != 1 {
		t.Fatalf("history = %#v, want one entry", raw["history"])
	}
	entry, ok := history[0].(map[string]any)
	if !ok {
		t.Fatalf("history[0] = %#v, want an object", history[0])
	}
	ts, ok := entry["at"].(string)
	if !ok {
		t.Fatalf("history[0].at = %#v, want an RFC3339 string, not a unix timestamp", entry["at"])
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("history[0].at %q does not parse as RFC3339: %v", ts, err)
	}
}

func marshalToMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return raw
}
