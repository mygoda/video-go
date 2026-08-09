package predictions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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
		BaseURL: "https://api.example/",
		// 环境变量名，不是密钥。
		CredentialRef: "AIGC_PROVIDER_GPUGEEK_KEY",
		AuthStyle:     domain.AuthStyleBearer,
	}
}

// imageModel 的 mapping 就是迁移里播下的那一份形状：两个顶层字段，
// 全部生成参数走点号路径落进 input 这个子对象。
func imageModel() domain.ModelConfig {
	return domain.ModelConfig{
		ID:            "gpugeek-seedream-4-5",
		Family:        domain.FamilyPredictions,
		UpstreamModel: "Volcengine/Doubao-Seedream-4.5",
		RequestMapping: json.RawMessage(`{
			"body_template": {"model": "", "input": {}},
			"rules": [
				{"from": "model.upstream_model", "to": "model"},
				{"from": "prompt", "to": "input.prompt"},
				{"from": "params.size", "to": "input.size"},
				{"from": "params.watermark", "to": "input.watermark", "cast": "bool"}
			]
		}`),
	}
}

func videoModel() domain.ModelConfig {
	proto := domain.VideoProtocolPredictions
	return domain.ModelConfig{
		ID:            "gpugeek-seedance-2-0",
		Family:        domain.FamilyVideo,
		VideoProtocol: &proto,
		UpstreamModel: "Volcengine/Doubao-Seedance-2.0",
		RequestMapping: json.RawMessage(`{
			"body_template": {"model": "", "input": {}},
			"rules": [
				{"from": "model.upstream_model", "to": "model"},
				{"from": "prompt", "to": "input.prompt"},
				{"from": "params.duration", "to": "input.duration", "cast": "int"}
			]
		}`),
	}
}

func imageInput() adapter.SubmitInput {
	return adapter.SubmitInput{
		TaskID:        "t1",
		Provider:      testProvider(),
		Model:         imageModel(),
		UpstreamModel: "Volcengine/Doubao-Seedream-4.5",
		Prompt:        "白色桌面上的红色立方体",
		Params:        map[string]any{"size": "2048x2048", "watermark": false},
		// 测试里用占位串，绝不写真实凭证。
		Credential: "test-credential-placeholder",
	}
}

func videoInput() adapter.SubmitInput {
	in := imageInput()
	in.Model = videoModel()
	in.UpstreamModel = "Volcengine/Doubao-Seedance-2.0"
	in.Params = map[string]any{"duration": 4}
	return in
}

func syncFor(rt roundTripFunc) adapter.SyncDriver {
	return NewSyncDriver(Driver{
		Renderer:       mapping.NewRenderer(nil),
		HTTP:           &http.Client{Transport: rt},
		SettleInterval: time.Millisecond,
	})
}

func asyncFor(rt roundTripFunc) adapter.AsyncDriver {
	return NewAsyncDriver(Driver{
		Renderer: mapping.NewRenderer(nil),
		HTTP:     &http.Client{Transport: rt},
	})
}

// TestParseOutputBothShapes 是这个包最该有的一个测试。
//
// 实测出图回字符串数组、出视频回单个字符串。假定其一——无论假定哪一个——
// 都会在另一种模态上 panic，而且是在任务已经成功、已经计过费之后。
func TestParseOutputBothShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"图像：字符串数组", `["https://cdn.example/a.jpeg"]`, []string{"https://cdn.example/a.jpeg"}},
		{"图像：多张", `["https://cdn.example/a.jpeg","https://cdn.example/b.jpeg"]`,
			[]string{"https://cdn.example/a.jpeg", "https://cdn.example/b.jpeg"}},
		{"视频：单个字符串", `"https://cdn.example/v.mp4"`, []string{"https://cdn.example/v.mp4"}},
		{"在途：null", `null`, nil},
		{"空数组", `[]`, []string{}},
		{"数组里的空串被剔除", `["","https://cdn.example/a.jpeg"]`, []string{"https://cdn.example/a.jpeg"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseOutput(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("ParseOutput(%s) 报错: %v", tc.raw, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseOutput(%s) = %v，想要 %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ParseOutput(%s) = %v，想要 %v", tc.raw, got, tc.want)
				}
			}
		})
	}
}

