// Command smoke walks the whole product chain against a running aigcd and
// fails loudly if any link is broken.
//
// 它存在的理由是验收线本身：「注册登录 → 拿模型列表与 capability schema →
// 提交任务 → SSE 看到状态流转 → 转存 → 资产入库 → 扣积分」这条链子，
// 任何一环单独测都测不出接缝上的问题。写成 Go 而不是 curl 脚本，
// 是因为 SSE 要真的当流读、边读边断言，curl 做不到。
//
// 用法：先 `make run`，再 `go run ./scripts/smoke`。
// 目标地址取 AIGC_SMOKE_BASE_URL，默认 http://localhost:8080。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n\033[31mSMOKE FAILED:\033[0m %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\n\033[32mSMOKE PASSED\033[0m")
}

var baseURL = envOr("AIGC_SMOKE_BASE_URL", "http://localhost:8080")

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	adminUser := envOr("AIGC_SEED_ADMIN_USERNAME", "admin")
	adminPass := envOr("AIGC_SEED_ADMIN_PASSWORD", "admin-dev-only")

	step("健康检查")
	var health map[string]any
	if err := call(ctx, "", http.MethodGet, "/healthz", nil, &health); err != nil {
		return fmt.Errorf("服务没起来（先 make run）：%w", err)
	}
	ok("healthz = %v", health["status"])

	// ── 注册 / 登录 ────────────────────────────────────────────────
	step("注册新用户并登录")
	username := fmt.Sprintf("smoke_%d", time.Now().UnixNano())
	password := "smoke-dev-only-password"

	var auth authResponse
	if err := call(ctx, "", http.MethodPost, "/api/auth/register",
		map[string]any{"username": username, "password": password}, &auth); err != nil {
		return fmt.Errorf("注册: %w", err)
	}
	if auth.Token == "" || auth.User.ID == "" {
		return fmt.Errorf("注册返回里没有 token 或 user.id: %+v", auth)
	}
	ok("注册成功 user=%s id=%s", auth.User.Username, auth.User.ID)

	var login authResponse
	if err := call(ctx, "", http.MethodPost, "/api/auth/login",
		map[string]any{"username": username, "password": password}, &login); err != nil {
		return fmt.Errorf("登录: %w", err)
	}
	ok("登录成功，拿到 token")
	token := login.Token

	// 错误口令必须 401，否则鉴权形同虚设。
	if err := call(ctx, "", http.MethodPost, "/api/auth/login",
		map[string]any{"username": username, "password": password + "x"}, nil); err == nil {
		return fmt.Errorf("错误口令居然登录成功了")
	}
	ok("错误口令被拒")

	// ── 权限隔离 ───────────────────────────────────────────────────
	step("admin / 普通用户隔离")
	if err := call(ctx, token, http.MethodGet, "/api/admin/users", nil, nil); err == nil {
		return fmt.Errorf("普通用户访问 /api/admin/users 居然通过了")
	}
	ok("普通用户访问 admin 路由被拒")

	var adminAuth authResponse
	if err := call(ctx, "", http.MethodPost, "/api/auth/login",
		map[string]any{"username": adminUser, "password": adminPass}, &adminAuth); err != nil {
		return fmt.Errorf("管理员登录失败（先 make seed）: %w", err)
	}
	adminToken := adminAuth.Token
	if err := call(ctx, adminToken, http.MethodGet, "/api/admin/users", nil, nil); err != nil {
		return fmt.Errorf("管理员访问 /api/admin/users: %w", err)
	}
	ok("管理员可访问 admin 路由")

	// ── 充值（顺带验管理接口）──────────────────────────────────────
	step("管理员给新用户充积分")
	if err := call(ctx, adminToken, http.MethodPost,
		"/api/admin/users/"+auth.User.ID+"/credits",
		map[string]any{"amount": 50000, "reason": "smoke test", "idempotency_key": username + "-topup"},
		nil); err != nil {
		return fmt.Errorf("充值: %w", err)
	}
	var me meResponse
	if err := call(ctx, token, http.MethodGet, "/api/me", nil, &me); err != nil {
		return fmt.Errorf("GET /api/me: %w", err)
	}
	if me.Credits < 50000 {
		return fmt.Errorf("充值后余额只有 %d，期望 >= 50000", me.Credits)
	}
	ok("余额 = %d", me.Credits)

	// ── 模型列表 + capability schema ───────────────────────────────
	step("拿模型列表与 capability schema")
	models, etag, err := fetchModels(ctx, token, "")
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return fmt.Errorf("模型列表是空的（先 make migrate-up）")
	}
	for _, m := range models {
		if m.ID == "" || m.Pricing == nil || m.Params == nil {
			return fmt.Errorf("模型 %q 的 capability schema 不完整", m.ID)
		}
	}
	ok("%d 个模型，ETag=%s", len(models), etag)

	if etag != "" {
		status, err := conditionalGet(ctx, token, "/api/models", etag)
		if err != nil {
			return err
		}
		if status != http.StatusNotModified {
			return fmt.Errorf("带 If-None-Match 请求 /api/models 返回 %d，期望 304", status)
		}
		ok("If-None-Match 命中 304")
	}

	imageModel := pickModel(models, "image")
	videoModel := pickModel(models, "video")
	if imageModel == nil {
		return fmt.Errorf("没有可用的 image 模型")
	}

	// ── SSE ────────────────────────────────────────────────────────
	step("接上 SSE 通道")
	sseCtx, sseCancel := context.WithCancel(ctx)
	defer sseCancel()
	sse, err := openSSE(sseCtx, token)
	if err != nil {
		return fmt.Errorf("连 SSE: %w", err)
	}
	ok("SSE 已连上")

	// ── 图片任务全链路 ─────────────────────────────────────────────
	step("提交图片任务，走完整链路")
	imgTask, err := runTask(ctx, token, sse, imageModel, "一只在霓虹灯下的猫，赛博朋克")
	if err != nil {
		return fmt.Errorf("图片任务: %w", err)
	}
	ok("图片任务 %s 成功，实扣 %d 积分，产物 %d 件", imgTask.ID, deref(imgTask.ActualCost), len(imgTask.Assets))

	if err := checkAssets(ctx, token, imgTask); err != nil {
		return fmt.Errorf("图片产物: %w", err)
	}

	// ── 积分确实扣了 ───────────────────────────────────────────────
	step("核对积分扣减")
	before := me.Credits
	var after meResponse
	if err := call(ctx, token, http.MethodGet, "/api/me", nil, &after); err != nil {
		return err
	}
	spent := before - after.Credits
	if spent <= 0 {
		return fmt.Errorf("任务成功后余额没变（%d → %d）", before, after.Credits)
	}
	ok("余额 %d → %d，扣了 %d", before, after.Credits, spent)

	var ledger ledgerPage
	if err := call(ctx, token, http.MethodGet, "/api/me/credit-ledger", nil, &ledger); err != nil {
		return err
	}
	if !hasLedgerType(ledger, "charge") {
		return fmt.Errorf("流水里没有 charge 记录：%+v", ledger.Items)
	}
	ok("积分流水里有 hold/charge 记录，共 %d 条", len(ledger.Items))

	// ── 幂等 ───────────────────────────────────────────────────────
	step("幂等提交")
	tok := "smoke-idem-" + username
	a, err := submitTask(ctx, token, imageModel, "幂等测试", tok)
	if err != nil {
		return err
	}
	b, err := submitTask(ctx, token, imageModel, "幂等测试", tok)
	if err != nil {
		return err
	}
	if a.TaskID != b.TaskID {
		return fmt.Errorf("同一 client_token 提交两次拿到两个任务：%s != %s", a.TaskID, b.TaskID)
	}
	ok("同一 client_token 复用了任务 %s", a.TaskID)

	// ── 视频任务（走轮询）──────────────────────────────────────────
	if videoModel != nil {
		step("提交视频任务，走轮询链路")
		vidTask, err := runTask(ctx, token, sse, videoModel, "海浪拍打礁石的慢镜头")
		if err != nil {
			return fmt.Errorf("视频任务: %w", err)
		}
		ok("视频任务 %s 成功，实扣 %d 积分，产物 %d 件", vidTask.ID, deref(vidTask.ActualCost), len(vidTask.Assets))
		if err := checkAssets(ctx, token, vidTask); err != nil {
			return fmt.Errorf("视频产物: %w", err)
		}
	} else {
		fmt.Println("  ! 没有 video 模型，跳过视频链路")
	}

	// ── 活跃任务对账 ───────────────────────────────────────────────
	step("SSE 重连对账接口")
	var active taskPage
	if err := call(ctx, token, http.MethodGet, "/api/tasks?status=active", nil, &active); err != nil {
		return err
	}
	if active.NextCursor != "" {
		return fmt.Errorf("status=active 不该分页，却返回了 next_cursor")
	}
	ok("status=active 返回 %d 条且不分页", len(active.Items))

	// ── 资产列表与血缘 ─────────────────────────────────────────────
	step("资产列表与血缘")
	var assets assetPage
	if err := call(ctx, token, http.MethodGet, "/api/assets?limit=10", nil, &assets); err != nil {
		return err
	}
	if len(assets.Items) == 0 {
		return fmt.Errorf("资产列表是空的，但任务已经成功了")
	}
	ok("资产列表 %d 件", len(assets.Items))

	// ── SSE 到底收到了什么 ─────────────────────────────────────────
	step("SSE 事件核对")
	seen := sse.Seen()
	if len(seen) == 0 {
		return fmt.Errorf("整个过程 SSE 一个事件都没收到")
	}
	ok("SSE 收到 %d 个事件：%s", sse.Count(), strings.Join(seen, ", "))

	// ── skill / 画布 ───────────────────────────────────────────────
	step("Skill 与画布持久化")
	var skills skillList
	if err := call(ctx, token, http.MethodGet, "/api/skills", nil, &skills); err != nil {
		return err
	}
	ok("%d 个 skill", len(skills.Skills))

	var project projectResp
	if err := call(ctx, token, http.MethodPost, "/api/projects",
		map[string]any{"name": "smoke project"}, &project); err != nil {
		return fmt.Errorf("建画布项目: %w", err)
	}
	var snapshot canvasSnapshot
	if err := call(ctx, token, http.MethodGet, "/api/projects/"+project.ID+"/canvas", nil, &snapshot); err != nil {
		return fmt.Errorf("取画布快照: %w", err)
	}
	ok("画布项目 %s revision=%d", project.ID, snapshot.Revision)

	// ── 管理端监控 ─────────────────────────────────────────────────
	step("管理端监控接口")
	var stats map[string]any
	if err := call(ctx, adminToken, http.MethodGet, "/api/admin/tasks/stats", nil, &stats); err != nil {
		return err
	}
	var usage map[string]any
	if err := call(ctx, adminToken, http.MethodGet, "/api/admin/storage/usage", nil, &usage); err != nil {
		return err
	}
	ok("tasks/stats 与 storage/usage 均可用（total_bytes=%v）", usage["total_bytes"])

	return nil
}

