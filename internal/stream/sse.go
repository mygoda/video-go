package stream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// WriteHeaders 写入 SSE 必需的响应头。
//
// 三条缺一不可，任何一条漏掉实时性直接归零:
//
//	Content-Type: text/event-stream  浏览器据此按 SSE 解析
//	Cache-Control: no-cache          否则中间缓存会把整条流当成一次响应缓存住
//	X-Accel-Buffering: no            nginx 默认对 proxy 响应做缓冲，
//	                                 事件会攒够一个 buffer 才吐出去——
//	                                 表现为进度条不动然后突然跳到完成
//
// 另外**绝不能开 gzip**：压缩流有自己的缓冲窗口，效果与代理缓冲一样。
// 因此这里显式声明 identity，挡掉上游中间件可能加上的压缩。
func WriteHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	h.Set("Content-Encoding", "identity")
}

// ParseLastEventID 解析客户端回传的 Last-Event-ID。
//
// 优先读请求头。**不接受 query 参数形态的 token 与 id**：token 进 URL 会进
// 访问日志和 Referer，而既然认证走 Bearer 头，续传游标也走头才一致。
//
// 无法解析时返回 0（从头只收新事件）而不是报错：一个畸形的 Last-Event-ID
// 不该让客户端连不上，它顶多是少补几条，而全量对账那条路还在。
func ParseLastEventID(r *http.Request) int64 {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		return 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}

// Encode 把一条事件写成 SSE 的线格式。
//
//	event: <type>
//	id: <id>
//	data: <json>
//	<空行>
//
// data 里的换行必须拆成多条 data: 行——SSE 用空行分隔消息，负载里一个裸的
// "\n\n" 会被解析成消息结束，后半段变成下一条消息的开头。JSON 编码器默认
// 会转义字符串里的换行，但 Data 是 any，谁都可能塞进来一个已经序列化好的
// 带缩进的串，所以这里不假设。
//
// ping 不带 id：它是保活而不是业务事件，占用 Last-Event-ID 的序列会让
// 客户端记住一个补发时查不到的游标。
func Encode(w io.Writer, ev Event) error {
	payload, err := json.Marshal(ev.Data)
	if err != nil {
		return fmt.Errorf("stream: 序列化事件负载: %w", err)
	}

	var b bytes.Buffer
	b.WriteString("event: ")
	b.WriteString(string(ev.Type))
	b.WriteByte('\n')

	if ev.Type != EventPing && ev.ID > 0 {
		b.WriteString("id: ")
		b.WriteString(strconv.FormatInt(ev.ID, 10))
		b.WriteByte('\n')
	}

	for line := range strings.SplitSeq(string(payload), "\n") {
		b.WriteString("data: ")
		b.WriteString(strings.TrimSuffix(line, "\r"))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')

	if _, err := w.Write(b.Bytes()); err != nil {
		return fmt.Errorf("stream: 写出事件: %w", err)
	}
	return nil
}
