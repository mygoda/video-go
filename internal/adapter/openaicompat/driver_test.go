package openaicompat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/adapter/mapping"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testProvider() domain.Provider {
	return domain.Provider{
		BaseURL:       "https://api.example/",
		CredentialRef: "AIGC_PROVIDER_OPENAI_KEY",
		AuthStyle:     domain.AuthStyleBearer,
	}
}

func testInput(family domain.ProtocolFamily) adapter.SubmitInput {
	return adapter.SubmitInput{
		TaskID:   "t1",
		Provider: testProvider(),
		Model: domain.ModelConfig{
			ID:     "m1",
			Family: family,
			RequestMapping: json.RawMessage(`{"rules":[
				{"from":"prompt","to":"prompt"},
				{"from":"params.size","to":"size"}
			]}`),
		},
		UpstreamModel: "gpt-image-1",
		Prompt:        "一只戴帽子的柯基",
		Params:        map[string]any{"size": "1024x1024"},
		Credential:    "test-credential-placeholder",
	}
}

// TestNormalize 钉死同步族的两个自造原始状态。
//
// 上游**不给状态字段**——同步请求要么 200 带产物，要么报错。
// 这两个值是我们造出来落进 tasks.upstream_status_raw 的，
// 认不出的值归 failed（而不是像异步族那样归 running）：
// 同步调用已经回来了，"还在跑"这个态在这里不存在。
func TestNormalize(t *testing.T) {
	cases := []struct {
		raw  string
		want adapter.Status
	}{
		{rawOK, adapter.StatusSucceeded},
		{rawError, adapter.StatusFailed},
		{"whatever", adapter.StatusFailed},
		{"", adapter.StatusFailed},
	}
	for _, c := range cases {
		if got := normalize(c.raw); got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestStatusMapCoversRawVocabulary(t *testing.T) {
	for _, raw := range []string{rawOK, rawError} {
		if _, ok := statusMap[raw]; !ok {
			t.Errorf("statusMap 缺少 %q", raw)
		}
	}
	if len(statusMap) != 2 {
		t.Errorf("statusMap 有 %d 条，期望 2 条", len(statusMap))
	}
}

// TestClassify 钉死失败三分类。审核拒绝与参数错误都是 400，
// 只看状态码会把前者报成后者，让前端去高亮一枚根本没错的参数芯片。
func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		err    apiError
		want   domain.TaskErrorCode
	}{
		{"内容策略", 400, apiError{Code: "content_policy_violation"}, domain.TaskErrorContentRejected},
		{"审核拦截", 400, apiError{Code: "moderation_blocked"}, domain.TaskErrorContentRejected},
		{"限流码", 429, apiError{Code: "rate_limit_exceeded"}, domain.TaskErrorUpstreamRateLimited},
		{"配额耗尽", 429, apiError{Code: "insufficient_quota"}, domain.TaskErrorUpstreamRateLimited},
		{"参数错误按 type", 400, apiError{Type: "invalid_request_error"}, domain.TaskErrorInvalidParam},
		{"用户输入错误", 400, apiError{Code: "image_generation_user_error"}, domain.TaskErrorInvalidParam},

		// 兼容实现常常不填 code，只有一句英文/中文文案。
		{"只有英文文案的审核", 400, apiError{Message: "Your request was rejected by the safety system"}, domain.TaskErrorContentRejected},
		{"只有中文文案的审核", 400, apiError{Message: "请求触发内容审核"}, domain.TaskErrorContentRejected},
		{"只有文案的限流", 429, apiError{Message: "Rate limit reached for gpt-image-1"}, domain.TaskErrorUpstreamRateLimited},

		// 认不出来时才落状态码。
		{"未知码落 400", 400, apiError{Code: "brand_new"}, domain.TaskErrorInvalidParam},
		{"未知码落 500", 500, apiError{Code: "brand_new"}, domain.TaskErrorInternal},
		{"无状态码无线索", 0, apiError{}, domain.TaskErrorInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.status, c.err); got != c.want {
				t.Errorf("classify() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestUpstreamErrorDoesNotForgeFieldError 保证上游的 param 名只进文案、
// 不变成 FieldError——前端按 ParamSpec.Key 找芯片，上游字段名不在那张表里。
func TestUpstreamErrorDoesNotForgeFieldError(t *testing.T) {
	e := upstreamError(400, apiError{Message: "bad size", Param: "size", Type: "invalid_request_error"})
	if !strings.Contains(e.Message, "size") {
		t.Errorf("上游字段名应当出现在文案里: %q", e.Message)
	}
	if len(e.FieldErrors) != 0 {
		t.Errorf("不该凭上游字段名造 FieldError: %+v", e.FieldErrors)
	}
}

// TestInvokeImagesBase64 验证 b64_json 形态的产物。
func TestInvokeImagesBase64(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("fake-png"))
	var gotURL string
	var gotBody map[string]any
	d := New(Driver{
		ProtocolFamily: domain.FamilyImages,
		Renderer:       mapping.NewRenderer(nil),
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotURL = r.URL.String()
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			return jsonResponse(200, `{"size":"1024x1024","data":[{"b64_json":"`+payload+`"}]}`), nil
		})},
	})

	res, err := d.Invoke(context.Background(), testInput(domain.FamilyImages))
	if err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}
	if want := "https://api.example" + PathImagesGenerations; gotURL != want {
		t.Errorf("端点 = %q, want %q", gotURL, want)
	}
	// mapping 没写 model 规则时驱动补上，否则上游必然 400。
	if gotBody["model"] != "gpt-image-1" {
		t.Errorf("body.model = %v", gotBody["model"])
	}
	if res.UpstreamRef != "" {
		t.Error("同步族没有后续轮询，UpstreamRef 必须为空，否则 L2 会给它排 poll")
	}
	if res.Status != adapter.StatusSucceeded || res.RawStatus != rawOK {
		t.Errorf("状态 = %q / raw %q", res.Status, res.RawStatus)
	}
	if len(res.Inline) != 1 {
		t.Fatalf("产物数 = %d", len(res.Inline))
	}
	a := res.Inline[0]
	if a.Kind != adapter.KindBase64 || a.Base64 != payload {
		t.Errorf("产物 = %+v", a)
	}
	if a.Type != domain.AssetTypeImage || a.MIME != "image/png" {
		t.Errorf("产物类型 = %q / %q", a.Type, a.MIME)
	}
	if a.Width == nil || *a.Width != 1024 || a.Height == nil || *a.Height != 1024 {
		t.Errorf("画幅 = %+v / %+v", a.Width, a.Height)
	}
}

