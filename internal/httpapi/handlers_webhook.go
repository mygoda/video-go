package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// webhookSecretEnvPrefix 是每 driver 一个共享密钥的环境变量前缀。
// 例如 ark 的密钥读 AIGC_WEBHOOK_SECRET_ARK。
const webhookSecretEnvPrefix = "AIGC_WEBHOOK_SECRET_"

// handleWebhook 是上游任务回调入口。
//
// **回调与轮询是同源的两条幂等路径**：回调只是把下一次轮询提前，它自己不解析
// 上游报文、不推状态机。任何一条都能独立把任务推到终态，因此回调丢了不影响
// 正确性，只影响延迟——这正是本地无公网地址时默认走轮询也能跑通的原因。
//
// 让回调直接解析报文推状态是另一种设计，代价是每个 driver 要维护两套解析
// （轮询响应一套、回调报文一套），而它们的差异只在包装层。提前轮询没有这个成本。
func (s *server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	driver, err := pathID(r, "driver")
	if err != nil {
		writeError(w, r, err)
		return
	}

	secret := os.Getenv(webhookSecretEnvPrefix + strings.ToUpper(driver))
	if secret == "" {
		// 没配密钥 = 这个 driver 的回调没开。回 401 而不是 404：
		// 404 会让上游以为地址写错了而重试到别处。
		writeError(w, r, errUnauthorized("该 driver 的回调未启用"))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		writeError(w, r, errInvalid("请求体读取失败"))
		return
	}
	if !validSignature(secret, body, r.Header.Get("X-Callback-Signature")) {
		writeError(w, r, errUnauthorized("签名校验失败"))
		return
	}

	// 上游的任务标识用 upstream_ref 传；对不上就静默受理——
	// 回调是尽力而为的加速通道，为一个认不出的报文回错误只会招来重试风暴。
	taskID := r.URL.Query().Get("task_id")
	if taskID != "" && s.deps.Poller != nil {
		if err := s.deps.Poller.Advance(r.Context(), taskID, ""); err != nil {
			slog.Warn("回调触发的提前轮询失败，等下一次定时轮询",
				"driver", driver, "task_id", taskID, "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// validSignature 比对 HMAC-SHA256 签名。
//
// 用 hmac.Equal 而不是 ==：字符串比较会在第一个不同的字节处返回，
// 泄漏出足以逐字节爆破签名的时间差。
func validSignature(secret string, body []byte, provided string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	got := strings.TrimPrefix(strings.TrimSpace(provided), "sha256=")
	return hmac.Equal([]byte(want), []byte(strings.ToLower(got)))
}
