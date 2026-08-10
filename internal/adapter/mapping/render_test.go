package mapping

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/capability"
)

func boolPtr(b bool) *bool { return &b }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func baseCtx() RenderContext {
	return RenderContext{
		Prompt:        "一只在雨里的猫",
		UpstreamModel: "doubao-seedance-2-0",
		Params: map[string]any{
			"resolution": "768p",
			"duration":   float64(5),
			"count":      float64(2),
			"strength":   0.6,
			"watermark":  false,
			"seed":       nil,
			"empty":      "",
		},
		InputURLs: map[string][]string{
			"reference_images": {"https://cdn/1.png", "https://cdn/2.png"},
			"first_frame":      {},
		},
	}
}

func TestRenderRules(t *testing.T) {
	cases := []struct {
		name string
		m    RequestMapping
		want map[string]any
	}{
		{
			name: "常量与模板骨架",
			m: RequestMapping{
				BodyTemplate: map[string]any{"parameters": map[string]any{"quality": "high"}},
				Rules:        []MappingRule{{Const: false, To: "parameters.watermark"}},
			},
			want: map[string]any{"parameters": map[string]any{"quality": "high", "watermark": false}},
		},
		{
			name: "prompt 与 upstream_model",
			m: RequestMapping{Rules: []MappingRule{
				{From: "model.upstream_model", To: "model"},
				{From: "prompt", To: "input.text"},
			}},
			want: map[string]any{
				"model": "doubao-seedance-2-0",
				"input": map[string]any{"text": "一只在雨里的猫"},
			},
		},
		{
			name: "嵌套路径按需创建中间对象",
			m:    RequestMapping{Rules: []MappingRule{{From: "params.resolution", To: "a.b.c"}}},
			want: map[string]any{"a": map[string]any{"b": map[string]any{"c": "768p"}}},
		},
		{
			name: "value_map 做取值翻译",
			m: RequestMapping{Rules: []MappingRule{
				{From: "params.resolution", To: "parameters.size", ValueMap: map[string]any{"768p": "720p", "1080p": "1920x1080"}},
			}},
			want: map[string]any{"parameters": map[string]any{"size": "720p"}},
		},
		{
			name: "value_map 用 JSON 字符串形态命中数值键",
			m: RequestMapping{Rules: []MappingRule{
				{From: "params.duration", To: "parameters.length", ValueMap: map[string]any{"5": "short", "10": "long"}},
			}},
			want: map[string]any{"parameters": map[string]any{"length": "short"}},
		},
		{
			name: "value_map 未命中时原样透传",
			m: RequestMapping{Rules: []MappingRule{
				{From: "params.resolution", To: "parameters.size", ValueMap: map[string]any{"1080p": "1920x1080"}},
			}},
			want: map[string]any{"parameters": map[string]any{"size": "768p"}},
		},
		{
			name: "cast string",
			m:    RequestMapping{Rules: []MappingRule{{From: "params.duration", To: "d", Cast: CastString}}},
			want: map[string]any{"d": "5"},
		},
		{
			name: "cast int 截断小数",
			m:    RequestMapping{Rules: []MappingRule{{From: "params.strength", To: "s", Cast: CastInt}}},
			want: map[string]any{"s": int64(0)},
		},
		{
			name: "cast float",
			m:    RequestMapping{Rules: []MappingRule{{From: "params.duration", To: "d", Cast: CastFloat}}},
			want: map[string]any{"d": float64(5)},
		},
		{
			name: "cast bool 从数值",
			m:    RequestMapping{Rules: []MappingRule{{From: "params.count", To: "b", Cast: CastBool}}},
			want: map[string]any{"b": true},
		},
		{
			name: "cast seconds_suffix",
			m:    RequestMapping{Rules: []MappingRule{{From: "params.duration", To: "parameters.duration", Cast: CastSecondsSuffix}}},
			want: map[string]any{"parameters": map[string]any{"duration": "5s"}},
		},
		{
			name: "wrap text_part 追加进数组",
			m: RequestMapping{Rules: []MappingRule{
				{From: "prompt", To: "content[]", Wrap: WrapTextPart},
			}},
			want: map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "一只在雨里的猫"},
			}},
		},
		{
			name: "wrap image_url_part 多值按槽内顺序追加",
			m: RequestMapping{Rules: []MappingRule{
				{From: "inputs.reference_images", To: "content[]", Wrap: WrapImageURLPart},
			}},
			want: map[string]any{"content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://cdn/1.png"}},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://cdn/2.png"}},
			}},
		},
		{
			name: "wrap video_url_part",
			m: RequestMapping{Rules: []MappingRule{
				{Const: "https://cdn/a.mp4", To: "content[]", Wrap: WrapVideoURLPart},
			}},
			want: map[string]any{"content": []any{
				map[string]any{"type": "video_url", "video_url": map[string]any{"url": "https://cdn/a.mp4"}},
			}},
		},
		{
			name: "wrap raw",
			m: RequestMapping{Rules: []MappingRule{
				{From: "params.resolution", To: "list[]", Wrap: WrapRaw},
			}},
			want: map[string]any{"list": []any{"768p"}},
		},
		{
			name: "wrap user_message 拼出 chat 协议的 messages 数组",
			m: RequestMapping{Rules: []MappingRule{
				{From: "prompt", To: "messages[]", Wrap: WrapUserMessage},
			}},
			want: map[string]any{"messages": []any{
				map[string]any{"role": "user", "content": "一只在雨里的猫"},
			}},
		},
		{
			name: "规则顺序即数组顺序：图在前文在后",
			m: RequestMapping{Rules: []MappingRule{
				{From: "inputs.reference_images", To: "content[]", Wrap: WrapImageURLPart},
				{From: "prompt", To: "content[]", Wrap: WrapTextPart},
			}},
			want: map[string]any{"content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://cdn/1.png"}},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://cdn/2.png"}},
				map[string]any{"type": "text", "text": "一只在雨里的猫"},
			}},
		},
		{
			name: "追加进模板里已有的数组",
			m: RequestMapping{
				BodyTemplate: map[string]any{"content": []any{map[string]any{"type": "text", "text": "前缀"}}},
				Rules:        []MappingRule{{From: "prompt", To: "content[]", Wrap: WrapTextPart}},
			},
			want: map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "前缀"},
				map[string]any{"type": "text", "text": "一只在雨里的猫"},
			}},
		},
		{
			name: "when 为真时写入",
			m: RequestMapping{Rules: []MappingRule{{
				From: "params.strength", To: "parameters.strength",
				When: &capability.Condition{Op: capability.OpHasInput, Slot: "reference_images"},
			}}},
			want: map[string]any{"parameters": map[string]any{"strength": 0.6}},
		},
		{
			name: "when 为假时整条跳过（隐式分流）",
			m: RequestMapping{Rules: []MappingRule{{
				From: "params.strength", To: "parameters.strength",
				When: &capability.Condition{Op: capability.OpHasInput, Slot: "first_frame"},
			}}},
			want: map[string]any{},
		},
		{
			name: "缺失参数默认省略",
			m:    RequestMapping{Rules: []MappingRule{{From: "params.nonexistent", To: "parameters.x"}}},
			want: map[string]any{},
		},
		{
			name: "空字符串默认省略",
			m:    RequestMapping{Rules: []MappingRule{{From: "params.empty", To: "parameters.x"}}},
			want: map[string]any{},
		},
		{
			name: "null 默认省略",
			m:    RequestMapping{Rules: []MappingRule{{From: "params.seed", To: "parameters.seed"}}},
			want: map[string]any{},
		},
		{
			name: "omit_when_empty=false 时显式写 null",
			m: RequestMapping{Rules: []MappingRule{
				{From: "params.seed", To: "parameters.seed", OmitWhenEmpty: boolPtr(false)},
			}},
			want: map[string]any{"parameters": map[string]any{"seed": nil}},
		},
		{
			name: "false 与 0 不算空值",
			m: RequestMapping{Rules: []MappingRule{
				{From: "params.watermark", To: "parameters.watermark"},
				{Const: float64(0), To: "parameters.offset"},
			}},
			want: map[string]any{"parameters": map[string]any{"watermark": false, "offset": float64(0)}},
		},
		{
			name: "空输入槽走省略路径",
			m: RequestMapping{Rules: []MappingRule{
				{From: "inputs.first_frame", To: "content[]", Wrap: WrapImageURLPart},
			}},
			want: map[string]any{},
		},
		{
			name: "value_map 与 cast 与 wrap 串联",
			m: RequestMapping{Rules: []MappingRule{{
				From: "params.duration", To: "content[]",
				ValueMap: map[string]any{"5": float64(6)}, Cast: CastSecondsSuffix, Wrap: WrapTextPart,
			}}},
			want: map[string]any{"content": []any{map[string]any{"type": "text", "text": "6s"}}},
		},
	}

	r := NewRenderer(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.Render(tc.m, baseCtx())
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %s\nwant %s", mustJSON(t, got), mustJSON(t, tc.want))
			}
		})
	}
}

