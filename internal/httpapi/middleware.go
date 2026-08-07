package httpapi

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// middleware 是标准的 handler 包装器。
type middleware func(http.Handler) http.Handler

// chain 按声明顺序套上中间件：chain(h, a, b) 的执行顺序是 a → b → h。
func chain(h http.Handler, ms ...middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}

// recoverer 把 panic 转成 500，而不是让整个进程随一个 handler 一起死。
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("panic in handler",
					"method", r.Method, "path", r.URL.Path, "panic", v,
					"stack", string(debug.Stack()))
				writeError(w, r, errInternal(nil))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requestLogger 记录一行访问日志。
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info("http",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "ms", time.Since(start).Milliseconds())
	})
}

// statusRecorder 记下实际写出的状态码。
//
// 它必须转发 Flush：SSE 靠 Flush 把每个事件立刻推出去，
// 包装层吞掉这个接口，事件就会攒在缓冲里直到连接关闭。
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status = code
		s.written = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// cors 放行配置里列出的来源。
//
// 前后端分离开发时 Vite 跑在另一个端口，不放行就一个接口也调不通。
// 允许携带凭证时不能回 "*"，必须回具体来源——浏览器会拒。
func cors(origins []string) middleware {
	allowAll := len(origins) == 0
	allowed := map[string]bool{}
	for _, o := range origins {
		if o == "*" {
			allowAll = true
		}
		allowed[o] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && (allowAll || allowed[origin]) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, If-None-Match, Last-Event-ID")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
				// ETag 默认不暴露给跨域脚本，不暴露前端就拿不到、也就发不出
				// If-None-Match，/api/models 的 304 协商直接失效。
				w.Header().Set("Access-Control-Expose-Headers", "ETag, Last-Event-ID")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// auth 校验 Bearer token 并把身份放进 context。
//
// 顺带更新 last_active_at，但**失败不影响请求**：活跃时间是运营指标，
// 让它把一个正常请求打成 500 是本末倒置。
func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeError(w, r, errUnauthorized("缺少 Authorization: Bearer <token>"))
			return
		}
		userID, role, err := s.deps.Tokens.Verify(token)
		if err != nil {
			writeError(w, r, errUnauthorized("令牌无效或已过期"))
			return
		}

		// 账号被停用后，手上那张还没过期的 token 必须立刻失效。
		u, err := s.deps.Users.GetByID(r.Context(), userID)
		if err != nil {
			writeError(w, r, errUnauthorized("令牌对应的账号不存在"))
			return
		}
		if u.Status == domain.UserStatusDisabled {
			writeError(w, r, errForbidden("账号已被停用"))
			return
		}
		// 以库里的角色为准：token 里的角色是签发那一刻的快照，
		// 管理员被降权后不该靠一张旧 token 继续访问 admin 路由。
		role = u.Role

		go func() {
			// 用后台 context：请求结束时不该把这次写打断。
			_ = s.deps.Users.TouchLastActive(detachedContext(), userID, s.now())
		}()

		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), Identity{UserID: userID, Role: role})))
	})
}

// adminOnly 挡在 /api/admin/* 前面。
func adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := IdentityFrom(r.Context())
		if !ok {
			writeError(w, r, errUnauthorized("未认证"))
			return
		}
		if !id.IsAdmin() {
			writeError(w, r, errForbidden("需要管理员权限"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// noStore 给不该被缓存的响应统一加头。
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/assets/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