// runTask 提交一条任务并等它到终态，途中要求 SSE 至少推过它的状态。
func runTask(ctx context.Context, token string, sse *sseClient, m *modelSchema, prompt string) (*task, error) {
	created, err := submitTask(ctx, token, m, prompt, fmt.Sprintf("smoke-%d", time.Now().UnixNano()))
	if err != nil {
		return nil, err
	}
	if created.EstimatedCost <= 0 {
		return nil, fmt.Errorf("estimated_cost = %d，服务端没算价", created.EstimatedCost)
	}
	fmt.Printf("  · 已入队 task=%s estimated_cost=%d\n", created.TaskID, created.EstimatedCost)

	deadline := time.Now().Add(3 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		var t task
		if err := call(ctx, token, http.MethodGet, "/api/tasks/"+created.TaskID, nil, &t); err != nil {
			return nil, err
		}
		if t.Status != last {
			fmt.Printf("  · 状态 %s\n", t.Status)
			last = t.Status
		}
		switch t.Status {
		case "succeeded":
			if len(t.Assets) == 0 {
				return nil, fmt.Errorf("任务 succeeded 却没有产物——转存必须发生在置成功之前")
			}
			if t.ActualCost == nil {
				return nil, fmt.Errorf("任务成功却没有 actual_cost")
			}
			if !sse.SawTask(created.TaskID) {
				return nil, fmt.Errorf("任务 %s 全程没有从 SSE 推出来", created.TaskID)
			}
			return &t, nil
		case "failed", "canceled":
			return nil, fmt.Errorf("任务进了 %s：%+v", t.Status, t.Error)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("任务 %s 三分钟没跑完", created.TaskID)
}

// checkAssets 断言产物真的落到了本平台存储，而不是留了个上游 URL。
func checkAssets(ctx context.Context, token string, t *task) error {
	for _, a := range t.Assets {
		if a.ID == "" || a.Original == "" {
			return fmt.Errorf("产物字段不全: %+v", a)
		}
		if strings.Contains(a.Original, "http://") || strings.Contains(a.Original, "https://") {
			if !strings.Contains(a.Original, "/api/assets/") {
				return fmt.Errorf("产物 original 指向了平台外的地址 %q，上游 URL 24h 就失效", a.Original)
			}
		}
		body, ct, err := getRaw(ctx, token, "/api/assets/"+a.ID+"/content")
		if err != nil {
			return fmt.Errorf("下载产物 %s: %w", a.ID, err)
		}
		if len(body) == 0 {
			return fmt.Errorf("产物 %s 下载下来是 0 字节", a.ID)
		}
		fmt.Printf("  · 产物 %s type=%s %d bytes content-type=%s\n", a.ID, a.Type, len(body), ct)

		var lineage lineageGraph
		if err := call(ctx, token, http.MethodGet, "/api/assets/"+a.ID+"/lineage", nil, &lineage); err != nil {
			return fmt.Errorf("血缘 %s: %w", a.ID, err)
		}
		if len(lineage.Nodes) == 0 {
			return fmt.Errorf("产物 %s 的血缘图是空的", a.ID)
		}
	}
	return nil
}

func submitTask(ctx context.Context, token string, m *modelSchema, prompt, clientToken string) (*createTaskResponse, error) {
	var out createTaskResponse
	body := map[string]any{
		"model_id":     m.ID,
		"prompt":       prompt,
		"inputs":       map[string][]string{},
		"params":       defaultParams(m),
		"client_token": clientToken,
	}
	if err := call(ctx, token, http.MethodPost, "/api/tasks", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// defaultParams 按 capability schema 拼出一组"照默认值提交"的参数，
// 前端渲染动态表单时做的也是这件事。
//
// 服务端要求可见参数一个不落地提交（价格由参数决定，缺省会让报价含糊），
// 所以这里必须真的走一遍 schema，而不是发个空对象。
//
// 带 visible_when 的参数一律跳过：本 harness 不传任何 inputs、其余参数
// 全取默认值，条件分支不会被触发。真跳错了服务端会带着字段名回 400，
// 是个明确的失败信号，不会静默放过。
func defaultParams(m *modelSchema) map[string]any {
	out := map[string]any{}
	var walk func(specs []paramSpec)
	walk = func(specs []paramSpec) {
		for _, p := range specs {
			// compound 自身不产生 key，取值落在它的 fields 上。
			if len(p.Fields) > 0 {
				walk(p.Fields)
				continue
			}
			if p.VisibleWhen != nil {
				continue
			}
			out[p.Key] = p.Default
		}
	}
	walk(m.Params)
	return out
}

func fetchModels(ctx context.Context, token, etag string) ([]modelSchema, string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GET /api/models -> %d: %s", resp.StatusCode, snippet(raw))
	}
	// openapi.yaml 的 listModels 回的是 {models: [...]}，不是分页信封：
	// 模型池是整份下发的，前端要拿它渲染动态表单，翻页没有意义。
	var page struct {
		Models []modelSchema `json:"models"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, "", fmt.Errorf("解析模型列表: %w: %s", err, snippet(raw))
	}
	return page.Models, resp.Header.Get("ETag"), nil
}

func conditionalGet(ctx context.Context, token, path, etag string) (int, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("If-None-Match", etag)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func pickModel(models []modelSchema, modality string) *modelSchema {
	for i := range models {
		if models[i].Modality == modality && models[i].Enabled {
			return &models[i]
		}
	}
	return nil
}

// ── SSE ────────────────────────────────────────────────────────────

type sseClient struct {
	mu     sync.Mutex
	events []string
	tasks  map[string]bool
	count  int
}

func openSSE(ctx context.Context, token string) (*sseClient, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/stream", nil)
	// token 只走 Authorization 头，绝不放 query——query 会进访问日志。
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("GET /api/stream -> %d: %s", resp.StatusCode, snippet(raw))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		resp.Body.Close()
		return nil, fmt.Errorf("/api/stream 的 Content-Type 是 %q", ct)
	}

	c := &sseClient{tasks: map[string]bool{}}
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		var evName string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				evName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				c.record(evName, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				evName = ""
			}
		}
	}()
	return c, nil
}

func (c *sseClient) record(name, data string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	if name == "" {
		name = "message"
	}
	if !contains(c.events, name) {
		c.events = append(c.events, name)
	}
	var payload struct {
		TaskID string `json:"task_id"`
		ID     string `json:"id"`
	}
	if json.Unmarshal([]byte(data), &payload) == nil {
		if payload.TaskID != "" {
			c.tasks[payload.TaskID] = true
		}
		if payload.ID != "" {
			c.tasks[payload.ID] = true
		}
	}
}

func (c *sseClient) Seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

func (c *sseClient) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

func (c *sseClient) SawTask(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tasks[id]
}

// ── HTTP 小工具 ────────────────────────────────────────────────────

func call(ctx context.Context, token, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s -> %d: %s", method, path, resp.StatusCode, snippet(raw))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s 响应解析失败: %w: %s", method, path, err, snippet(raw))
		}
	}
	return nil
}

func getRaw(ctx context.Context, token, path string) ([]byte, string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("GET %s -> %d: %s", path, resp.StatusCode, snippet(raw))
	}
	return raw, resp.Header.Get("Content-Type"), nil
}

// ── 响应体（只声明断言用得到的字段）─────────────────────────────────

type authResponse struct {
	Token string     `json:"token"`
	User  meResponse `json:"user"`
}

type meResponse struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Role        string `json:"role"`
	Credits     int64  `json:"credits"`
	CreditsHeld int64  `json:"credits_held"`
}

type modelSchema struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Modality string      `json:"modality"`
	Enabled  bool        `json:"enabled"`
	Params   []paramSpec `json:"params"`
	Pricing  any         `json:"pricing"`
}

type paramSpec struct {
	Key         string      `json:"key"`
	Control     string      `json:"control"`
	Default     any         `json:"default"`
	VisibleWhen any         `json:"visible_when"`
	Fields      []paramSpec `json:"fields"`
}

type createTaskResponse struct {
	TaskID        string `json:"task_id"`
	Status        string `json:"status"`
	EstimatedCost int    `json:"estimated_cost"`
}

type task struct {
	ID         string  `json:"id"`
	Status     string  `json:"status"`
	ActualCost *int    `json:"actual_cost"`
	Assets     []asset `json:"assets"`
	Error      any     `json:"error"`
}

type asset struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Original string `json:"original"`
}

type taskPage struct {
	Items      []task `json:"items"`
	NextCursor string `json:"next_cursor"`
}

type assetPage struct {
	Items      []asset `json:"items"`
	NextCursor string  `json:"next_cursor"`
}

type ledgerPage struct {
	Items []struct {
		Type   string `json:"type"`
		Amount int64  `json:"amount"`
	} `json:"items"`
}

type lineageGraph struct {
	Nodes []asset `json:"nodes"`
	Edges []any   `json:"edges"`
}

type skillList struct {
	Skills []any `json:"skills"`
}

type projectResp struct {
	ID string `json:"id"`
}

type canvasSnapshot struct {
	Revision int64 `json:"revision"`
}

// ── 杂项 ───────────────────────────────────────────────────────────

func step(s string) { fmt.Printf("\n\033[1m▶ %s\033[0m\n", s) }
func ok(f string, a ...any) {
	fmt.Printf("  \033[32m✓\033[0m %s\n", fmt.Sprintf(f, a...))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func hasLedgerType(p ledgerPage, typ string) bool {
	for _, e := range p.Items {
		if e.Type == typ {
			return true
		}
	}
	return false
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