func TestRenderErrors(t *testing.T) {
	cases := []struct {
		name string
		m    RequestMapping
	}{
		{"既无 from 也无 const", RequestMapping{Rules: []MappingRule{{To: "x"}}}},
		{"from 与 const 同时给", RequestMapping{Rules: []MappingRule{{From: "prompt", Const: "x", To: "x"}}}},
		{"缺 to", RequestMapping{Rules: []MappingRule{{From: "prompt"}}}},
		{"未知取值源", RequestMapping{Rules: []MappingRule{{From: "user.name", To: "x"}}}},
		{"params. 后为空", RequestMapping{Rules: []MappingRule{{From: "params.", To: "x"}}}},
		{"inputs. 后为空", RequestMapping{Rules: []MappingRule{{From: "inputs.", To: "x"}}}},
		{"to 路径中有空段", RequestMapping{Rules: []MappingRule{{From: "prompt", To: "a..b"}}}},
		{"[] 不在末尾", RequestMapping{Rules: []MappingRule{{From: "prompt", To: "a[].b"}}}},
		{"未知 cast", RequestMapping{Rules: []MappingRule{{From: "prompt", To: "x", Cast: "date"}}}},
		{"未知 wrap", RequestMapping{Rules: []MappingRule{{From: "prompt", To: "x[]", Wrap: "audio_part"}}}},
		{"cast int 遇非数值", RequestMapping{Rules: []MappingRule{{From: "prompt", To: "x", Cast: CastInt}}}},
		{"cast bool 遇非布尔字符串", RequestMapping{Rules: []MappingRule{{From: "prompt", To: "x", Cast: CastBool}}}},
		{"多值写进标量目标", RequestMapping{Rules: []MappingRule{{From: "inputs.reference_images", To: "image"}}}},
		{
			"追加到已有的非数组值",
			RequestMapping{
				BodyTemplate: map[string]any{"content": "文本"},
				Rules:        []MappingRule{{From: "prompt", To: "content[]"}},
			},
		},
		{
			"下钻到非对象值",
			RequestMapping{
				BodyTemplate: map[string]any{"parameters": "x"},
				Rules:        []MappingRule{{From: "prompt", To: "parameters.text"}},
			},
		},
		{
			"when 条件本身非法",
			RequestMapping{Rules: []MappingRule{{
				From: "prompt", To: "x",
				When: &capability.Condition{Op: "between", Key: "duration", Value: float64(1)},
			}}},
		},
	}

	r := NewRenderer(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.Render(tc.m, baseCtx()); err == nil {
				t.Fatal("期望报错，实际通过")
			}
		})
	}
}