// TestParseOutputRejectsUnknownShape 确认第三种形态是报错而不是静默返回空——
// 静默返回空会让一次成功的任务变成"成功但没有产物"，比失败更难查。
func TestParseOutputRejectsUnknownShape(t *testing.T) {
	if _, err := ParseOutput(json.RawMessage(`{"url":"https://cdn.example/a.jpeg"}`)); err == nil {
		t.Fatal("对象形态的 output 应当报错")
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]adapter.Status{
		"starting":   adapter.StatusQueued,
		"processing": adapter.StatusRunning,
		"succeeded":  adapter.StatusSucceeded,
		"failed":     adapter.StatusFailed,
		"canceled":   adapter.StatusCanceled,
		"  Failed ":  adapter.StatusFailed,
		// 未知状态按在途处理：上游随时可能加新状态，把在途任务判死代价更大。
		"reticulating": adapter.StatusRunning,
	}
	for raw, want := range cases {
		if got := Normalize(raw); got != want {
			t.Errorf("Normalize(%q) = %q，想要 %q", raw, got, want)
		}
	}
}

// TestInvokeImageOneRoundTrip 覆盖实测的主路径：提交那一次往返就是终态。
func TestInvokeImageOneRoundTrip(t *testing.T) {
	var gotURL string
	var gotAuth string
	var gotBody map[string]any
	calls := 0

	d := syncFor(func(r *http.Request) (*http.Response, error) {
		calls++
		gotURL = r.URL.String()
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		return jsonResponse(200, `{"id":"volcengine-1","status":"succeeded",
			"output":["https://cdn.example/a.jpeg?X-Tos-Expires=86400"],
			"metrics":{"predict_time":10.6}}`), nil
	})

	res, err := d.Invoke(context.Background(), imageInput())
	if err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}
	if calls != 1 {
		t.Errorf("终态响应不应再轮询，实际打了 %d 次", calls)
	}
	if gotURL != "https://api.example/predictions" {
		t.Errorf("提交地址 = %q", gotURL)
	}
	if gotAuth != "Bearer test-credential-placeholder" {
		t.Errorf("鉴权头 = %q", gotAuth)
	}
	if gotBody["model"] != "Volcengine/Doubao-Seedream-4.5" {
		t.Errorf("顶层 model = %v", gotBody["model"])
	}
	input, ok := gotBody["input"].(map[string]any)
	if !ok {
		t.Fatalf("input 不是对象: %v", gotBody["input"])
	}
	if input["prompt"] != "白色桌面上的红色立方体" || input["size"] != "2048x2048" {
		t.Errorf("input = %v", input)
	}
	// false 不是空值，必须原样落进请求体（否则上游按它自己的默认打水印）。
	if input["watermark"] != false {
		t.Errorf("watermark = %v，想要 false", input["watermark"])
	}

	// 同步族的 UpstreamRef 恒为空，L2 据此不为它安排轮询。
	if res.UpstreamRef != "" {
		t.Errorf("同步调用不应返回 upstream_ref，实际 %q", res.UpstreamRef)
	}
	if res.Status != adapter.StatusSucceeded || res.RawStatus != "succeeded" {
		t.Errorf("状态 = %q / %q", res.Status, res.RawStatus)
	}
	if len(res.Inline) != 1 {
		t.Fatalf("产物数 = %d", len(res.Inline))
	}
	a := res.Inline[0]
	if a.Kind != adapter.KindURL || a.Type != domain.AssetTypeImage || a.MIME != "image/jpeg" {
		t.Errorf("产物 = %q / %q / %q", a.Kind, a.Type, a.MIME)
	}
	if a.ExpiresAt == nil {
		t.Error("产物必须带过期时刻——24h 窗口是立刻转存的依据")
	}
}

