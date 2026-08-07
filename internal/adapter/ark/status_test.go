package ark

import (
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// TestNormalize 逐值对拍上游状态到平台状态的映射。
//
// 这张表是本驱动最容易在上游改版时悄悄失准的地方：改错一个值不会有编译错误，
// 只会让线上任务卡在错误的状态里。因此每个上游取值都必须被显式钉死。
func TestNormalize(t *testing.T) {
	cases := []struct {
		raw  string
		want adapter.Status
	}{
		{"queued", adapter.StatusQueued},
		{"running", adapter.StatusRunning},
		{"succeeded", adapter.StatusSucceeded},
		{"failed", adapter.StatusFailed},
		// expired 是 Ark 独有的：任务跑完了但产物已过期。
		// 平台拿不到产物就是没产物，等价于失败，而不是第六种平台状态。
		{"expired", adapter.StatusFailed},

		// 大小写与空白不该影响判定。
		{"SUCCEEDED", adapter.StatusSucceeded},
		{"  running  ", adapter.StatusRunning},

		// 未知状态按 running：上游随时可能加新值，
		// 把在途任务判死比多轮询几圈代价大得多。
		{"cancelling", adapter.StatusRunning},
		{"", adapter.StatusRunning},
	}
	for _, c := range cases {
		if got := Normalize(c.raw); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// TestStatusMapCoversUpstreamVocabulary 保证 statusMap 与上游状态常量一一对上。
// 加了常量却忘了加映射会让新状态静默落进 running 分支。
func TestStatusMapCoversUpstreamVocabulary(t *testing.T) {
	for _, raw := range []string{statusQueued, statusRunning, statusSucceeded, statusFailed, statusExpired} {
		if _, ok := statusMap[raw]; !ok {
			t.Errorf("statusMap 缺少上游状态 %q", raw)
		}
	}
	if len(statusMap) != 5 {
		t.Errorf("statusMap 有 %d 条，期望 5 条（多出来的可能是拼错的 key）", len(statusMap))
	}
}

// TestClassify 钉死失败三分类。
//
// **只看 HTTP 状态码会分错**：SensitiveContentDetected 与 InvalidParameter
// 都是 400，前者要引导用户改提示词、后者要高亮参数芯片，前端呈现完全不同。
func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		err    apiError
		want   domain.TaskErrorCode
	}{
		{"审核拒绝的错误码", 400, apiError{Code: "SensitiveContentDetected"}, domain.TaskErrorContentRejected},
		{"输出审核", 400, apiError{Code: "OutputVideoSensitive"}, domain.TaskErrorContentRejected},
		{"参数错误的错误码", 400, apiError{Code: "InvalidParameter"}, domain.TaskErrorInvalidParam},
		{"限流", 429, apiError{Code: "RateLimitExceeded"}, domain.TaskErrorUpstreamRateLimited},
		{"欠费", 429, apiError{Code: "AccountOverdueError"}, domain.TaskErrorUpstreamRateLimited},
		{"服务端错误", 500, apiError{Code: "InternalServiceError"}, domain.TaskErrorInternal},

		// 没有错误码时靠文案兜底。
		{"只有文案的审核拒绝", 400, apiError{Message: "prompt contains sensitive content"}, domain.TaskErrorContentRejected},
		{"只有中文文案的审核拒绝", 400, apiError{Message: "输入内容涉及敏感信息"}, domain.TaskErrorContentRejected},
		{"只有文案的限流", 429, apiError{Message: "rate limit reached"}, domain.TaskErrorUpstreamRateLimited},

		// 文案与错误码都认不出时才落到状态码。
		{"未知码落状态码 400", 400, apiError{Code: "SomethingNew"}, domain.TaskErrorInvalidParam},
		{"未知码落状态码 429", 429, apiError{Code: "SomethingNew"}, domain.TaskErrorUpstreamRateLimited},
		{"未知码落状态码 503", 503, apiError{Code: "SomethingNew"}, domain.TaskErrorInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.status, c.err); got != c.want {
				t.Errorf("classify() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestFailureForExpired 钉死 expired 的归类。
//
// expired 是**我们轮询得太晚**，责任在平台不在用户：归 internal_error、
// 可重试、不扣费。归成 content_rejected 或 invalid_param 都会让用户
// 去改一段本来没问题的提示词。
func TestFailureForExpired(t *testing.T) {
	err := failureFor("expired", pollResponse{Status: "expired"})
	if err.Code != domain.TaskErrorInternal {
		t.Errorf("expired 的错误码 = %q, want %q", err.Code, domain.TaskErrorInternal)
	}
	if !err.Retryable {
		t.Error("expired 应当可重试：重跑一次就能拿到产物")
	}
	if err.Charged {
		t.Error("失败三分类一律不扣费")
	}
}

// TestFailureAlwaysUncharged 保证任何失败都不扣费——这是产品承诺，
// 不能靠各处调用方自觉传 false。
func TestFailureAlwaysUncharged(t *testing.T) {
	for _, code := range []domain.TaskErrorCode{
		domain.TaskErrorInvalidParam,
		domain.TaskErrorUpstreamRateLimited,
		domain.TaskErrorContentRejected,
		domain.TaskErrorInternal,
	} {
		e := failureFor("failed", pollResponse{Error: &apiError{Code: string(code)}})
		if e.Charged {
			t.Errorf("%q 的失败被标成已扣费", code)
		}
	}
}