func TestRenderDoesNotMutateTemplate(t *testing.T) {
	// 同一份 ModelConfig 被并发的多个任务共用，就地改一次就是全局污染。
	m := RequestMapping{
		BodyTemplate: map[string]any{
			"parameters": map[string]any{"quality": "high"},
			"content":    []any{map[string]any{"type": "text", "text": "前缀"}},
		},
		Rules: []MappingRule{
			{From: "prompt", To: "content[]", Wrap: WrapTextPart},
			{From: "params.resolution", To: "parameters.size"},
		},
	}
	before := mustJSON(t, m.BodyTemplate)

	r := NewRenderer(nil)
	first, err := r.Render(m, baseCtx())
	if err != nil {
		t.Fatal(err)
	}
	if after := mustJSON(t, m.BodyTemplate); after != before {
		t.Fatalf("模板被就地修改了:\nbefore %s\nafter  %s", before, after)
	}

	// 第二次渲染必须与第一次完全相同，否则说明状态在实例间泄漏。
	second, err := r.Render(m, baseCtx())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("两次渲染结果不同:\n%s\n%s", mustJSON(t, first), mustJSON(t, second))
	}
}

func TestRenderArkShapedBody(t *testing.T) {
	// 端到端形态：Ark 的 model / content / parameters 三段结构，
	// 首帧图在提示词之前（顺序影响生成语义）。
	raw := `{
	  "body_template": {"parameters": {"watermark": false}},
	  "rules": [
	    {"from": "model.upstream_model", "to": "model"},
	    {"from": "inputs.first_frame", "to": "content[]", "wrap": "image_url_part",
	     "when": {"op": "has_input", "slot": "first_frame"}},
	    {"from": "prompt", "to": "content[]", "wrap": "text_part"},
	    {"from": "params.resolution", "to": "parameters.resolution", "value_map": {"768p": "720p"}},
	    {"from": "params.duration", "to": "parameters.duration", "cast": "seconds_suffix"}
	  ]
	}`
	m, err := DecodeMapping(raw)
	if err != nil {
		t.Fatal(err)
	}

	ctx := baseCtx()
	ctx.InputURLs["first_frame"] = []string{"https://cdn/frame.png"}

	got, err := NewRenderer(nil).Render(m, ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"model": "doubao-seedance-2-0",
		"content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://cdn/frame.png"}},
			map[string]any{"type": "text", "text": "一只在雨里的猫"},
		},
		"parameters": map[string]any{"watermark": false, "resolution": "720p", "duration": "5s"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %s\nwant %s", mustJSON(t, got), mustJSON(t, want))
	}

	// 不传首帧时那条规则整条消失 —— 隐式分流没有 if，只有条件规则。
	ctx.InputURLs["first_frame"] = nil
	got, err = NewRenderer(nil).Render(m, ctx)
	if err != nil {
		t.Fatal(err)
	}
	content := got["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("未传首帧时 content 应只有文本部分，got %s", mustJSON(t, content))
	}
}