// TestInvokeImagesURL 验证 url 形态。response_format 是 mapping 里的一条配置，
// driver 说了不算，两种形态都得认。
func TestInvokeImagesURL(t *testing.T) {
	d := New(Driver{
		Renderer: mapping.NewRenderer(nil),
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(200, `{"output_format":"webp","data":[{"url":"https://cdn.example/a.webp"}]}`), nil
		})},
	})
	res, err := d.Invoke(context.Background(), testInput(domain.FamilyImages))
	if err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}
	a := res.Inline[0]
	if a.Kind != adapter.KindURL || a.URL != "https://cdn.example/a.webp" {
		t.Errorf("产物 = %+v", a)
	}
	if a.MIME != "image/webp" {
		t.Errorf("MIME = %q，应当跟随 output_format", a.MIME)
	}
}

// TestInvokeChatProducesTextArtifact 验证文本也走 ArtifactRef——
// 绕过资产记录会让 chat 产物没有血缘、做同款对它失效。
func TestInvokeChatProducesTextArtifact(t *testing.T) {
	var gotURL string
	d := New(Driver{
		ProtocolFamily: domain.FamilyChat,
		Renderer:       mapping.NewRenderer(nil),
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotURL = r.URL.String()
			return jsonResponse(200, `{"choices":[{"index":0,"message":{"role":"assistant","content":"你好"}}]}`), nil
		})},
	})
	if d.Name() != NameChat {
		t.Errorf("Name() = %q, want %q", d.Name(), NameChat)
	}

	res, err := d.Invoke(context.Background(), testInput(domain.FamilyChat))
	if err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}
	if want := "https://api.example" + PathChatCompletions; gotURL != want {
		t.Errorf("端点 = %q, want %q", gotURL, want)
	}
	a := res.Inline[0]
	if a.Type != domain.AssetTypeText || a.Kind != adapter.KindBase64 {
		t.Errorf("产物 = %+v", a)
	}
	raw, _ := base64.StdEncoding.DecodeString(a.Base64)
	if string(raw) != "你好" {
		t.Errorf("文本 = %q", raw)
	}
}

// TestInvokeErrorInBody200 钉死"200 但 body 里带 error"这条路径。
// 只看状态码会把一次失败当成功，再在解析产物时报一句不知所云的"没有产物"。
func TestInvokeErrorInBody200(t *testing.T) {
	d := New(Driver{
		Renderer: mapping.NewRenderer(nil),
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(200, `{"error":{"code":"content_policy_violation","message":"blocked"}}`), nil
		})},
	})
	_, err := d.Invoke(context.Background(), testInput(domain.FamilyImages))
	de, ok := err.(*domain.Error)
	if !ok {
		t.Fatalf("期望 *domain.Error，得到 %v", err)
	}
	if de.Code != domain.ErrorCode(domain.TaskErrorContentRejected) {
		t.Errorf("错误码 = %q, want content_rejected", de.Code)
	}
}

// TestReclassifyPromotesContentRejection 验证非 2xx 时 httpx 的状态码兜底
// 会被响应体里的协议特有错误码纠正过来。
func TestReclassifyPromotesContentRejection(t *testing.T) {
	d := New(Driver{
		Renderer: mapping.NewRenderer(nil),
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(400, `{"error":{"code":"content_policy_violation","message":"blocked"}}`), nil
		})},
	})
	_, err := d.Invoke(context.Background(), testInput(domain.FamilyImages))
	de, ok := err.(*domain.Error)
	if !ok {
		t.Fatalf("期望 *domain.Error，得到 %v", err)
	}
	if de.Code != domain.ErrorCode(domain.TaskErrorContentRejected) {
		t.Errorf("错误码 = %q, want content_rejected", de.Code)
	}
	if de.Retryable {
		t.Error("content_rejected 不可重试")
	}
}

// TestInvokeMissingCredentialFailsEarly 保证没配环境变量时的报错点名那个变量，
// 而不是等上游回 401 再去猜。
func TestInvokeMissingCredentialFailsEarly(t *testing.T) {
	d := New(Driver{Renderer: mapping.NewRenderer(nil)})
	in := testInput(domain.FamilyImages)
	in.Credential = ""
	_, err := d.Invoke(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "AIGC_PROVIDER_OPENAI_KEY") {
		t.Errorf("错误消息应当点名缺失的环境变量: %v", err)
	}
}

func TestParseSize(t *testing.T) {
	if w, h, ok := parseSize("1792x1024"); !ok || w != 1792 || h != 1024 {
		t.Errorf("parseSize = %d,%d,%v", w, h, ok)
	}
	if _, _, ok := parseSize("auto"); ok {
		t.Error("非法尺寸串应当返回 false 而不是 0x0")
	}
}
