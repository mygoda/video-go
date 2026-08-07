package ark

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/adapter/mapping"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// roundTripFunc 让测试不打网络就能喂给驱动一份录制的响应。
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
		ID:      "p1",
		BaseURL: "https://ark.example/",
		// 环境变量名，不是密钥。
		CredentialRef: "AIGC_PROVIDER_ARK_KEY",
		AuthStyle:     domain.AuthStyleBearer,
	}
}

func testModel() domain.ModelConfig {
	proto := domain.VideoProtocolArk
	return domain.ModelConfig{
		ID:            "m1",
		Family:        domain.FamilyVideo,
		VideoProtocol: &proto,
		UpstreamModel: "doubao-seedance-test",
		RequestMapping: json.RawMessage(`{
			"body_template": {"parameters": {}},
			"rules": [
				{"from": "model.upstream_model", "to": "model"},
				{"from": "prompt", "to": "content[]", "wrap": "text_part"},
				{"from": "inputs.first_frame", "to": "content[]", "wrap": "image_url_part"},
				{"from": "params.resolution", "to": "parameters.resolution", "value_map": {"768p": "720p"}}
			]
		}`),
	}
}

func testInput() adapter.SubmitInput {
	return adapter.SubmitInput{
		TaskID:        "t1",
		Provider:      testProvider(),
		Model:         testModel(),
		UpstreamModel: "doubao-seedance-test",
		Prompt:        "一只在雨里的猫",
		Params:        map[string]any{"resolution": "768p"},
		Inputs: []adapter.InputRef{
			{Slot: "first_frame", URL: "https://cdn.example/f.png"},
		},
		// 测试里用占位串，绝不写真实凭证。
		Credential: "test-credential-placeholder",
	}
}

// TestSubmitRendersBodyAndReturnsTaskID 验证提交走的是 mapping 渲染出的 body，
// 且 content 数组的顺序就是规则顺序——后者对 Ark 有生成语义，不是实现细节。
func TestSubmitRendersBodyAndReturnsTaskID(t *testing.T) {
	var gotURL string
	var gotBody map[string]any
	var gotAuth string

	d := New(Driver{
		Renderer: mapping.NewRenderer(nil),
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotURL = r.URL.String()
			gotAuth = r.Header.Get("Authorization")
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			return jsonResponse(200, `{"id":"cgt-123","status":"queued"}`), nil
		})},
	})

	res, err := d.Submit(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Submit 失败: %v", err)
	}

	if want := "https://ark.example" + PathSubmit; gotURL != want {
		t.Errorf("提交端点 = %q, want %q", gotURL, want)
	}
	if gotAuth != "Bearer test-credential-placeholder" {
		t.Errorf("鉴权头 = %q，凭证应当以 Bearer 形式放在 header 里", gotAuth)
	}
	if gotBody["model"] != "doubao-seedance-test" {
		t.Errorf("body.model = %v", gotBody["model"])
	}

	content, ok := gotBody[FieldContent].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("body.content = %v，期望两段（文本 + 首帧图）", gotBody[FieldContent])
	}
	// 规则顺序即数组顺序：prompt 在前、首帧图在后。调换会改变生成语义。
	if first := content[0].(map[string]any); first["type"] != "text" {
		t.Errorf("content[0].type = %v, want text", first["type"])
	}
	if second := content[1].(map[string]any); second["type"] != "image_url" {
		t.Errorf("content[1].type = %v, want image_url", second["type"])
	}

	params := gotBody[FieldParameters].(map[string]any)
	if params["resolution"] != "720p" {
		t.Errorf("value_map 未生效：parameters.resolution = %v, want 720p", params["resolution"])
	}

	if res.UpstreamRef != "cgt-123" {
		t.Errorf("UpstreamRef = %q, want cgt-123", res.UpstreamRef)
	}
	if res.Status != adapter.StatusQueued || res.RawStatus != "queued" {
		t.Errorf("状态 = %q / raw %q", res.Status, res.RawStatus)
	}
}

// TestPollSucceededProducesURLArtifact 验证成功轮询产出一件 KindURL 产物，
// 且带上 24h 的过期时刻——后者是 L3 判断"转存有没有跑在过期前面"的依据。
func TestPollSucceededProducesURLArtifact(t *testing.T) {
	var gotURL string
	d := New(Driver{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotURL = r.URL.String()
			return jsonResponse(200, `{"id":"cgt-123","status":"succeeded","content":{"video_url":"https://cdn.example/v.mp4"}}`), nil
		})},
	})

	res, err := d.Poll(context.Background(), adapter.PollRequest{
		TaskID:      "t1",
		Provider:    testProvider(),
		Model:       testModel(),
		UpstreamRef: "cgt-123",
		Credential:  "test-credential-placeholder",
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}

	if !strings.HasSuffix(gotURL, "/api/v3/contents/generations/tasks/cgt-123") {
		t.Errorf("轮询端点 = %q，应当按 task id 寻址", gotURL)
	}
	if res.Status != adapter.StatusSucceeded {
		t.Fatalf("状态 = %q, want succeeded", res.Status)
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("产物数 = %d, want 1", len(res.Artifacts))
	}
	a := res.Artifacts[0]
	if a.Kind != adapter.KindURL || a.URL != "https://cdn.example/v.mp4" {
		t.Errorf("产物 = %+v", a)
	}
	if a.ExpiresAt == nil {
		t.Error("产物必须带过期时刻，L3 靠它监控转存是否跑在过期前面")
	}
}

