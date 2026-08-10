package domain

import (
	"encoding/json"
	"errors"
	"strconv"
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

// TestShotParamsJSONContract 钉住台词是**独立字段**且恒定出现。
//
// 这两件事都不是风格问题：台词一旦被并进 description，出片时就没法把它单独
// 拼进 prompt（Seedance 靠它生成同步人声），字幕也没了来源。而字段少了一个
// 又会让前端的镜头卡读到 undefined。
func TestShotParamsJSONContract(t *testing.T) {
	raw := marshalToMap(t, ShotParams{ShotNo: 1, Description: "雨夜的巷口"})

	for _, key := range []string{
		"shot_no", "description", "dialogue", "duration_sec", "camera", "shot_size",
		"first_frame_asset_id",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("shot params 少了字段 %q：前端会读到 undefined", key)
		}
	}
	if raw["dialogue"] != "" {
		t.Errorf("dialogue = %#v, want an empty string for a wordless shot", raw["dialogue"])
	}
}

func TestParseShotParams(t *testing.T) {
	ok, err := ParseShotParams(map[string]any{
		"shot_no": 2, "description": "推近", "dialogue": "你终于来了",
		"duration_sec": 4, "camera": "手持跟拍", "shot_size": "特写",
		"first_frame_asset_id": "as_1",
	})
	if err != nil {
		t.Fatalf("a well-formed shot was rejected: %v", err)
	}
	if ok.ShotNo != 2 || ok.Dialogue != "你终于来了" || ok.DurationSec != 4 {
		t.Errorf("parsed shot params = %+v", ok)
	}
	// 首帧图挂在 params 上而不是 Card.AssetID：出片那一步要往 AssetID 写视频。
	if ok.FirstFrameAssetID != "as_1" {
		t.Errorf("first_frame_asset_id = %q, want %q", ok.FirstFrameAssetID, "as_1")
	}

	// 空 params 放行：镜头卡可以先建出来再填。
	if _, err := ParseShotParams(nil); err != nil {
		t.Errorf("empty params were rejected: %v", err)
	}

	// 台词写进近义 key 必须当场报错。宽松接收的话它会安静存下来，
	// 出片时读 dialogue 读到空串，成片没有人声且无从追查。
	for _, bad := range []map[string]any{
		{"shot_no": 1, "dialog": "你终于来了"},
		{"shot_no": 1, "lines": "你终于来了"},
		{"shot_no": 0},
		{"shot_no": 1, "duration_sec": -1},
	} {
		_, err := ParseShotParams(bad)
		var de *Error
		if !errors.As(err, &de) || de.Code != CodeInvalidParam {
			t.Errorf("ParseShotParams(%v) = %v, want invalid_param", bad, err)
			continue
		}
		if len(de.FieldErrors) == 0 {
			t.Errorf("ParseShotParams(%v) reported no field errors", bad)
		}
	}
}

// TestParseScriptParams 钉住剧本卡的版本列表恒定出现且不接受近义 key。
//
// versions 缺席会让前端的版本切换器读到 undefined；把版本写进 history、
// revisions 这类近义 key 又会被安静存下来，用户看到的是"改了半天一版
// 没留下"，而这正是剧本卡要解决的问题本身。
func TestParseScriptParams(t *testing.T) {
	empty, err := ParseScriptParams(nil)
	if err != nil {
		t.Fatalf("empty params were rejected: %v", err)
	}
	if empty.Versions == nil {
		t.Error("versions is nil; the frontend reads .length on it")
	}

	sp, err := ParseScriptParams(map[string]any{
		"versions": []any{map[string]any{
			"rev": 1, "title": "雨夜重逢", "text": "第一稿", "at": "2026-08-10T00:00:00Z",
		}},
	})
	if err != nil {
		t.Fatalf("a well-formed script params was rejected: %v", err)
	}
	if len(sp.Versions) != 1 || sp.Versions[0].Text != "第一稿" {
		t.Errorf("parsed script params = %+v", sp)
	}

	for _, bad := range []map[string]any{
		{"history": []any{}},
		{"revisions": []any{}},
		{"versions": []any{}, "text": "正文不该放在 params 里"},
	} {
		_, err := ParseScriptParams(bad)
		var de *Error
		if !errors.As(err, &de) || de.Code != CodeInvalidParam {
			t.Errorf("ParseScriptParams(%v) = %v, want invalid_param", bad, err)
			continue
		}
		if len(de.FieldErrors) == 0 {
			t.Errorf("ParseScriptParams(%v) reported no field errors", bad)
		}
	}
}

// TestScriptParamsAppendVersion 钉住版号是全局递增的，不是数组下标。
//
// 截断丢掉最旧几版之后 len 会倒退，用它编号会让新版本拿到一个已经用过的
// 号，前端的版本列表就会出现两个 v37——用户点「回退到 v37」时无从知道
// 点中的是哪一版。
func TestScriptParamsAppendVersion(t *testing.T) {
	at := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	var sp ScriptParams
	for i := 0; i < ScriptMaxVersions+5; i++ {
		sp = sp.AppendVersion("稿", "第 "+strconv.Itoa(i)+" 稿", at)
	}

	if len(sp.Versions) != ScriptMaxVersions {
		t.Fatalf("kept %d versions, want the cap %d", len(sp.Versions), ScriptMaxVersions)
	}
	last := sp.Versions[len(sp.Versions)-1]
	if last.Rev != ScriptMaxVersions+5 {
		t.Errorf("last rev = %d, want %d: 版号必须继续递增，不能跟着截断回退",
			last.Rev, ScriptMaxVersions+5)
	}
	// 留下的是最近的那些版：被丢掉的应该是最旧的开头几版。
	if sp.Versions[0].Rev != 6 {
		t.Errorf("first kept rev = %d, want 6", sp.Versions[0].Rev)
	}
}