func TestRenderChatShapedBody(t *testing.T) {
	// 端到端形态：OpenAI 兼容的 chat 请求体。这份 mapping 与
	// migrations/000003 播种的 gpugeek 模型逐字一致——它是「接一个走已知协议的
	// 新模型 = 一条配置，零代码」在真实上游上的那条配置。
	raw := `{
	  "rules": [
	    {"from": "model.upstream_model", "to": "model"},
	    {"from": "prompt", "to": "messages[]", "wrap": "user_message"},
	    {"from": "params.temperature", "to": "temperature", "cast": "float"},
	    {"from": "params.max_tokens", "to": "max_tokens", "cast": "int"}
	  ]
	}`
	m, err := DecodeMapping(raw)
	if err != nil {
		t.Fatal(err)
	}

	ctx := baseCtx()
	ctx.UpstreamModel = "Vendor3/qwen-flash"
	ctx.Params["temperature"] = 0.7
	ctx.Params["max_tokens"] = float64(2048)

	got, err := NewRenderer(nil).Render(m, ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"model":       "Vendor3/qwen-flash",
		"messages":    []any{map[string]any{"role": "user", "content": "一只在雨里的猫"}},
		"temperature": 0.7,
		"max_tokens":  int64(2048),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %s\nwant %s", mustJSON(t, got), mustJSON(t, want))
	}
}

func TestDecodeMapping(t *testing.T) {
	raw := `{"body_template":{"a":1},"rules":[{"from":"prompt","to":"x"}]}`
	var asMap map[string]any
	if err := json.Unmarshal([]byte(raw), &asMap); err != nil {
		t.Fatal(err)
	}
	typed, err := DecodeMapping(asMap)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		in    any
		rules int
	}{
		{"map[string]any", asMap, 1},
		{"json.RawMessage", json.RawMessage(raw), 1},
		{"[]byte", []byte(raw), 1},
		{"string", raw, 1},
		{"已是强类型", typed, 1},
		{"强类型指针", &typed, 1},
		{"nil 视为空配置", nil, 0},
		{"字面 null 视为空配置", "null", 0},
		{"空串视为空配置", "", 0},
		{"空指针视为空配置", (*RequestMapping)(nil), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeMapping(tc.in)
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if len(got.Rules) != tc.rules {
				t.Fatalf("规则数 %d, want %d", len(got.Rules), tc.rules)
			}
		})
	}

	for _, bad := range []any{`{"rules":`, map[string]any{"rules": make(chan int)}, `{"rules":5}`} {
		if _, err := DecodeMapping(bad); err == nil {
			t.Fatalf("期望报错，实际通过: %v", bad)
		}
	}
}

func TestRenderVisionChatShapedBody(t *testing.T) {
	// 端到端形态：OpenAI 兼容的**多模态输入** chat 请求体。这份 mapping 与
	// migrations/000004 播种的 gpugeek 视觉模型逐字一致。
	//
	// 它同时钉住两件事：messages.0.content[] 这种下标寻址能走通，
	// 以及 inline 取的是 data URL 而不是地址——上游拉不到本平台的地址，
	// 一旦这里悄悄退回 URL，失败会推迟到几十秒后的一次下载超时。
	raw := `{
	  "body_template": { "messages": [ { "role": "user", "content": [] } ] },
	  "rules": [
	    {"from": "model.upstream_model", "to": "model"},
	    {"from": "inputs.image", "to": "messages.0.content[]", "wrap": "image_url_part", "inline": true},
	    {"from": "prompt", "to": "messages.0.content[]", "wrap": "text_part"},
	    {"from": "params.temperature", "to": "temperature", "cast": "float"}
	  ]
	}`
	m, err := DecodeMapping(raw)
	if err != nil {
		t.Fatal(err)
	}

	ctx := baseCtx()
	ctx.UpstreamModel = "Vendor3/qwen3-vl-plus"
	ctx.Params["temperature"] = 0.7
	ctx.InputURLs["image"] = []string{"http://127.0.0.1:8081/api/assets/a1/content?variant=original"}
	ctx.InputDataURLs = map[string][]string{"image": {"data:image/png;base64,QUJD"}}

	got, err := NewRenderer(nil).Render(m, ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"model": "Vendor3/qwen3-vl-plus",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,QUJD"}},
				map[string]any{"type": "text", "text": "一只在雨里的猫"},
			},
		}},
		"temperature": 0.7,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %s\nwant %s", mustJSON(t, got), mustJSON(t, want))
	}
}

