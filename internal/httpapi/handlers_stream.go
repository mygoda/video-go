package httpapi

import (
	"net/http"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/stream"
)

// handleStream 是 SSE 长连接。
//
// **一条连接推该用户的全部任务事件**，不是每个任务一条。浏览器对同一域名的
// 并发连接数有上限（HTTP/1.1 下 6 条），按任务开连接的话用户同时跑七个任务
// 就把整个站点的其他请求全堵死了。
//
// 前端用 fetch + ReadableStream 而不是 EventSource：EventSource 带不上
// Authorization 头，只能把 token 塞 query，而 token 不能进 query（见 bearerToken）。
//
// 服务端的 WriteTimeout 必须是 0（见 config.Config），否则连接会在写超时后
// 被无声掐断，表现为"跑着跑着就没进度了"。
func (s *server) handleStream(w http.ResponseWriter, r *http.Request) {
	id, err := identity(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, errInternal(nil))
		return
	}

	ctx := r.Context()
	events, err := s.deps.Stream.Subscribe(ctx, id.UserID, stream.ParseLastEventID(r))
	if err != nil {
		writeError(w, r, err)
		return
	}

	stream.WriteHeaders(w)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// 心跳由本层发而不是 Hub 发：它要的是"这条 TCP 连接还活着"，
	// 而 Hub 不知道自己的订阅者是 SSE 还是别的什么。
	ping := time.NewTicker(stream.PingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-events:
			if !open {
				return
			}
			if err := stream.Encode(w, ev); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