// TestInvokeWaitsForTerminal 覆盖上游先回 processing 的情况。
// 同步族没有轮询这一步，不在这里等，任务就永远停在 running。
func TestInvokeWaitsForTerminal(t *testing.T) {
	calls := 0
	var pollURL string

	d := syncFor(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method == http.MethodPost {
			return jsonResponse(200, `{"id":"volcengine-2","status":"processing","output":null}`), nil
		}
		pollURL = r.URL.String()
		return jsonResponse(200, `{"id":"volcengine-2","status":"succeeded",
			"output":["https://cdn.example/a.png"]}`), nil
	})

	res, err := d.Invoke(context.Background(), imageInput())
	if err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}
	if calls != 2 {
		t.Errorf("应当提交一次再轮询一次，实际 %d 次", calls)
	}
	if pollURL != "https://api.example/predictions/volcengine-2" {
		t.Errorf("轮询地址 = %q", pollURL)
	}
	if res.Inline[0].MIME != "image/png" {
		t.Errorf("MIME 应按后缀推成 image/png，实际 %q", res.Inline[0].MIME)
	}
}

// TestInvokeFailedIsError 确认同步失败以 error 形态抛出。
// 返回一个 Status=failed 却不带 error 的结果，会被 L2 当成一次没有产物的成功。
func TestInvokeFailedIsError(t *testing.T) {
	d := syncFor(func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"id":"volcengine-3","status":"failed",
			"error":"prompt rejected by moderation","output":null}`), nil
	})

	_, err := d.Invoke(context.Background(), imageInput())
	if err == nil {
		t.Fatal("failed 状态必须以 error 返回")
	}
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("错误类型 = %T", err)
	}
	if de.Code != domain.ErrorCode(domain.TaskErrorContentRejected) {
		t.Errorf("失败分类 = %q，想要 %q", de.Code, domain.TaskErrorContentRejected)
	}
	if de.Charged {
		t.Error("上游失败不该计费")
	}
}

// TestPollVideoSingleStringOutput 是视频那一侧的形态：output 是**一个字符串**。
func TestPollVideoSingleStringOutput(t *testing.T) {
	d := asyncFor(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.example/predictions/volcengine-cgt-1" {
			t.Errorf("轮询地址 = %q", r.URL.String())
		}
		// input 被上游回填了我们没发过的 input_has_video——它必须被忽略，
		// 拿回显的 input 做任何断言都会错。
		return jsonResponse(200, `{"id":"volcengine-cgt-1","status":"succeeded",
			"input":{"prompt":"x","input_has_video":false},
			"output":"https://cdn.example/v.mp4?X-Tos-Expires=86400",
			"metrics":{"predict_time":255.8}}`), nil
	})

	res, err := d.Poll(context.Background(), adapter.PollRequest{
		TaskID:      "t1",
		Provider:    testProvider(),
		Model:       videoModel(),
		UpstreamRef: "volcengine-cgt-1",
		Credential:  "test-credential-placeholder",
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if res.Status != adapter.StatusSucceeded {
		t.Fatalf("状态 = %q", res.Status)
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("产物数 = %d", len(res.Artifacts))
	}
	a := res.Artifacts[0]
	if a.Kind != adapter.KindURL || a.Type != domain.AssetTypeVideo || a.MIME != "video/mp4" {
		t.Errorf("产物 = %q / %q / %q", a.Kind, a.Type, a.MIME)
	}
	if a.URL != "https://cdn.example/v.mp4?X-Tos-Expires=86400" {
		t.Errorf("产物地址 = %q", a.URL)
	}
}

// TestPollFailedClassifies 覆盖轮询到 failed 的分类。
// 这个协议不给结构化错误码，error 字段是一段人话，只能按关键词分——
// 只看 HTTP 状态码会把审核拒绝报成参数错误。
func TestPollFailedClassifies(t *testing.T) {
	cases := []struct {
		name string
		body string
		want domain.TaskErrorCode
	}{
		{"字符串错误：审核", `{"status":"failed","error":"content flagged as sensitive"}`,
			domain.TaskErrorContentRejected},
		{"对象错误：限流", `{"status":"failed","error":{"message":"rate limit exceeded"}}`,
			domain.TaskErrorUpstreamRateLimited},
		{"对象错误：参数", `{"status":"failed","error":{"message":"duration must be -1 or in range [4, 15] seconds"}}`,
			domain.TaskErrorInvalidParam},
		{"认不出来就落内部错误", `{"status":"failed","error":"something went sideways"}`,
			domain.TaskErrorInternal},
		{"上游没给原因", `{"status":"failed","error":null}`,
			domain.TaskErrorInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := asyncFor(func(*http.Request) (*http.Response, error) {
				return jsonResponse(200, tc.body), nil
			})
			res, err := d.Poll(context.Background(), adapter.PollRequest{
				Provider:    testProvider(),
				UpstreamRef: "volcengine-cgt-1",
				Credential:  "test-credential-placeholder",
			})
			if err != nil {
				t.Fatalf("Poll 失败: %v", err)
			}
			if res.Status != adapter.StatusFailed {
				t.Fatalf("状态 = %q", res.Status)
			}
			if res.Err == nil {
				t.Fatal("failed 必须带 Err")
			}
			if res.Err.Code != tc.want {
				t.Errorf("失败分类 = %q，想要 %q（消息 %q）", res.Err.Code, tc.want, res.Err.Message)
			}
			if res.Err.Charged {
				t.Error("上游失败不该计费")
			}
		})
	}
}

// TestPollIsIdempotent 兑现 AsyncDriver.Poll 的幂等要求：
// 它只发一个 GET、不写任何状态，连查两次结果必须一致。
func TestPollIsIdempotent(t *testing.T) {
	d := asyncFor(func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"id":"volcengine-cgt-1","status":"succeeded",
			"output":"https://cdn.example/v.mp4"}`), nil
	})
	req := adapter.PollRequest{
		Provider:    testProvider(),
		UpstreamRef: "volcengine-cgt-1",
		Credential:  "test-credential-placeholder",
	}
	first, err := d.Poll(context.Background(), req)
	if err != nil {
		t.Fatalf("第一次 Poll 失败: %v", err)
	}
	second, err := d.Poll(context.Background(), req)
	if err != nil {
		t.Fatalf("第二次 Poll 失败: %v", err)
	}
	if first.Status != second.Status || first.Artifacts[0].URL != second.Artifacts[0].URL {
		t.Errorf("两次轮询结果不一致: %+v / %+v", first, second)
	}
}

