package httpapi

import (
	"context"
	"net/http"
	"time"
)

// server 持有依赖，所有 handler 都是它的方法。
//
// 用一个结构体而不是把 Deps 传进每个 handler，是为了让 handler 之间能共享
// 少量派生状态（如 ffmpeg 是否存在的探测结果），也让路由表读起来是一串方法名。
type server struct {
	deps Deps
}

// Register 把全部路由挂到 mux 上。
//
// 四个路由组的中间件不同，这是分组的全部理由：
//   - /api/*            认证
//   - /api/admin/*      认证 + 管理员
//   - /api/stream       认证，但**不能**套任何缓冲/超时中间件
//   - /api/webhooks/*   不认证（上游回调），靠签名与不可猜的路径自证
func Register(mux *http.ServeMux, deps Deps) {
	s := &server{deps: deps}
	s.routes(mux)
}

// now 取当前时间。Clock 未注入时退回真实时钟，省得每个调用点都判空。
func (s *server) now() time.Time {
	if s.deps.Clock != nil {
		return s.deps.Clock.Now()
	}
	return time.Now().UTC()
}

// detachedContext 给"请求已结束但仍要写完"的后台动作用，
// 带自己的超时，不会随请求取消。
func detachedContext() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	// 交给超时回收，调用方不必持有 cancel。
	_ = cancel
	return ctx
}
