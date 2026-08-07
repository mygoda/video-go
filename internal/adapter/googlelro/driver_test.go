package googlelro

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
		BaseURL:       "https://generativelanguage.example",
		CredentialRef: "AIGC_PROVIDER_GOOGLE_KEY",
		AuthStyle:     domain.AuthStyleBearer,
	}
}

func testInput() adapter.SubmitInput {
	proto := domain.VideoProtocolGoogleLRO
	return adapter.SubmitInput{
		TaskID:   "t1",
		Provider: testProvider(),
		Model: domain.ModelConfig{
			ID:             "m1",
			Family:         domain.FamilyVideo,
			VideoProtocol:  &proto,
			RequestMapping: json.RawMessage(`{"rules":[{"from":"prompt","to":"instances[]","wrap":"text_part"}]}`),
		},
		UpstreamModel: "veo-3.0-generate",
		Prompt:        "雪山日出",
		Credential:    "test-credential-placeholder",
	}
}

// TestNormalize 钉死本协议唯一的状态信号：一个 done 布尔。
//
// 上游**没有状态枚举**——没有 queued / running / succeeded 这类字符串。
// statusMap 的 key 因此是布尔的字符串形式，它仍然存在是为了让
// "归一化关系有唯一出处"这件事在四个驱动里长得一样。
func TestNormalize(t *testing.T) {
	cases := []struct {
		raw  string
		want adapter.Status
	}{
		{"false", adapter.StatusRunning},
		{"true", adapter.StatusSucceeded},
		{"TRUE", adapter.StatusSucceeded},
		{" false ", adapter.StatusRunning},
		// 未知值按 running：判死一条还在跑的任务代价更大。
		{"maybe", adapter.StatusRunning},
		{"", adapter.StatusRunning},
	}
	for _, c := range cases {
		if got := Normalize(c.raw); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// TestRawStatus 保证落库的原始状态串就是布尔的字面形态。
// 编一个假的 "running" 会让看库的人以为上游给了状态枚举。
func TestRawStatus(t *testing.T) {
	if RawStatus(true) != "true" || RawStatus(false) != "false" {
		t.Errorf("RawStatus = %q / %q", RawStatus(true), RawStatus(false))
	}
	if _, ok := statusMap[RawStatus(true)]; !ok {
		t.Error("statusMap 的 key 必须与 RawStatus 的输出一致")
	}
	if _, ok := statusMap[RawStatus(false)]; !ok {
		t.Error("statusMap 的 key 必须与 RawStatus 的输出一致")
	}
	if len(statusMap) != 2 {
		t.Errorf("statusMap 有 %d 条，期望 2 条（done 只有两个取值）", len(statusMap))
	}
}

// TestClassify 钉死 gRPC 状态码到失败三分类的映射。
//
// 本协议不给错误码字符串，只给 google.rpc.Code 整数——这是唯一可靠的判据。
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  statusError
		want domain.TaskErrorCode
	}{
		{"INVALID_ARGUMENT", statusError{Code: 3, Message: "bad request"}, domain.TaskErrorInvalidParam},
		{"NOT_FOUND", statusError{Code: 5, Message: "model not found"}, domain.TaskErrorInvalidParam},
		{"RESOURCE_EXHAUSTED", statusError{Code: 8, Message: "exhausted"}, domain.TaskErrorUpstreamRateLimited},
		{"FAILED_PRECONDITION", statusError{Code: 9, Message: "precondition"}, domain.TaskErrorContentRejected},
		{"INTERNAL", statusError{Code: 13, Message: "oops"}, domain.TaskErrorInternal},
		{"UNAVAILABLE", statusError{Code: 14, Message: "try later"}, domain.TaskErrorInternal},
		{"DEADLINE_EXCEEDED", statusError{Code: 4, Message: "timeout"}, domain.TaskErrorInternal},

		// 文案先于状态码：PERMISSION_DENIED 既可能是安全拦截也可能是凭证没权限，
		// 两者的处置完全不同，只看码会把后者也报成"内容被拒"，
		// 让用户白改半天提示词而真正的问题在运维那边。
		{"安全拦截", statusError{Code: 7, Message: "blocked by responsible AI filters"}, domain.TaskErrorContentRejected},
		{"凭证没权限", statusError{Code: 7, Message: "The caller does not have permission: api key invalid"}, domain.TaskErrorInternal},
		{"配额", statusError{Code: 0, Message: "Quota exceeded for requests"}, domain.TaskErrorUpstreamRateLimited},

		// 认不出来的码落到 internal。
		{"未知码", statusError{Code: 99, Message: "??"}, domain.TaskErrorInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(0, c.err); got != c.want {
				t.Errorf("classify() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestSubmitReturnsOperationName 钉死本协议与另外两家最根本的差异：
// 轮询按 **operation name**（一个完整的相对路径）寻址，不是按 id。
func TestSubmitReturnsOperationName(t *testing.T) {
	var gotURL string
	d := New(Driver{
		Renderer: mapping.NewRenderer(nil),
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotURL = r.URL.String()
			return jsonResponse(200, `{"name":"models/veo-3.0-generate/operations/abc123","done":false}`), nil
		})},
	})

	res, err := d.Submit(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Submit 失败: %v", err)
	}
	if !strings.HasSuffix(gotURL, "/v1beta/models/veo-3.0-generate:predictLongRunning") {
		t.Errorf("提交端点 = %q", gotURL)
	}
	if res.UpstreamRef != "models/veo-3.0-generate/operations/abc123" {
		t.Errorf("UpstreamRef = %q，本协议里它是 operation name 而不是 id", res.UpstreamRef)
	}
	if res.Status != adapter.StatusQueued || res.RawStatus != "false" {
		t.Errorf("状态 = %q / raw %q", res.Status, res.RawStatus)
	}
}

// TestPollAddressesByOperationName 验证轮询地址由响应体决定：
// operation name 直接拼在 base_url 后面。
func TestPollAddressesByOperationName(t *testing.T) {
	var gotURL string
	d := New(Driver{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotURL = r.URL.String()
			return jsonResponse(200, `{"name":"models/x/operations/abc","done":false}`), nil
		})},
	})

	res, err := d.Poll(context.Background(), adapter.PollRequest{
		Provider:    testProvider(),
		UpstreamRef: "models/x/operations/abc",
		Credential:  "test-credential-placeholder",
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if gotURL != "https://generativelanguage.example/models/x/operations/abc" {
		t.Errorf("轮询端点 = %q", gotURL)
	}
	if res.Status != adapter.StatusRunning {
		t.Errorf("done=false 应当归一化为 running，实际 %q", res.Status)
	}
}

// TestPollDoneWithErrorIsFailed 验证 done=true 之后还要看 error 字段
// 才能分出成功与失败——这一步刻意不在 statusMap 里。
func TestPollDoneWithErrorIsFailed(t *testing.T) {
	d := New(Driver{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(200, `{"name":"op","done":true,"error":{"code":9,"message":"blocked by safety filters"}}`), nil
		})},
	})

	res, err := d.Poll(context.Background(), adapter.PollRequest{
		Provider: testProvider(), UpstreamRef: "op", Credential: "test-credential-placeholder",
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if res.Status != adapter.StatusFailed {
		t.Errorf("done=true 且有 error 应当归 failed，实际 %q", res.Status)
	}
	// RawStatus 仍然是 done 的字面值：上游的原始信号不因平台的判定而改写。
	if res.RawStatus != "true" {
		t.Errorf("RawStatus = %q, want true", res.RawStatus)
	}
	if res.Err == nil || res.Err.Code != domain.TaskErrorContentRejected {
		t.Errorf("错误 = %+v, want content_rejected", res.Err)
	}
	if res.Err.Charged {
		t.Error("失败三分类一律不扣费")
	}
}

// TestPollDoneWithBase64Artifact 验证内联 base64 产物。
func TestPollDoneWithBase64Artifact(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("fake-mp4"))
	d := New(Driver{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(200, `{"name":"op","done":true,"response":{"generateVideoResponse":{"generatedSamples":[
				{"video":{"bytesBase64Encoded":"`+payload+`","mimeType":"video/mp4","durationMillis":5000}}]}}}`), nil
		})},
	})

	res, err := d.Poll(context.Background(), adapter.PollRequest{
		Provider: testProvider(), UpstreamRef: "op", Credential: "test-credential-placeholder",
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if res.Status != adapter.StatusSucceeded || len(res.Artifacts) != 1 {
		t.Fatalf("结果 = %+v", res)
	}
	a := res.Artifacts[0]
	if a.Kind != adapter.KindBase64 || a.Base64 != payload {
		t.Errorf("产物 = %+v", a)
	}
	if a.DurationMS == nil || *a.DurationMS != 5000 {
		t.Errorf("时长 = %+v", a.DurationMS)
	}

	// base64 产物由驱动本地解码，不打网络。
	stream, err := d.FetchArtifact(context.Background(), a, adapter.PollRequest{Provider: testProvider()})
	if err != nil {
		t.Fatalf("FetchArtifact 失败: %v", err)
	}
	defer stream.Body.Close()
	raw, _ := io.ReadAll(stream.Body)
	if string(raw) != "fake-mp4" {
		t.Errorf("解码结果 = %q", raw)
	}
}

// TestPollDoneWithGCSArtifact 验证 gs:// 产物形态。
func TestPollDoneWithGCSArtifact(t *testing.T) {
	d := New(Driver{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(200, `{"name":"op","done":true,"response":{"generatedSamples":[
				{"video":{"uri":"gs://my-bucket/out/video.mp4","mimeType":"video/mp4"}}]}}`), nil
		})},
	})

	res, err := d.Poll(context.Background(), adapter.PollRequest{
		Provider: testProvider(), UpstreamRef: "op", Credential: "test-credential-placeholder",
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	a := res.Artifacts[0]
	if a.Kind != adapter.KindGCSURI || a.GCSURI != "gs://my-bucket/out/video.mp4" {
		t.Errorf("产物 = %+v", a)
	}
}

// TestGCSDownloadURL 验证 object 名里的斜杠被整体转义，
// 否则它会被当成路径分隔符、取到一个不存在的对象。
func TestGCSDownloadURL(t *testing.T) {
	url, err := gcsDownloadURL("gs://my-bucket/out/video.mp4")
	if err != nil {
		t.Fatalf("gcsDownloadURL 失败: %v", err)
	}
	if !strings.Contains(url, "out%2Fvideo.mp4") {
		t.Errorf("object 名里的斜杠必须转义成 %%2F: %s", url)
	}
	if !strings.HasSuffix(url, "?alt=media") {
		t.Errorf("必须走 media 下载形式: %s", url)
	}

	if _, err := gcsDownloadURL("https://example.com/a.mp4"); err == nil {
		t.Error("非 gs:// 的 URI 应当报错")
	}
}

// TestSubmitRequiresUpstreamModel 保证缺 upstream_model 时早失败。
// 本协议的模型名在 URL 路径里，缺了根本拼不出端点，不能靠 mapping 兜底。
func TestSubmitRequiresUpstreamModel(t *testing.T) {
	d := New(Driver{Renderer: mapping.NewRenderer(nil)})
	in := testInput()
	in.UpstreamModel = ""
	if _, err := d.Submit(context.Background(), in); err == nil {
		t.Fatal("缺 upstream_model 时应当失败")
	}
}
