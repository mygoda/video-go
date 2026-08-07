// Package httpx holds the HTTP plumbing shared by the real upstream drivers.
//
// 它只承载**所有 HTTP 协议驱动都一样**的那部分：拼 URL、放鉴权头、
// 编解 JSON、把非 2xx 变成 *domain.Error。任何一家独有的东西——端点路径、
// 状态枚举、产物取法——都不在这里，它们是各 driver 包存在的理由。
//
// 拆出来是因为四个驱动里这段代码逐字相同，复制四份意味着以后修一个
// 超时处理的 bug 要在四个地方各修一遍，而漏掉一个不会有任何编译错误。
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// MaxErrorBodyBytes 是读取错误响应体的上限。
// 不设上限的话，一个上游返回几百 MB HTML 错误页就能把内存吃光。
const MaxErrorBodyBytes = 64 << 10

// Client 返回可用的 http.Client：调用方没注入就用 http.DefaultClient。
// 超时由 ctx 承载（Provider.TimeoutMS 决定），不在这里设 Client.Timeout——
// 后者会把流式取产物一起掐断，而一段视频下载几分钟是正常的。
func Client(c *http.Client) *http.Client {
	if c == nil {
		return http.DefaultClient
	}
	return c
}

// URL 把 base 与相对路径拼成完整地址，容忍 base 末尾有无斜杠。
func URL(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

// Auth 按 Provider 声明的方式把凭证放进请求。
//
// 凭证只从这里进入请求，且**绝不进日志**：*http.Request 不会被整体打印，
// 各 driver 打日志时只打 method / url / status。
func Auth(req *http.Request, p domain.Provider, credential string) {
	if credential == "" {
		return
	}
	switch p.AuthStyle {
	case domain.AuthStyleHeader:
		name := p.AuthHeaderName
		if name == "" {
			name = "Authorization"
		}
		req.Header.Set(name, credential)
	case domain.AuthStyleQuery:
		name := p.AuthHeaderName
		if name == "" {
			name = "key"
		}
		q := req.URL.Query()
		q.Set(name, credential)
		req.URL.RawQuery = q.Encode()
	default:
		// AuthStyleBearer 是缺省：Ark 与 OpenAI 都是这一种。
		req.Header.Set("Authorization", "Bearer "+credential)
	}
}

// PostJSON 发一个 JSON 请求并把响应解进 out。
//
// out 为 nil 时丢弃响应体（但仍然读空并关闭，否则连接不能复用）。
func PostJSON(ctx context.Context, c *http.Client, url string, p domain.Provider, credential string, headers map[string]string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return adapter.CallError(domain.TaskErrorInternal, err, "请求体序列化失败")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return adapter.CallError(domain.TaskErrorInternal, err, "构造请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	Auth(req, p, credential)
	return do(c, req, out)
}

// GetJSON 发一个 GET 并把响应解进 out。
func GetJSON(ctx context.Context, c *http.Client, url string, p domain.Provider, credential string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return adapter.CallError(domain.TaskErrorInternal, err, "构造请求失败")
	}
	req.Header.Set("Accept", "application/json")
	Auth(req, p, credential)
	return do(c, req, out)
}

// GetStream 发一个 GET 并把响应体**原样**交出去，由调用方关闭。
//
// 它是产物下载路径：绝不 ReadAll。一段视频几十上百 MB，
// 并发几个就能把进程撑爆（见 adapter.ArtifactStream 的注释）。
func GetStream(ctx context.Context, c *http.Client, url string, p domain.Provider, credential string, accept string) (adapter.ArtifactStream, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return adapter.ArtifactStream{}, adapter.CallError(domain.TaskErrorInternal, err, "构造请求失败")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	Auth(req, p, credential)

	resp, err := Client(c).Do(req)
	if err != nil {
		return adapter.ArtifactStream{}, adapter.CallError(domain.TaskErrorInternal, err, "上游请求失败")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
		return adapter.ArtifactStream{}, StatusError(resp.StatusCode, snippet)
	}

	mime := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	// ContentLength 为 -1 表示 chunked，此时长度未知，按契约填 0。
	size := resp.ContentLength
	if size < 0 {
		size = 0
	}
	return adapter.ArtifactStream{Body: resp.Body, MIME: mime, Bytes: size}, nil
}

func do(c *http.Client, req *http.Request, out any) error {
	resp, err := Client(c).Do(req)
	if err != nil {
		// 连不上 / 超时 / TLS 失败一律 internal_error 且可重试:
		// 它说的是"这次调用没打通"，不是"上游拒绝了这个请求"。
		return adapter.CallError(domain.TaskErrorInternal, err, "上游请求失败")
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
		return StatusError(resp.StatusCode, snippet)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxErrorBodyBytes))
		return nil
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return adapter.CallError(domain.TaskErrorInternal, err, "读取上游响应失败")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return adapter.CallError(domain.TaskErrorInternal, err, "上游响应不是预期的 JSON")
	}
	return nil
}

// StatusError 把一个非 2xx 响应变成归一化后的 *domain.Error。
//
// 它只看状态码，是**兜底**：各 driver 应先用自己认识的上游错误码分类
// （审核拒绝在 OpenAI 与 Ark 那里都是 400，只看状态码会误判成参数错误），
// 认不出来才落到这里。
func StatusError(status int, body []byte) *domain.Error {
	code := adapter.ClassifyHTTPStatus(status)
	return adapter.CallError(code, nil, "上游返回 %d: %s", status, Snippet(body))
}

// Snippet 把响应体裁成适合进日志与错误消息的一小段。
// 上游错误体里不含凭证（凭证在 header 里），因此可以安全回显。
func Snippet(body []byte) string {
	const limit = 512
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "(空响应体)"
	}
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}

// RequireCredential 在凭证为空时给出一个可读的失败，而不是让上游回 401
// 再去猜。它把"没配 env"这个部署问题在第一时间指出来。
//
// credentialRef 是**环境变量名**（Provider.CredentialRef），把它写进错误消息
// 是安全的；密钥本身永远不进错误、不进日志、不进代码。
func RequireCredential(credential, credentialRef string) error {
	if credential != "" {
		return nil
	}
	return adapter.CallError(domain.TaskErrorInternal, nil,
		"供应商凭证为空：环境变量 %s 未配置", credentialRef)
}
