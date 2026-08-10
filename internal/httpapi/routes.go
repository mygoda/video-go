package httpapi

import "net/http"

// routes 是全部路由的唯一登记处。
//
// 分组只按中间件差异切（见 doc.go）：同一组里的端点共享一条中间件链，
// 新加一个 admin 端点只要挂进 admin 组就自动带上角色检查，
// 不依赖 handler 作者记得写。
func (s *server) routes(mux *http.ServeMux) {
	base := []middleware{
		recoverer,
		requestLogger,
		cors(s.deps.Config.CORSOrigins),
		noStore,
	}

	// open：无需认证。注册/登录，以及上游 webhook 回调。
	open := func(h http.HandlerFunc) http.Handler {
		return chain(h, base...)
	}
	// user：需要有效 Bearer token。
	user := func(h http.HandlerFunc) http.Handler {
		return chain(h, append(append([]middleware{}, base...), s.auth)...)
	}
	// admin：在 user 之上再要求 role=admin。
	admin := func(h http.HandlerFunc) http.Handler {
		return chain(h, append(append([]middleware{}, base...), s.auth, adminOnly)...)
	}

	// ── auth ────────────────────────────────────────────────────
	mux.Handle("POST /api/auth/register", open(s.handleRegister))
	mux.Handle("POST /api/auth/login", open(s.handleLogin))

	// ── me ──────────────────────────────────────────────────────
	mux.Handle("GET /api/me", user(s.handleGetMe))
	mux.Handle("POST /api/me/password", user(s.handleChangePassword))
	mux.Handle("GET /api/me/credit-ledger", user(s.handleMyCreditLedger))

	// ── L1 能力声明 ─────────────────────────────────────────────
	mux.Handle("GET /api/models", user(s.handleListModels))
	mux.Handle("GET /api/skills", user(s.handleListSkills))

	// ── 上传与任务 ──────────────────────────────────────────────
	mux.Handle("POST /api/uploads", user(s.handleCreateUpload))
	// openapi.yaml 之外的增补：Upload.preview_url 必须指向某处，而 upload
	// 还不是 asset，走不了 /api/assets/{id}/content。与资产内容同理走 open，
	// <img src> 带不上 Authorization 头。
	mux.Handle("GET /api/uploads/{uploadId}/content", open(s.handleUploadContent))
	mux.Handle("POST /api/tasks", user(s.handleCreateTask))
	mux.Handle("GET /api/tasks", user(s.handleListTasks))
	mux.Handle("GET /api/tasks/{taskId}", user(s.handleGetTask))
	mux.Handle("POST /api/tasks/{taskId}/cancel", user(s.handleCancelTask))
	// DELETE 是「从我的任务流里移掉」，不是删记录——见 handleDismissTask。
	mux.Handle("DELETE /api/tasks/{taskId}", user(s.handleDismissTask))

	// ── 资产 ────────────────────────────────────────────────────
	mux.Handle("GET /api/assets", user(s.handleListAssets))
	mux.Handle("GET /api/assets/{assetId}", user(s.handleGetAsset))
	mux.Handle("DELETE /api/assets/{assetId}", user(s.handleDeleteAsset))
	mux.Handle("GET /api/assets/{assetId}/lineage", user(s.handleAssetLineage))
	// content 走 open：<img src> / <video src> 带不上 Authorization 头。
	// 资产 id 是不可猜的 UUID，且响应带 private 缓存控制。
	mux.Handle("GET /api/assets/{assetId}/content", open(s.handleAssetContent))

	// ── 画布 ────────────────────────────────────────────────────
	mux.Handle("GET /api/projects", user(s.handleListProjects))
	mux.Handle("POST /api/projects", user(s.handleCreateProject))
	mux.Handle("GET /api/projects/{projectId}", user(s.handleGetProject))
	mux.Handle("PATCH /api/projects/{projectId}", user(s.handleUpdateProject))
	mux.Handle("DELETE /api/projects/{projectId}", user(s.handleDeleteProject))
	mux.Handle("GET /api/projects/{projectId}/canvas", user(s.handleGetCanvas))
	mux.Handle("PATCH /api/projects/{projectId}/canvas", user(s.handlePatchCanvas))
	mux.Handle("POST /api/canvas/{projectId}/chat", user(s.handleCanvasChat))
	mux.Handle("POST /api/canvas/{projectId}/storyboard", user(s.handleCanvasStoryboard))
	mux.Handle("POST /api/canvas/{projectId}/cards/{cardId}/refine", user(s.handleRefineScriptCard))
	mux.Handle("POST /api/canvas/{projectId}/compose", user(s.handleCanvasCompose))

	// ── SSE ─────────────────────────────────────────────────────
	// **刻意不套 requestLogger**：它到连接关闭才落一行日志，对长连接毫无意义，
	// 而任何包装 ResponseWriter 的中间件都是缓冲风险的来源。
	mux.Handle("GET /api/stream", chain(http.HandlerFunc(s.handleStream),
		recoverer, cors(s.deps.Config.CORSOrigins), s.auth))

	// ── webhook（不认证，靠签名）─────────────────────────────────
	mux.Handle("POST /api/webhooks/tasks/{driver}", open(s.handleWebhook))

	// ── admin ───────────────────────────────────────────────────
	mux.Handle("GET /api/admin/providers", admin(s.handleAdminListProviders))
	mux.Handle("POST /api/admin/providers", admin(s.handleAdminCreateProvider))
	mux.Handle("GET /api/admin/providers/{providerId}", admin(s.handleAdminGetProvider))
	mux.Handle("PATCH /api/admin/providers/{providerId}", admin(s.handleAdminUpdateProvider))
	mux.Handle("DELETE /api/admin/providers/{providerId}", admin(s.handleAdminDeleteProvider))

	mux.Handle("GET /api/admin/models", admin(s.handleAdminListModels))
	mux.Handle("POST /api/admin/models", admin(s.handleAdminCreateModel))
	mux.Handle("GET /api/admin/models/{modelId}", admin(s.handleAdminGetModel))
	mux.Handle("PATCH /api/admin/models/{modelId}", admin(s.handleAdminUpdateModel))
	mux.Handle("DELETE /api/admin/models/{modelId}", admin(s.handleAdminDeleteModel))
	mux.Handle("POST /api/admin/models/{modelId}/probe", admin(s.handleAdminProbeModel))

	mux.Handle("GET /api/admin/users", admin(s.handleAdminListUsers))
	mux.Handle("GET /api/admin/users/{userId}", admin(s.handleAdminGetUser))
	mux.Handle("PATCH /api/admin/users/{userId}", admin(s.handleAdminUpdateUser))
	mux.Handle("POST /api/admin/users/{userId}/credits", admin(s.handleAdminTopup))
	mux.Handle("GET /api/admin/users/{userId}/credit-ledger", admin(s.handleAdminUserLedger))

	mux.Handle("GET /api/admin/tasks", admin(s.handleAdminListTasks))
	mux.Handle("GET /api/admin/tasks/stats", admin(s.handleAdminTaskStats))
	mux.Handle("POST /api/admin/tasks/{taskId}/retry", admin(s.handleAdminRetryTask))
	mux.Handle("POST /api/admin/tasks/{taskId}/cancel", admin(s.handleAdminCancelTask))

	mux.Handle("GET /api/admin/circuit-breakers", admin(s.handleAdminBreakers))
	mux.Handle("GET /api/admin/storage/usage", admin(s.handleAdminStorageUsage))
	mux.Handle("POST /api/admin/storage/cleanup", admin(s.handleAdminStorageCleanup))

	mux.Handle("GET /api/admin/skills", admin(s.handleAdminListSkills))
	mux.Handle("POST /api/admin/skills", admin(s.handleAdminCreateSkill))
	mux.Handle("PATCH /api/admin/skills/{skillId}", admin(s.handleAdminUpdateSkill))
	mux.Handle("DELETE /api/admin/skills/{skillId}", admin(s.handleAdminDeleteSkill))

	// 健康检查。不在 openapi.yaml 里——它是给编排器看的，不是给前端调的。
	mux.Handle("GET /healthz", open(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))
}