// TestSubmitVideoRejectedAtSubmit 覆盖实测的失败路径：上游在提交那一次
// 就以 400 拒掉（例 duration: 99）。此时必须以 error 抛出且不计费。
func TestSubmitVideoRejectedAtSubmit(t *testing.T) {
	d := asyncFor(func(*http.Request) (*http.Response, error) {
		return jsonResponse(400, `{"code":400,"message":"duration must be -1 or in range [4, 15] seconds"}`), nil
	})

	in := videoInput()
	in.Params = map[string]any{"duration": 99}
	_, err := d.Submit(context.Background(), in)
	if err == nil {
		t.Fatal("上游 400 必须以 error 返回")
	}
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("错误类型 = %T", err)
	}
	if de.Code != domain.ErrorCode(domain.TaskErrorInvalidParam) {
		t.Errorf("失败分类 = %q，想要 %q", de.Code, domain.TaskErrorInvalidParam)
	}
	if de.Charged {
		t.Error("提交被拒不该计费")
	}
}

// TestSubmitVideoReturnsRef 覆盖提交成功：拿到 id 与在途状态。
func TestSubmitVideoReturnsRef(t *testing.T) {
	d := asyncFor(func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"id":"volcengine-cgt-2","status":"processing","output":null}`), nil
	})
	res, err := d.Submit(context.Background(), videoInput())
	if err != nil {
		t.Fatalf("Submit 失败: %v", err)
	}
	if res.UpstreamRef != "volcengine-cgt-2" {
		t.Errorf("upstream_ref = %q", res.UpstreamRef)
	}
	if res.Status != adapter.StatusRunning || res.RawStatus != "processing" {
		t.Errorf("状态 = %q / %q", res.Status, res.RawStatus)
	}
}

// TestSubmitRejectsEmptyInput 确认 mapping 少写规则时报错指向配置，
// 而不是把一个上游只会回"参数错误"的空 input 发出去。
func TestSubmitRejectsEmptyInput(t *testing.T) {
	d := asyncFor(func(*http.Request) (*http.Response, error) {
		t.Fatal("请求体不合法时不该发出请求")
		return nil, nil
	})
	in := videoInput()
	in.Model.RequestMapping = json.RawMessage(`{"body_template":{"model":"","input":{}},"rules":[]}`)
	if _, err := d.Submit(context.Background(), in); err == nil {
		t.Fatal("空 input 应当在发请求前就被拦下")
	}
}

// TestFetchArtifactDoesNotLayerAuth 确认取产物不往已签名的直链上叠 Authorization——
// 有的对象存储会因为同时出现两种鉴权而直接拒绝。
func TestFetchArtifactDoesNotLayerAuth(t *testing.T) {
	var gotAuth string
	d := asyncFor(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		return &http.Response{
			StatusCode:    200,
			Header:        http.Header{"Content-Type": []string{"video/mp4"}},
			Body:          io.NopCloser(strings.NewReader("mp4-bytes")),
			ContentLength: 9,
		}, nil
	})

	stream, err := d.FetchArtifact(context.Background(),
		adapter.ArtifactRef{Kind: adapter.KindURL, URL: "https://cdn.example/v.mp4?sig=x", MIME: "video/mp4"},
		adapter.PollRequest{Provider: testProvider(), Credential: "test-credential-placeholder"})
	if err != nil {
		t.Fatalf("FetchArtifact 失败: %v", err)
	}
	defer stream.Body.Close()
	if gotAuth != "" {
		t.Errorf("直链请求不该带 Authorization，实际 %q", gotAuth)
	}
	if stream.MIME != "video/mp4" || stream.Bytes != 9 {
		t.Errorf("流 = %q / %d", stream.MIME, stream.Bytes)
	}
}

// TestFetchArtifactExpiredHint 确认 403 被翻译成能指出根因的一句话：
// 拿到 URL 却下不下来，绝大多数是转存跑在了 24h 过期之后。
func TestFetchArtifactExpiredHint(t *testing.T) {
	d := asyncFor(func(*http.Request) (*http.Response, error) {
		return jsonResponse(403, `AccessDenied`), nil
	})
	_, err := d.FetchArtifact(context.Background(),
		adapter.ArtifactRef{Kind: adapter.KindURL, URL: "https://cdn.example/v.mp4"},
		adapter.PollRequest{Provider: testProvider()})
	if err == nil || !strings.Contains(err.Error(), "已失效") {
		t.Fatalf("403 应当给出过期提示，实际 %v", err)
	}
}

// TestFamiliesSelectSides 是两个实例共用一个查找键的地基：
// Family() 必须与模型配置里的 protocol_family 那一列逐字对得上，
// adapter.IsAsync 正是拿这个值比对来决定走同步还是异步的。
func TestFamiliesSelectSides(t *testing.T) {
	sync := NewSyncDriver(Driver{})
	async := NewAsyncDriver(Driver{})

	if sync.Name() != Name || async.Name() != Name {
		t.Fatalf("两个实例必须共用查找键 %q，实际 %q / %q", Name, sync.Name(), async.Name())
	}
	if sync.Family() != domain.FamilyPredictions {
		t.Errorf("同步实例 Family = %q", sync.Family())
	}
	if async.Family() != domain.FamilyVideo {
		t.Errorf("异步实例 Family = %q", async.Family())
	}
	if async.DefaultPollInterval() != DefaultPollInterval {
		t.Errorf("默认轮询间隔 = %s", async.DefaultPollInterval())
	}
}

func TestImageMIME(t *testing.T) {
	cases := map[string]string{
		"https://cdn.example/a.jpeg?X-Tos-Expires=86400": "image/jpeg",
		"https://cdn.example/a.png":                      "image/png",
		"https://cdn.example/a.webp":                     "image/webp",
		"https://cdn.example/a":                          "image/jpeg",
	}
	for url, want := range cases {
		if got := imageMIME(url); got != want {
			t.Errorf("imageMIME(%q) = %q，想要 %q", url, got, want)
		}
	}
}
