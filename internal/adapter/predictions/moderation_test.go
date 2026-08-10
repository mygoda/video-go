package predictions

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// realPersonRejectBody 是 DEM-100 单变量对照实验里录下来的真实响应体：
// 写实画风 + 首帧含可辨认真人脸，上游 seedance 在**提交那一次**就 400 拒收，
// 同图重放照样拒。A 组 3/3 复现，这就是本包要归对类的那一条。
const realPersonRejectBody = `{"code":400,"message":"The request failed because the input image 'content[1]' may contain real person.","requestId":"0365ba0e-7b3a-4a1e-9c2f-5d8e1f2a3b4c"}`

// sensitiveTextRejectBody 是主库里**已经分对**的那一条（seedream 文本命中审核）。
// 它是回归钉子：本次补词只许把漏掉的那类捞回来，不许动已经对的这条。
const sensitiveTextRejectBody = `{"code":400,"message":"The request failed because the input text may contain sensitive information.","requestId":"9f41c7d2-1a55-4b60-8e7c-2c9d0b6a4e13"}`

// TestClassifyModerationReject 钉住本票的核心缺口：上游把审核结论写成人话，
// 一个既有关键词都不命中，classify 就回 internal，reclassify 认不出线索便
// 保留 httpx 按 400 给出的 invalid_param——于是一次必然失败的审核拒绝
// 被告知用户"改参数重提"。
func TestClassifyModerationReject(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{"DEM-100 实测：输入图片含真人", "上游返回 400: " + realPersonRejectBody},
		{"复数措辞", `{"message":"input image may contain real people"}`},
		{"中文措辞", `{"message":"输入图片中包含真人"}`},
		{"违反内容政策", `{"message":"Your request violates our content policy."}`},
		{"使用政策", `{"message":"This image is not permitted under our usage policy."}`},
		{"政策违规", `{"message":"policy violation detected in the submitted image"}`},
		{"内容过滤器", `{"message":"The prompt was rejected by the content filter."}`},
		{"被谁拦下的", `{"message":"Request blocked by our safety system."}`},
		{"NSFW", `{"message":"NSFW content detected in output frame"}`},
		{"明令禁止", `{"message":"The submitted image contains prohibited content."}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.msg); got != domain.TaskErrorContentRejected {
				t.Errorf("classify(%q) = %q，想要 %q", tc.msg, got, domain.TaskErrorContentRejected)
			}
		})
	}
}

// TestClassifySensitiveRegression 是回归钉子：主库里那条 sensitive 文案
// 在补词之前就分对了，补词之后必须还是同一个分类。
func TestClassifySensitiveRegression(t *testing.T) {
	msg := "上游返回 400: " + sensitiveTextRejectBody
	if got := classify(msg); got != domain.TaskErrorContentRejected {
		t.Errorf("classify(%q) = %q，想要 %q", msg, got, domain.TaskErrorContentRejected)
	}
}

// TestClassifyKeepsInvalidParam 是补词的**误伤边界**。
//
// 审核那一支排在 invalid 之前，一次误命中就把参数错误判成 content_rejected：
// Retryable 变 false、文案变成"换素材或改提示词"，而用户真正该改的是一个
// 数值。这里逐条钉住最容易被新词吃掉的那些参数错误。
func TestClassifyKeepsInvalidParam(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		// 最容易误伤的一条：Flux 系有个正经参数就叫 safety_tolerance。
		// 收裸 safety 会把这条判成内容违规，因此本表只收 content filter 这类限定词。
		{"参数名里带 safety", `{"message":"safety_tolerance must be in range [0, 6]"}`},
		// 裸 policy 是正经技术名词，因此只收 content/usage policy 这类两词形式。
		{"重试策略配置错误", `{"message":"invalid retry policy: max_attempts must be positive"}`},
		{"存储桶策略", `{"message":"invalid bucket policy on the referenced object"}`},
		{"尺寸非法", `{"message":"size must be one of 1024x1024, 2048x2048"}`},
		{"缺字段", `{"message":"missing required field: prompt"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.msg); got != domain.TaskErrorInvalidParam {
				t.Errorf("classify(%q) = %q，想要 %q", tc.msg, got, domain.TaskErrorInvalidParam)
			}
		})
	}
}

