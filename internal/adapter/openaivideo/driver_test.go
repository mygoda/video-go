package openaivideo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

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
		BaseURL:       "https://api.example",
		CredentialRef: "AIGC_PROVIDER_OPENAI_KEY",
		AuthStyle:     domain.AuthStyleBearer,
	}
}

func testInput() adapter.SubmitInput {
	proto := domain.VideoProtocolOpenAIVideo
	return adapter.SubmitInput{
		TaskID:   "t1",
		Provider: testProvider(),
		Model: domain.ModelConfig{
			ID:            "m1",
			Family:        domain.FamilyVideo,
			VideoProtocol: &proto,
			RequestMapping: json.RawMessage(`{"rules":[
				{"from":"prompt","to":"prompt"},
				{"from":"params.size","to":"size"}
			]}`),
		},
		UpstreamModel:  "sora-2",
		Prompt:         "海边日落",
		Params:         map[string]any{"size": "1280x720"},
		Credential:     "test-credential-placeholder",
		IdempotencyKey: "idem-1",
	}
}

// TestNormalize 逐值对拍上游状态到平台状态的映射。
//
// in_progress → running 是本表存在的最直白的理由：语义一致、叫法不同。
func TestNormalize(t *testing.T) {
	cases := []struct {
		raw  string
		want adapter.Status
	}{
		{"queued", adapter.StatusQueued},
		{"in_progress", adapter.StatusRunning},
		{"completed", adapter.StatusSucceeded},
		{"failed", adapter.StatusFailed},

		{"IN_PROGRESS", adapter.StatusRunning},
		{" completed ", adapter.StatusSucceeded},

		// 上游没有 canceled 状态，取消是平台侧的动作。
		{"canceled", adapter.StatusRunning},
		{"", adapter.StatusRunning},
	}
	for _, c := range cases {
		if got := Normalize(c.raw); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestStatusMapCoversUpstreamVocabulary(t *testing.T) {
	for _, raw := range []string{statusQueued, statusInProgress, statusCompleted, statusFailed} {
		if _, ok := statusMap[raw]; !ok {
			t.Errorf("statusMap 缺少上游状态 %q", raw)
		}
	}
	if len(statusMap) != 4 {
		t.Errorf("statusMap 有 %d 条，期望 4 条", len(statusMap))
	}
}

// TestClassify 钉死失败三分类。审核拒绝在这套协议里同样是 400，
// 光看状态码分不出它与参数错误，而两者在前端是完全不同的呈现。
func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		err    apiError
		want   domain.TaskErrorCode
	}{
		{"审核拒绝", 400, apiError{Code: "moderation_blocked"}, domain.TaskErrorContentRejected},
		{"内容策略", 400, apiError{Code: "content_policy_violation"}, domain.TaskErrorContentRejected},
		{"限流", 429, apiError{Code: "rate_limit_exceeded"}, domain.TaskErrorUpstreamRateLimited},
		{"配额耗尽", 429, apiError{Code: "insufficient_quota"}, domain.TaskErrorUpstreamRateLimited},
		{"参数错误", 400, apiError{Type: "invalid_request_error"}, domain.TaskErrorInvalidParam},
		{"不支持的时长", 400, apiError{Code: "unsupported_duration"}, domain.TaskErrorInvalidParam},
		{"服务端错误", 500, apiError{Code: "server_error"}, domain.TaskErrorInternal},

		{"只有文案的审核拒绝", 400, apiError{Message: "Your request was rejected by the safety system"}, domain.TaskErrorContentRejected},
		{"只有文案的限流", 429, apiError{Message: "Rate limit reached for requests"}, domain.TaskErrorUpstreamRateLimited},
		{"未知码落状态码", 422, apiError{Code: "brand_new"}, domain.TaskErrorInvalidParam},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.status, c.err); got != c.want {
				t.Errorf("classify() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestSubmitCarriesIdempotencyKey 保证提交带幂等键。
// 视频提交是分钟级且计费不菲，一次超时重发就是重复计费。
func TestSubmitCarriesIdempotencyKey(t *testing.T) {
	var gotKey, gotURL string
	d := New(Driver{
		Renderer: mapping.NewRenderer(nil),
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotKey = r.Header.Get("Idempotency-Key")
			gotURL = r.URL.String()
			return jsonResponse(200, `{"id":"video_1","status":"queued"}`), nil
		})},
	})

	res, err := d.Submit(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Submit 失败: %v", err)
	}
	if gotKey != "idem-1" {
		t.Errorf("Idempotency-Key = %q, want idem-1", gotKey)
	}
	if gotURL != "https://api.example"+PathSubmit {
		t.Errorf("提交端点 = %q", gotURL)
	}
	if res.UpstreamRef != "video_1" || res.Status != adapter.StatusQueued {
		t.Errorf("结果 = %+v", res)
	}
}

