package googlelro

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// RawStatus 把 done 布尔变成落进 tasks.upstream_status_raw 的字符串。
//
// 本协议没有状态串可存，但那一列对所有族都要有值——排障时"这条任务
// 上游到底跑完没有"必须能从库里看出来。用 "true"/"false" 而不是编一个
// 假的 "running"，是为了让看库的人一眼知道这是个布尔而不是状态枚举。
func RawStatus(done bool) string { return strconv.FormatBool(done) }

// Normalize 把 done 布尔的字符串形式归一化成平台状态。
//
// **它只回答"done 这个信号本身"的含义**：done=true 之后还要看 error 字段
// 才能分出成功与失败，那一步在 Poll 里做。把 error 判定也塞进这里会让
// 这张表的 key 变成 "true+error" 这种编出来的复合串，反而更难对拍。
func Normalize(rawDone string) adapter.Status {
	if s, ok := statusMap[strings.ToLower(strings.TrimSpace(rawDone))]; ok {
		return s
	}
	return adapter.StatusRunning
}

// operation 是提交与轮询共用的 LRO 响应体。
type operation struct {
	Name     string         `json:"name"`
	Done     bool           `json:"done"`
	Error    *statusError   `json:"error"`
	Response *lroResponse   `json:"response"`
	Metadata map[string]any `json:"metadata"`
}

// statusError 是 google.rpc.Status 的形状：code 是 gRPC 码（整数），
// message 是文案，details 里可能带 reason。
type statusError struct {
	Code    int              `json:"code"`
	Message string           `json:"message"`
	Details []map[string]any `json:"details"`
}

// lroResponse 承载产物。两种形态并存是因为它由 Veo 的输出配置决定：
// 配了 GCS 输出就给 gcsUri，没配就内联 base64。driver 说了不算，两种都要认。
type lroResponse struct {
	GenerateVideoResponse struct {
		GeneratedSamples []generatedSample `json:"generatedSamples"`
	} `json:"generateVideoResponse"`
	// 有的版本把样本直接放在顶层。
	GeneratedSamples []generatedSample `json:"generatedSamples"`
	Predictions      []prediction      `json:"predictions"`
}

type generatedSample struct {
	Video struct {
		URI            string `json:"uri"`
		BytesBase64    string `json:"bytesBase64Encoded"`
		EncodedVideo   string `json:"encodedVideo"`
		MIMEType       string `json:"mimeType"`
		DurationMillis int    `json:"durationMillis"`
	} `json:"video"`
}

// prediction 是 predictLongRunning 的另一种结果形状。
type prediction struct {
	GCSURI             string `json:"gcsUri"`
	BytesBase64Encoded string `json:"bytesBase64Encoded"`
	MIMEType           string `json:"mimeType"`
}

// DefaultArtifactMIME 是产物默认的内容类型，上游没给 mimeType 时用它。
const DefaultArtifactMIME = "video/mp4"

// artifactRefs 从成功的 operation 里抽出全部产物。
//
// 三种承载位置都要认（generateVideoResponse.generatedSamples、
// 顶层 generatedSamples、predictions），因为它们出自同一套 API 的不同版本，
// 而 base_url 指向哪个版本是 provider 配置说了算，driver 无从预判。
func artifactRefs(op operation) ([]adapter.ArtifactRef, error) {
	if op.Response == nil {
		return nil, adapter.CallError(domain.TaskErrorInternal, nil,
			"operation 已 done 且无 error，但 %s 字段为空", FieldResponse)
	}

	samples := op.Response.GenerateVideoResponse.GeneratedSamples
	if len(samples) == 0 {
		samples = op.Response.GeneratedSamples
	}

	refs := make([]adapter.ArtifactRef, 0, len(samples)+len(op.Response.Predictions))
	for _, s := range samples {
		ref := adapter.ArtifactRef{
			Type:  domain.AssetTypeVideo,
			MIME:  orDefault(s.Video.MIMEType, DefaultArtifactMIME),
			Index: len(refs),
		}
		b64 := orDefault(s.Video.BytesBase64, s.Video.EncodedVideo)
		switch {
		case b64 != "":
			ref.Kind = adapter.KindBase64
			ref.Base64 = b64
			ref.Bytes = int64(base64.StdEncoding.DecodedLen(len(b64)))
		case strings.HasPrefix(s.Video.URI, GCSURIScheme):
			ref.Kind = adapter.KindGCSURI
			ref.GCSURI = s.Video.URI
		case s.Video.URI != "":
			// 少数配置下上游直接给 https 直链。
			ref.Kind = adapter.KindURL
			ref.URL = s.Video.URI
		default:
			continue
		}
		if s.Video.DurationMillis > 0 {
			ms := s.Video.DurationMillis
			ref.DurationMS = &ms
		}
		refs = append(refs, ref)
	}

	for _, p := range op.Response.Predictions {
		ref := adapter.ArtifactRef{
			Type:  domain.AssetTypeVideo,
			MIME:  orDefault(p.MIMEType, DefaultArtifactMIME),
			Index: len(refs),
		}
		switch {
		case p.BytesBase64Encoded != "":
			ref.Kind = adapter.KindBase64
			ref.Base64 = p.BytesBase64Encoded
			ref.Bytes = int64(base64.StdEncoding.DecodedLen(len(p.BytesBase64Encoded)))
		case strings.HasPrefix(p.GCSURI, GCSURIScheme):
			ref.Kind = adapter.KindGCSURI
			ref.GCSURI = p.GCSURI
		default:
			continue
		}
		refs = append(refs, ref)
	}

	if len(refs) == 0 {
		return nil, adapter.CallError(domain.TaskErrorInternal, nil,
			"operation 已 done 且无 error，但 %s 里没有可用产物", FieldResponse)
	}
	return refs, nil
}