// TestClassifyNotAllowedIsNotModeration 钉住裸 not allowed 没有被收进审核表。
//
// 断言只钉"不是审核拒绝"而不钉具体分类，是因为这几条今天落在 internal_error
// （既有的 invalid 词表里没有 not allowed）——那是另一个缺口，不是本票的活。
// 本票要保证的是：补审核词的时候没有把这类参数问题一起吃进来，
// 否则用户会被告知"你的内容违规"，而他该改的是一个取值。
func TestClassifyNotAllowedIsNotModeration(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{"取值不允许", `{"message":"value 99 is not allowed for parameter duration"}`},
		{"模型不支持该参数", `{"message":"parameter watermark is not allowed for this model"}`},
		{"账号被封", `{"message":"your account has been blocked"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.msg); got == domain.TaskErrorContentRejected {
				t.Errorf("classify(%q) 误判成 %q", tc.msg, got)
			}
		})
	}
}

// TestSubmitRealPersonRejectIsContentRejected 走完整的 DEM-100 路径：
// 提交 → 上游 400 → httpx 按状态码给 invalid_param → reclassify 纠正。
//
// Retryable 一起钉：invalid_param 的 Retryable=true 语义是"用户改完参数重提"，
// 而 DEM-100 已证明同图重放必然再拒，把它标成可重试就是在骗用户反复重提。
func TestSubmitRealPersonRejectIsContentRejected(t *testing.T) {
	d := asyncFor(func(*http.Request) (*http.Response, error) {
		return jsonResponse(400, realPersonRejectBody), nil
	})

	_, err := d.Submit(context.Background(), videoInput())
	if err == nil {
		t.Fatal("上游审核拒绝必须以 error 返回")
	}
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("错误类型 = %T", err)
	}
	if de.Code != domain.ErrorCode(domain.TaskErrorContentRejected) {
		t.Fatalf("失败分类 = %q，想要 %q（消息 %q）", de.Code, domain.TaskErrorContentRejected, de.Message)
	}
	if de.Retryable {
		t.Error("审核拒绝不可重试：DEM-100 实测同图重放第二次照样被拒")
	}
	if de.Charged {
		t.Error("上游拒收不该计费")
	}
	assertUserFacingModerationMessage(t, de.Message, "0365ba0e-7b3a-4a1e-9c2f-5d8e1f2a3b4c")
}

// TestPollRealPersonRejectIsContentRejected 覆盖另一条入口：上游先收下任务、
// 轮询时才回 failed。这条路走的是 failureFor，与提交那条不是同一个函数。
func TestPollRealPersonRejectIsContentRejected(t *testing.T) {
	d := asyncFor(func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"id":"volcengine-cgt-9","status":"failed",
			"error":{"message":"The request failed because the input image 'content[1]' may contain real person. requestId: 0365ba0e-7b3a-4a1e-9c2f-5d8e1f2a3b4c"}}`), nil
	})

	res, err := d.Poll(context.Background(), adapter.PollRequest{
		Provider:    testProvider(),
		UpstreamRef: "volcengine-cgt-9",
		Credential:  "test-credential-placeholder",
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if res.Err == nil {
		t.Fatal("failed 必须带 Err")
	}
	if res.Err.Code != domain.TaskErrorContentRejected {
		t.Fatalf("失败分类 = %q，想要 %q（消息 %q）",
			res.Err.Code, domain.TaskErrorContentRejected, res.Err.Message)
	}
	if res.Err.Retryable {
		t.Error("审核拒绝不可重试")
	}
}

// TestModerationMessageIsUserFacing 直接钉文案本身。
//
// 它不经过 classify，因此**关键词表怎么改都不影响它**——这是故意的：
// 变异掉关键词时红的应当只有"分类"那几条，文案这条必须仍然是绿的，
// 否则说不清红是因为分类塌了还是因为文案塌了。
func TestModerationMessageIsUserFacing(t *testing.T) {
	msg := moderationMessage("上游返回 400: " + realPersonRejectBody)
	// 打出来是为了 go test -v 能直接读到用户最终看到的那句话——
	// 这句文案是本票的交付物之一，靠断言反推读不出全貌。
	t.Logf("用户可见文案 = %s", msg)
	assertUserFacingModerationMessage(t, msg, "0365ba0e-7b3a-4a1e-9c2f-5d8e1f2a3b4c")

	if !strings.Contains(msg, "真人") {
		t.Errorf("真人拒收要说清是什么被拒了，实际 %q", msg)
	}

	// 主库那条文本命中审核的分类没变，但文案同样换成中文——
	// 它此前也是把整段英文 JSON 甩给用户的。
	textMsg := moderationMessage("上游返回 400: " + sensitiveTextRejectBody)
	assertUserFacingModerationMessage(t, textMsg, "9f41c7d2-1a55-4b60-8e7c-2c9d0b6a4e13")
	if !strings.Contains(textMsg, "提示词") {
		t.Errorf("文本命中审核要指向提示词，实际 %q", textMsg)
	}
}

// TestModerationMessageWithoutRequestID 覆盖上游没给追踪号的形态：
// 少了它文案照出，不能因为抠不到 id 就把整段英文又甩回给用户。
func TestModerationMessageWithoutRequestID(t *testing.T) {
	msg := moderationMessage(`{"message":"The prompt was rejected by the content filter."}`)
	if strings.Contains(msg, "上游追踪号") {
		t.Errorf("没有 requestId 时不该出现空的追踪号，实际 %q", msg)
	}
	if !strings.Contains(msg, "内容审核未通过") {
		t.Errorf("文案 = %q", msg)
	}
	if strings.Contains(msg, "The prompt was rejected") {
		t.Errorf("英文原文不该整段进用户文案，实际 %q", msg)
	}
}

// TestModerationMessageLogsUpstreamOriginal 钉住"原文不能丢"这一半：
// 用户拿到的是中文，排障的人要的是上游那段英文，它必须落在日志里。
func TestModerationMessageLogsUpstreamOriginal(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	moderationMessage("上游返回 400: " + realPersonRejectBody)

	logged := buf.String()
	if !strings.Contains(logged, "may contain real person") {
		t.Errorf("上游英文原文必须进日志，实际日志 %q", logged)
	}
	if !strings.Contains(logged, "0365ba0e-7b3a-4a1e-9c2f-5d8e1f2a3b4c") {
		t.Errorf("追踪号必须进日志，实际日志 %q", logged)
	}
}

// assertUserFacingModerationMessage 断言一段文案适合直接给用户看：
// 中文、带得走的追踪号、不含整段英文原文、不劝重试。
func assertUserFacingModerationMessage(t *testing.T, msg, requestID string) {
	t.Helper()

	if !strings.Contains(msg, "内容审核未通过") {
		t.Errorf("用户可见文案要说清是审核拒绝，实际 %q", msg)
	}
	if !strings.Contains(msg, requestID) {
		t.Errorf("用户可见文案必须保留上游追踪号 %s，实际 %q", requestID, msg)
	}
	// 英文原文整段不该出现：它是给排障的人看的，进了日志就够了。
	for _, fragment := range []string{
		"The request failed",
		"may contain real person",
		`{"code":400`,
		"requestId",
	} {
		if strings.Contains(msg, fragment) {
			t.Errorf("英文原文片段 %q 不该出现在用户文案里，实际 %q", fragment, msg)
		}
	}
	// DEM-100 已证明同图重放必然再拒，劝重试就是在骗用户。
	for _, bad := range []string{"稍后重试", "请重试", "再试一次"} {
		if strings.Contains(msg, bad) {
			t.Errorf("审核拒绝不该劝重试（%q），实际 %q", bad, msg)
		}
	}
}
