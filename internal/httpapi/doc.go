// Package httpapi wires the HTTP surface described by docs/openapi.yaml.
//
// 路由用标准库的 net/http.ServeMux（Go 1.22+ 支持 "METHOD /path/{param}" 的
// 模式匹配）。不引第三方路由库：本项目的路由规模用标准库完全够用，
// 而多一个框架依赖就多一套中间件约定和一份升级负担。
//
// # 四个路由组
//
//	/api/*            用户端。除 auth 外全部要求有效 Bearer token。
//	/api/admin/*      管理端。在用户端要求之上再要求 role=admin，
//	                  越权返回 403 forbidden。**角色隔离靠路由前缀而不是
//	                  逐个 handler 里判断** —— 前缀级的中间件不会因为某个新
//	                  handler 忘了写检查而漏掉，逐 handler 判断迟早会漏一个。
//	/api/stream       SSE 长连接。一条连接推该用户全部任务的事件。
//	                  它不能套响应缓冲类中间件（gzip、buffered writer），
//	                  否则事件会成批到达，实时性归零；因此单列一组。
//	/api/webhooks/*   上游回调入口。**不走 Bearer 鉴权**（上游不可能持有用户
//	                  的 JWT），改用每 provider 一个共享密钥的签名头校验，
//	                  密钥从 env 读。因此它也必须单列，不能挂在 /api/* 下
//	                  被用户鉴权中间件拦掉。
//
// # 中间件链（自外向内）
//
//	RequestID    生成/透传请求 id，贯穿日志便于串起一次请求
//	Recover      panic 兜底，转成 500 internal_error，绝不让连接直接断
//	AccessLog    访问日志。记 request id、用户 id、路径、状态码、耗时
//	CORS         本地开发前后端分离，需要放行前端来源
//	Auth         解析 Bearer token，把用户放进 context；失败返回 401 unauthorized
//	AdminOnly    仅 /api/admin/* 组挂载；role != admin 返回 403 forbidden
//	RateLimit    按用户限流，返回 429 rate_limited
//
// # 统一错误出口
//
// 所有错误响应一律是 domain.ErrorEnvelope，前端严格按 error.code 分支，
// 不解析 message。handler 返回 *domain.Error，由统一的写出函数转成
// HTTP 状态码 + envelope —— 每个 handler 自己拼 JSON 迟早会拼出几种不同的形状，
// 而前端只认一种。
//
// # 一条纪律
//
// 本包里**不允许出现任何 `if provider == ...` / `switch vendor`**。
// 协议差异只存在于 internal/adapter 的各 driver 子包内（见那里的红线）。
// handler 只认 domain 与各层接口。
package httpapi