// gRPC 状态码到平台失败三分类的映射。
//
// 本协议不给错误码字符串，只给 google.rpc.Code 整数——这是唯一可靠的判据，
// 文案匹配在这里比另外两家更不可信（它会随语言环境变）。
var grpcCodeMap = map[int]domain.TaskErrorCode{
	3:  domain.TaskErrorInvalidParam,        // INVALID_ARGUMENT
	5:  domain.TaskErrorInvalidParam,        // NOT_FOUND（模型名写错）
	7:  domain.TaskErrorContentRejected,     // PERMISSION_DENIED，安全策略拦截走这个码
	8:  domain.TaskErrorUpstreamRateLimited, // RESOURCE_EXHAUSTED
	9:  domain.TaskErrorContentRejected,     // FAILED_PRECONDITION，责任 AI 过滤走这个码
	11: domain.TaskErrorInvalidParam,        // OUT_OF_RANGE
	13: domain.TaskErrorInternal,            // INTERNAL
	14: domain.TaskErrorInternal,            // UNAVAILABLE
	4:  domain.TaskErrorInternal,            // DEADLINE_EXCEEDED
}

func classify(status int, e statusError) domain.TaskErrorCode {
	// 文案关键词先看一眼：PERMISSION_DENIED 既可能是安全拦截也可能是
	// 凭证没权限，两者的处置完全不同（前者让用户改提示词，后者要运维改配置），
	// 只看 gRPC 码会把后者也报成"内容被拒"，让用户白改半天提示词。
	msg := strings.ToLower(e.Message)
	if containsAny(msg, "safety", "responsible ai", "blocked", "policy", "prohibited") {
		return domain.TaskErrorContentRejected
	}
	if containsAny(msg, "quota", "rate limit", "resource exhausted", "too many") {
		return domain.TaskErrorUpstreamRateLimited
	}
	if e.Code == 7 && containsAny(msg, "credential", "permission", "iam", "api key", "not authorized") {
		return domain.TaskErrorInternal
	}
	if code, ok := grpcCodeMap[e.Code]; ok {
		return code
	}
	if status != 0 {
		return adapter.ClassifyHTTPStatus(status)
	}
	return domain.TaskErrorInternal
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func upstreamError(status int, e statusError) *domain.Error {
	msg := e.Message
	if msg == "" {
		msg = "上游未给出错误描述"
	}
	return adapter.CallError(classify(status, e), nil, "%s", msg)
}

// failureFor 把 done=true 且带 error 的 operation 变成归一化后的 domain.TaskError。
func failureFor(e statusError) *domain.TaskError {
	err := upstreamError(0, e)
	return adapter.Failure(domain.TaskErrorCode(err.Code), "%s", err.Message)
}

// reclassify 在 httpx 只按状态码分过一次类之后，再按本协议的文案纠正。
func reclassify(err error) error {
	var de *domain.Error
	if !errors.As(err, &de) {
		return err
	}
	msg := strings.ToLower(de.Message)
	switch {
	case containsAny(msg, "safety", "responsible ai", "blocked", "prohibited"):
		return adapter.CallError(domain.TaskErrorContentRejected, de.Err, "%s", de.Message)
	case containsAny(msg, "resource_exhausted", "quota", "rate limit"):
		return adapter.CallError(domain.TaskErrorUpstreamRateLimited, de.Err, "%s", de.Message)
	}
	return err
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// escapePathSegment 转义 URL 路径中的一段。
// GCS 的 object 名里可以有斜杠，必须整体编码成 %2F 才不会被当成路径分隔。
func escapePathSegment(s string) string { return url.QueryEscape(s) }