// 图槽为空时整条 inputs 规则被跳过，content 里只剩文本——
// 这就是 InputSlotSpec 说的隐式分流：传图走看图说话，不传退化成普通对话。
func TestRenderVisionWithoutImage(t *testing.T) {
	m, err := DecodeMapping(`{
	  "body_template": { "messages": [ { "role": "user", "content": [] } ] },
	  "rules": [
	    {"from": "inputs.image", "to": "messages.0.content[]", "wrap": "image_url_part", "inline": true},
	    {"from": "prompt", "to": "messages.0.content[]", "wrap": "text_part"}
	  ]
	}`)
	if err != nil {
		t.Fatal(err)
	}

	got, err := NewRenderer(nil).Render(m, baseCtx())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": "一只在雨里的猫"}},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %s\nwant %s", mustJSON(t, got), mustJSON(t, want))
	}
}

func TestRenderIndexedPathErrors(t *testing.T) {
	withMessages := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{}}}}

	cases := []struct {
		name string
		m    RequestMapping
	}{
		{
			// 下标越界不自动补齐：补齐会让 "messages.1" 这种笔误静默造出
			// 一条空消息发给上游，而那是一次真花钱的调用。
			"下标越界",
			RequestMapping{
				BodyTemplate: withMessages,
				Rules:        []MappingRule{{From: "prompt", To: "messages.9.content[]"}},
			},
		},
		{
			"数组上用非下标段",
			RequestMapping{
				BodyTemplate: withMessages,
				Rules:        []MappingRule{{From: "prompt", To: "messages.role.content[]"}},
			},
		},
		{
			// 槽里有素材却没拿到内联形态，说明读字节那步没做或失败了。
			"声明 inline 但缺内联素材",
			RequestMapping{
				Rules: []MappingRule{{From: "inputs.reference_images", To: "content[]", Wrap: WrapImageURLPart, Inline: true}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRenderer(nil).Render(tc.m, baseCtx()); err == nil {
				t.Fatal("期望报错，实际通过")
			}
		})
	}
}

// TestRenderPositionalImageSlots 钉住 Seedance 的首尾帧形态：两个槽追加进同一个
// 裸字符串数组，**顺序即规则顺序**——images[0] 是首帧、images[1] 是尾帧，
// 上游只按位置识别，顺序错了就是首尾颠倒的片子。
//
// 同时钉住「只给首帧」渲染出长度为 1 的数组而不是留个空洞：
// 空槽取到空值，OmitWhenEmpty 默认为真，于是那一条规则整条被跳过。
func TestRenderPositionalImageSlots(t *testing.T) {
	m := RequestMapping{
		BodyTemplate: map[string]any{"input": map[string]any{}},
		Rules: []MappingRule{
			{From: "inputs.first_frame", To: "input.images[]", Inline: true},
			{From: "inputs.last_frame", To: "input.images[]", Inline: true},
		},
	}

	ctx := baseCtx()
	ctx.InputDataURLs = map[string][]string{
		"first_frame": {"data:image/png;base64,Rklsc1Q="},
		"last_frame":  {"data:image/png;base64,TEFTVA=="},
	}

	got, err := NewRenderer(nil).Render(m, ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"input": map[string]any{"images": []any{
		"data:image/png;base64,Rklsc1Q=",
		"data:image/png;base64,TEFTVA==",
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("首尾帧顺序不符\n got=%s\nwant=%s", mustJSON(t, got), mustJSON(t, want))
	}

	ctx.InputDataURLs = map[string][]string{"first_frame": {"data:image/png;base64,Rklsc1Q="}}
	got, err = NewRenderer(nil).Render(m, ctx)
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]any{"input": map[string]any{"images": []any{"data:image/png;base64,Rklsc1Q="}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("只给首帧时不该留空洞\n got=%s\nwant=%s", mustJSON(t, got), mustJSON(t, want))
	}
}