// TestPollSucceededProducesBinaryArtifact 钉死本协议最关键的差异：
// 产物**没有 URL**，只能靠带鉴权的流式端点取字节，因此 Kind 必须是 binary。
func TestPollSucceededProducesBinaryArtifact(t *testing.T) {
	expires := time.Now().Add(2 * time.Hour).Unix()
	d := New(Driver{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(200, `{"id":"video_1","status":"completed","size":"1280x720","seconds":8,"expires_at":`+
				strconv.FormatInt(expires, 10)+`}`), nil
		})},
	})

	res, err := d.Poll(context.Background(), adapter.PollRequest{
		Provider:    testProvider(),
		UpstreamRef: "video_1",
		Credential:  "test-credential-placeholder",
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("产物数 = %d", len(res.Artifacts))
	}
	a := res.Artifacts[0]
	if a.Kind != adapter.KindBinary {
		t.Errorf("产物 Kind = %q, want binary（本协议的产物是裸字节流，不是 URL）", a.Kind)
	}
	if a.URL != "" {
		t.Error("本协议不该编造 URL")
	}
	if a.ExpiresAt == nil {
		t.Error("上游给了显式 expires_at，必须原样带上")
	}
	if a.Width == nil || *a.Width != 1280 || a.Height == nil || *a.Height != 720 {
		t.Errorf("画幅解析错误: %+v / %+v", a.Width, a.Height)
	}
	if a.DurationMS == nil || *a.DurationMS != 8000 {
		t.Errorf("时长 = %+v, want 8000ms", a.DurationMS)
	}
}

// TestPollReportsProgress 验证 0-100 的整数进度被换算成 0-1 的比例。
func TestPollReportsProgress(t *testing.T) {
	d := New(Driver{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(200, `{"id":"video_1","status":"in_progress","progress":40}`), nil
		})},
	})
	res, err := d.Poll(context.Background(), adapter.PollRequest{
		Provider: testProvider(), UpstreamRef: "video_1", Credential: "test-credential-placeholder",
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if res.Status != adapter.StatusRunning || res.RawStatus != "in_progress" {
		t.Errorf("状态 = %q / raw %q", res.Status, res.RawStatus)
	}
	if res.Progress == nil || *res.Progress != 0.4 {
		t.Errorf("进度 = %+v, want 0.4", res.Progress)
	}
}

// TestFetchArtifactUsesAuthenticatedContentEndpoint 验证取产物打的是
// /content 端点、带鉴权、且流式交出字节。
func TestFetchArtifactUsesAuthenticatedContentEndpoint(t *testing.T) {
	var gotURL, gotAuth string
	d := New(Driver{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotURL, gotAuth = r.URL.String(), r.Header.Get("Authorization")
			return &http.Response{
				StatusCode:    200,
				Header:        http.Header{"Content-Type": []string{"video/mp4"}},
				Body:          io.NopCloser(strings.NewReader("mp4")),
				ContentLength: 3,
			}, nil
		})},
	})

	stream, err := d.FetchArtifact(context.Background(),
		adapter.ArtifactRef{Kind: adapter.KindBinary, MIME: ArtifactMIME},
		adapter.PollRequest{Provider: testProvider(), UpstreamRef: "video_1", Credential: "test-credential-placeholder"})
	if err != nil {
		t.Fatalf("FetchArtifact 失败: %v", err)
	}
	defer stream.Body.Close()

	if !strings.HasSuffix(gotURL, "/v1/videos/video_1/content") {
		t.Errorf("取产物端点 = %q", gotURL)
	}
	if gotAuth != "Bearer test-credential-placeholder" {
		t.Errorf("取产物必须带鉴权，实际 = %q", gotAuth)
	}
	if stream.MIME != "video/mp4" {
		t.Errorf("MIME = %q", stream.MIME)
	}
}

// TestPollIntervalBacksOff 验证指数退避到 30s 封顶。
// 退避是协议知识，必须留在驱动里；让 L2 自己写一套等于把协议差异搬到 L0 之上。
func TestPollIntervalBacksOff(t *testing.T) {
	d := Driver{}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 15 * time.Second},
		{1, 30 * time.Second},
		{5, 30 * time.Second},
	}
	for _, c := range cases {
		if got := d.PollIntervalAt(c.attempt); got != c.want {
			t.Errorf("PollIntervalAt(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}