// TestPollExpiredIsFailed 钉死 expired 的完整行为：状态归 failed、
// 原始状态串仍然如实回传（排障要看得到它到底是 failed 还是 expired）。
func TestPollExpiredIsFailed(t *testing.T) {
	d := New(Driver{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(200, `{"id":"cgt-123","status":"expired"}`), nil
		})},
	})

	res, err := d.Poll(context.Background(), adapter.PollRequest{
		Provider:    testProvider(),
		UpstreamRef: "cgt-123",
		Credential:  "test-credential-placeholder",
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if res.Status != adapter.StatusFailed {
		t.Errorf("状态 = %q, want failed", res.Status)
	}
	if res.RawStatus != "expired" {
		t.Errorf("RawStatus = %q，上游原始状态必须原样回传供排障", res.RawStatus)
	}
	if res.Err == nil || res.Err.Code != domain.TaskErrorInternal {
		t.Errorf("错误 = %+v, want internal_error", res.Err)
	}
}

// TestSubmitMissingCredentialFailsEarly 保证没配环境变量时给出的是
// 一句能直接指出部署问题的话，而不是等上游回 401 再去猜。
func TestSubmitMissingCredentialFailsEarly(t *testing.T) {
	d := New(Driver{Renderer: mapping.NewRenderer(nil)})
	in := testInput()
	in.Credential = ""

	_, err := d.Submit(context.Background(), in)
	if err == nil {
		t.Fatal("凭证为空时应当失败")
	}
	if !strings.Contains(err.Error(), "AIGC_PROVIDER_ARK_KEY") {
		t.Errorf("错误消息应当点名缺失的环境变量: %v", err)
	}
}

// TestFetchArtifactStreams 保证取产物走的是流式，且不往已签名的直链上
// 再叠一层 Authorization——有的对象存储会因为两种鉴权并存而直接拒绝。
func TestFetchArtifactStreams(t *testing.T) {
	var sawAuth bool
	d := New(Driver{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			sawAuth = r.Header.Get("Authorization") != ""
			return &http.Response{
				StatusCode:    200,
				Header:        http.Header{"Content-Type": []string{"video/mp4"}},
				Body:          io.NopCloser(strings.NewReader("fake-mp4-bytes")),
				ContentLength: 14,
			}, nil
		})},
	})

	stream, err := d.FetchArtifact(context.Background(),
		adapter.ArtifactRef{Kind: adapter.KindURL, URL: "https://cdn.example/v.mp4", MIME: "video/mp4"},
		adapter.PollRequest{Provider: testProvider(), Credential: "test-credential-placeholder"})
	if err != nil {
		t.Fatalf("FetchArtifact 失败: %v", err)
	}
	defer stream.Body.Close()

	if sawAuth {
		t.Error("不应往已签名的产物直链上叠 Authorization 头")
	}
	raw, _ := io.ReadAll(stream.Body)
	if string(raw) != "fake-mp4-bytes" {
		t.Errorf("流内容 = %q", raw)
	}
	if stream.Bytes != 14 {
		t.Errorf("Bytes = %d, want 14", stream.Bytes)
	}
}

// TestReclassifyPromotesContentRejection 验证非 2xx 响应体里的协议特有错误码
// 能把 httpx 的状态码兜底分类纠正过来——审核拒绝也是 400，
// 不纠正就会被当成参数错误反复重试。
func TestReclassifyPromotesContentRejection(t *testing.T) {
	d := New(Driver{
		Renderer: mapping.NewRenderer(nil),
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(400, `{"error":{"code":"SensitiveContentDetected","message":"blocked"}}`), nil
		})},
	})

	_, err := d.Submit(context.Background(), testInput())
	var de *domain.Error
	if err == nil || !asDomainError(err, &de) {
		t.Fatalf("期望 *domain.Error，得到 %v", err)
	}
	if de.Code != domain.ErrorCode(domain.TaskErrorContentRejected) {
		t.Errorf("错误码 = %q, want content_rejected", de.Code)
	}
	if de.Retryable {
		t.Error("content_rejected 不可重试：同一段提示词再送一万次仍然会被拒")
	}
}

func asDomainError(err error, target **domain.Error) bool {
	de, ok := err.(*domain.Error)
	if ok {
		*target = de
	}
	return ok
}
