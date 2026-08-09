// Command healthcheck 一条命令回答一个问题：**现在这套东西，用户点下去是真的吗？**
//
// 它和 scripts/smoke 分工不同。smoke 是回归网：把整条产品链（注册 → SSE →
// 转存 → 扣费 → 画布）从头走一遍，任何一环断了就红。healthcheck 只盯四件
// 上线前会要命、而且**会随数据与配置漂移**的事：
//
//	A. 用户端目录里还有没有 mock / 假上游的模型（有一个就是红）
//	B. 前端调用的每个端点，后端是不是真的注册了（静态对表，不需要服务在跑）
//	C. 图片主流程：提交 → succeeded → 产物真的落在本地存储 → 字节数 > 0
//	D. 视频主流程：同上，走完轮询链
//	E. 资产库那一屏渲染得出来吗：没有 text 混进来，视频的宽高时长齐全
//
// 它们的共同点是「代码没改也可能变红」——管理员在后台加一条 mock 模型、
// 有人删了一条路由、上游挂了，单测和 go build 全都发现不了。
//
// 各段互不短路：前面红了后面照跑，最后一次性给汇总，因为上线前你想知道的是
// 「一共有几处不对」，不是「第一处不对在哪」。
//
// 用法：先 `make run`，再 `make health`。
// 目标地址取 AIGC_HEALTH_BASE_URL（缺省回落 AIGC_SMOKE_BASE_URL，再缺省 http://localhost:18080）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// API_BASE 的兜底值，见 frontend/src/api/client.ts。前端所有相对路径都挂在它下面。
const apiBase = "/api"

var baseURL = envOr("AIGC_HEALTH_BASE_URL", envOr("AIGC_SMOKE_BASE_URL", "http://localhost:18080"))

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var r report

	// 路由对表放在最前面：它是纯静态的，服务没起来也能出结论。
	checkRoutes(&r)

	cat, err := checkCatalog(ctx, &r)
	switch {
	case err != nil:
		r.fail("图片主流程", "前置的目录体检没过，跳过：%v", err)
		r.fail("视频主流程", "前置的目录体检没过，跳过：%v", err)
		r.fail("资产库可见性", "前置的目录体检没过，跳过：%v", err)
	case os.Getenv("AIGC_HEALTH_SKIP_FLOWS") != "":
		// 两条主流程每跑一次就向 GPUGeek 真下一单、真扣一次积分。
		// 想高频盯着目录和路由（它们是纯读的）时，用这个开关把花钱的部分关掉。
		fmt.Println("  AIGC_HEALTH_SKIP_FLOWS 已置位，跳过两条主流程（不产生上游消费）")
		// 资产库体检是纯读的，跳过主流程时照跑——它查的是历史产物那一屏。
		checkLibrary(ctx, &r, cat)
	default:
		checkFlow(ctx, &r, cat, "image", "图片主流程",
			envOr("AIGC_HEALTH_IMAGE_MODEL", ""), "a red apple on a wooden table, studio light")
		checkFlow(ctx, &r, cat, "video", "视频主流程",
			envOr("AIGC_HEALTH_VIDEO_MODEL", ""), "a paper plane flying over a calm lake")
		// 放在两条主流程之后：刚生成的那两件产物也要一起过这关，
		// 否则「新产物就缺宽高」这种回归要等下一次跑才暴露。
		checkLibrary(ctx, &r, cat)
	}

	r.print()
	if r.red() {
		os.Exit(1)
	}
}

// ── A. 目录体检 ─────────────────────────────────────────────────────

// catalog 是一次体检里攒下来的、后面两段主流程还要用的东西。
type catalog struct {
	userToken  string
	adminToken string
	models     []modelSchema // 用户端可见的全部模型
	fake       map[string]string
}

// fakeReason 判定一条模型是不是「假上游」。
//
// 判据有两条，缺一不可：protocol_family=mock 是显式的假驱动；base_url 指向
// 回环地址是隐式的假上游（QA 的 qa-dead-upstream 就是 http://127.0.0.1:9/dead，
// 它的 protocol_family 是 images，光看 family 抓不住）。
func fakeReason(family string, prov provider) string {
	if family == "mock" {
		return "protocol_family=mock"
	}
	u, err := url.Parse(prov.BaseURL)
	if err != nil || u.Scheme == "" {
		return "供应商 base_url 不是一个 http(s) 地址：" + prov.BaseURL
	}
	switch host := u.Hostname(); host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return "供应商 " + prov.Name + " 指向本机 " + prov.BaseURL
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "供应商 " + prov.Name + " 的 base_url 不走 http(s)：" + prov.BaseURL
	}
	return ""
}

func checkCatalog(ctx context.Context, r *report) (*catalog, error) {
	const name = "用户端目录"

	adminToken, err := login(ctx, envOr("AIGC_SEED_ADMIN_USERNAME", "admin"),
		envOr("AIGC_SEED_ADMIN_PASSWORD", "admin-dev-only"))
	if err != nil {
		r.fail(name, "管理员登录失败（服务没起来？先 make run / make seed）：%v", err)
		return nil, err
	}

	cat := &catalog{adminToken: adminToken, fake: map[string]string{}}

	// 探针账号固定一个，不每跑一次注册一个新用户——健康自检是要天天跑的，
	// 每跑一次多一行 users 就是自己给自己造假数据。
	userToken, err := ensureProbeUser(ctx, adminToken)
	if err != nil {
		r.fail(name, "准备探针账号：%v", err)
		return nil, err
	}
	cat.userToken = userToken

	// 用户端目录 = 普通用户身份下的 GET /api/models，与前端下拉框拿到的是同一份。
	models, err := listModels(ctx, userToken)
	if err != nil {
		r.fail(name, "GET /api/models：%v", err)
		return nil, err
	}
	cat.models = models

	// 响应体里没有 protocol_family / provider_id（用户不需要知道），
	// 因此真假判定要拿管理端的配置行回查。
	cfgs, err := listAdminModels(ctx, adminToken)
	if err != nil {
		r.fail(name, "GET /api/admin/models：%v", err)
		return nil, err
	}
	provs, err := listProviders(ctx, adminToken)
	if err != nil {
		r.fail(name, "GET /api/admin/providers：%v", err)
		return nil, err
	}
	provByID := map[string]provider{}
	for _, p := range provs {
		provByID[p.ID] = p
	}
	cfgByID := map[string]modelConfig{}
	for _, c := range cfgs {
		cfgByID[c.ID] = c
	}

	byModality := map[string]int{}
	var unknown []string
	for _, m := range models {
		byModality[m.Modality]++
		c, okCfg := cfgByID[m.ID]
		if !okCfg {
			unknown = append(unknown, m.ID)
			continue
		}
		if why := fakeReason(c.Family, provByID[c.ProviderID]); why != "" {
			cat.fake[m.ID] = why
		}
	}

	fmt.Printf("  用户可见模型 %d 个：image=%d video=%d text=%d\n",
		len(models), byModality["image"], byModality["video"], byModality["text"])
	for _, m := range models {
		mark := "真"
		if why, bad := cat.fake[m.ID]; bad {
			mark = "假 · " + why
		}
		fmt.Printf("    · %-28s %-6s %s\n", m.ID, m.Modality, mark)
	}

	switch {
	case len(models) == 0:
		r.fail(name, "用户端一个模型都看不见——目录是空的")
	case len(unknown) > 0:
		r.fail(name, "这些模型在用户端出现却查不到配置行：%s", strings.Join(unknown, ", "))
	case len(cat.fake) > 0:
		var lines []string
		for id, why := range cat.fake {
			lines = append(lines, id+"（"+why+"）")
		}
		sort.Strings(lines)
		r.fail(name, "用户端目录里有 %d 个假模型：%s", len(cat.fake), strings.Join(lines, "；"))
	default:
		r.pass(name, "%d 个模型全部指向真实上游，mock/假上游 0 个", len(models))
	}

	return cat, nil
}

// ensureProbeUser 保证探针账号存在、能登录、且积分够跑两条主流程。
func ensureProbeUser(ctx context.Context, adminToken string) (string, error) {
	const username = "health_probe"
	password := envOr("AIGC_HEALTH_PROBE_PASSWORD", "health-probe-dev-only")

	var auth authResponse
	err := call(ctx, "", http.MethodPost, "/api/auth/login",
		map[string]any{"username": username, "password": password}, &auth)
	if err != nil {
		// 第一次跑没有这个账号，注册出来；注册也失败才是真的失败。
		if err2 := call(ctx, "", http.MethodPost, "/api/auth/register",
			map[string]any{"username": username, "password": password}, &auth); err2 != nil {
			return "", fmt.Errorf("登录失败（%v）且注册也失败：%w", err, err2)
		}
	}

	var me meResponse
	if err := call(ctx, auth.Token, http.MethodGet, "/api/me", nil, &me); err != nil {
		return "", err
	}
	// 真实上游是要花钱的，余额见底时自检会以「任务失败」的形态变红，
	// 那是个会误导人的红：链路没坏，只是没钱了。这里先补上。
	const floor = 20000
	if me.Credits < floor {
		key := fmt.Sprintf("health-topup-%s", time.Now().UTC().Format("2006-01-02T15"))
		if err := call(ctx, adminToken, http.MethodPost, "/api/admin/users/"+me.ID+"/credits",
			map[string]any{"amount": floor, "reason": "health check", "idempotency_key": key},
			nil); err != nil {
			return "", fmt.Errorf("给探针账号补积分：%w", err)
		}
	}
	return auth.Token, nil
}

// ── B. 路由对表 ─────────────────────────────────────────────────────

var (
	reBackendRoute = regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+) (/[^"]*)"`)
	reMethodOpt    = regexp.MustCompile(`method:\s*'([A-Z]+)'`)
	rePathParam    = regexp.MustCompile(`\{[^}]*\}`)
)

func checkRoutes(r *report) {
	const name = "前后端路由对表"

	root, err := repoRoot()
	if err != nil {
		r.fail(name, "找不到仓库根目录：%v", err)
		return
	}

	backend, err := backendRoutes(filepath.Join(root, "internal", "httpapi", "routes.go"))
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	frontend, err := frontendCalls(filepath.Join(root, "frontend", "src"))
	if err != nil {
		r.fail(name, "%v", err)
		return
	}

	var missing []string
	for ep, where := range frontend {
		if _, has := backend[ep]; !has {
			missing = append(missing, ep+"（"+where+"）")
		}
	}
	var unused []string
	for ep := range backend {
		if _, has := frontend[ep]; !has {
			unused = append(unused, ep)
		}
	}
	sort.Strings(missing)
	sort.Strings(unused)

	fmt.Printf("  前端调用 %d 个端点，后端注册 %d 个\n", len(frontend), len(backend))
	if len(unused) > 0 {
		// 后端有、前端没用**不是**故障：webhook、healthz、管理端还没做的页面
		// 都长这样。列出来是给人看的，不参与红绿。
		fmt.Printf("  后端有而前端未用（仅供参考，不判红）：\n")
		for _, ep := range unused {
			fmt.Printf("    · %s\n", ep)
		}
	}
	if len(missing) > 0 {
		r.fail(name, "前端调用但后端没有这 %d 个端点：%s", len(missing), strings.Join(missing, "；"))
		return
	}
	r.pass(name, "前端调用的 %d 个端点后端全部注册", len(frontend))
}

func backendRoutes(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读 routes.go：%w", err)
	}
	out := map[string]string{}
	for _, m := range reBackendRoute.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]+" "+normPath(m[2])] = path
	}
	if len(out) == 0 {
		return nil, errors.New("routes.go 里一条 mux.Handle 都没解析出来——正则和代码对不上了")
	}
	return out, nil
}

// frontendCalls 把前端源码里所有出网请求抠出来，规格化成 "METHOD /api/..."。
//
// 两种形态都要认：走 api/client.ts 的 request<T>('/path', {method}) 是大头，
// 但 SSE 是直接 fetch(`${API_BASE}/stream`) 的——只扫前者会漏掉它，
// 而它恰恰是最不该断的一条。
func frontendCalls(srcDir string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.Walk(srcDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || (!strings.HasSuffix(p, ".ts") && !strings.HasSuffix(p, ".tsx")) {
			return nil
		}
		// mock/ 下是浏览器内的假后端，它「实现」端点而不是调用端点。
		if strings.Contains(filepath.ToSlash(p), "/mock/") {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel := filepath.Base(p)
		for ep := range scanCalls(string(raw)) {
			out[ep] = rel
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("扫前端源码：%w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("前端源码里一个请求都没解析出来——正则和代码对不上了")
	}
	return out, nil
}

func scanCalls(src string) map[string]bool {
	out := map[string]bool{}
	add := func(path, tail string) {
		method := http.MethodGet
		if m := reMethodOpt.FindStringSubmatch(tail); m != nil {
			method = m[1]
		}
		out[method+" "+normPath(apiBase+path)] = true
	}

	// 形态一：request<...>('/path' 或 request<...>(`/path`
	for i := 0; ; {
		k := strings.Index(src[i:], "request<")
		if k < 0 {
			break
		}
		i += k + len("request<")
		open := strings.IndexByte(src[i:], '(')
		if open < 0 {
			break
		}
		j := i + open + 1
		for j < len(src) && (src[j] == ' ' || src[j] == '\n' || src[j] == '\t') {
			j++
		}
		if j >= len(src) || (src[j] != '\'' && src[j] != '"' && src[j] != '`') {
			continue // request<T>(path: string, ...) 的声明本身，不是调用
		}
		lit, end := readLiteral(src, j)
		if end < 0 {
			continue
		}
		add(lit, until(src, end, "request<"))
		i = end
	}

	// 形态二：fetch(`${API_BASE}/path`)
	for i := 0; ; {
		k := strings.Index(src[i:], "${API_BASE}")
		if k < 0 {
			break
		}
		i += k + len("${API_BASE}")
		end := strings.IndexByte(src[i:], '`')
		if end < 0 {
			break
		}
		add(src[i:i+end], until(src, i+end, "${API_BASE}"))
		i += end
	}
	return out
}

// readLiteral 从 src[start]（一个引号）开始读到配对的引号，返回字面量内容与右引号之后的下标。
// 模板串里的 ${...} 原样保留，交给 normPath 处理。
func readLiteral(src string, start int) (string, int) {
	quote := src[start]
	var b strings.Builder
	for i := start + 1; i < len(src); i++ {
		switch {
		case src[i] == '\\':
			i++
		case quote == '`' && src[i] == '$' && i+1 < len(src) && src[i+1] == '{':
			// 插值内部可以再嵌模板串和大括号，按深度吃掉。
			depth, j := 1, i+2
			for ; j < len(src) && depth > 0; j++ {
				switch src[j] {
				case '{':
					depth++
				case '}':
					depth--
				}
			}
			b.WriteString("*")
			i = j - 1
		case src[i] == quote:
			return b.String(), i + 1
		default:
			b.WriteByte(src[i])
		}
	}
	return "", -1
}

// until 取 src[from:] 到下一个 marker 之前的片段——一次调用的选项对象一定在这段里，
// 下一次调用的选项一定不在。
func until(src string, from int, marker string) string {
	rest := src[from:]
	if k := strings.Index(rest, marker); k >= 0 {
		rest = rest[:k]
	}
	return rest
}

// normPath 把两边的路径压到同一个形状上：路径参数一律 *，查询串一律丢掉。
//
// 前端的 `/tasks/${id}` 与后端的 `/api/tasks/{taskId}` 说的是同一条路由，
// 参数叫什么名字不影响「这条路由存不存在」。而 `/assets${qs}` 这种把整个
// 查询串拼进模板的写法，插值不是一个路径段，要连同它一起抹掉。
func normPath(p string) string {
	p = rePathParam.ReplaceAllString(p, "*")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if s != "*" {
			segs[i] = strings.ReplaceAll(s, "*", "")
		}
	}
	p = strings.Join(segs, "/")
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("从当前目录一路往上没找到 go.mod")
		}
		dir = parent
	}
}

// ── C/D. 主流程 ─────────────────────────────────────────────────────

func checkFlow(ctx context.Context, r *report, cat *catalog, modality, name, forceID, prompt string) {
	m := pickModel(cat, modality, forceID)
	if m == nil {
		r.fail(name, "用户端没有一个可用的 %s 真实模型", modality)
		return
	}
	fmt.Printf("  用模型 %s（%s）跑一次\n", m.ID, m.Name)

	created := createTaskResponse{}
	body := map[string]any{
		"model_id":     m.ID,
		"prompt":       prompt,
		"inputs":       map[string][]string{},
		"params":       defaultParams(m),
		"client_token": fmt.Sprintf("health-%s-%d", modality, time.Now().UnixNano()),
	}
	if err := call(ctx, cat.userToken, http.MethodPost, "/api/tasks", body, &created); err != nil {
		r.fail(name, "提交任务：%v", err)
		return
	}
	fmt.Printf("  · 已入队 task=%s estimated_cost=%d\n", created.TaskID, created.EstimatedCost)

	t, err := poll(ctx, cat.userToken, created.TaskID)
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	if len(t.Assets) == 0 {
		r.fail(name, "任务 %s 成功了却没有产物", t.ID)
		return
	}

	var total int
	for _, a := range t.Assets {
		// 产物必须已经转存到本平台：上游 URL 一般 24h 就失效，
		// 留着它等于给用户一张过两天就打不开的图。
		if strings.HasPrefix(a.Original, "http://") || strings.HasPrefix(a.Original, "https://") {
			if !strings.Contains(a.Original, "/api/assets/") {
				r.fail(name, "产物 %s 的 original 还指着平台外的 %s，没转存", a.ID, a.Original)
				return
			}
		}
		raw, ct, err := getRaw(ctx, cat.userToken, "/api/assets/"+a.ID+"/content")
		if err != nil {
			r.fail(name, "下载产物 %s：%v", a.ID, err)
			return
		}
		if len(raw) == 0 {
			r.fail(name, "产物 %s 下载下来是 0 字节", a.ID)
			return
		}
		total += len(raw)
		fmt.Printf("  · 产物 %s type=%s %d bytes content-type=%s\n", a.ID, a.Type, len(raw), ct)
	}
	r.pass(name, "%s：%d 个产物共 %d 字节，已落本地存储，计费 %d",
		m.ID, len(t.Assets), total, deref(t.ActualCost))
}

// pickModel 从用户端目录里挑一个真实上游的模型。挑不到宁可红，也不退回 mock——
// 用 mock 跑通的主流程正是这次要清掉的东西。
func pickModel(cat *catalog, modality, forceID string) *modelSchema {
	for i := range cat.models {
		m := &cat.models[i]
		if forceID != "" {
			if m.ID == forceID {
				return m
			}
			continue
		}
		if m.Modality == modality && cat.fake[m.ID] == "" {
			return m
		}
	}
	return nil
}

func poll(ctx context.Context, token, taskID string) (*task, error) {
	deadline := time.Now().Add(5 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		var t task
		if err := call(ctx, token, http.MethodGet, "/api/tasks/"+taskID, nil, &t); err != nil {
			return nil, err
		}
		if t.Status != last {
			fmt.Printf("  · 状态 %s\n", t.Status)
			last = t.Status
		}
		switch t.Status {
		case "succeeded":
			return &t, nil
		case "failed", "canceled":
			return nil, fmt.Errorf("任务 %s 进了 %s：%v", taskID, t.Status, t.Error)
		}
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("任务 %s 五分钟没跑完", taskID)
}

// defaultParams 按 capability schema 拼一组「照默认值提交」的参数，
// 与前端渲染动态表单时做的是同一件事。带 visible_when 的参数跳过：
// 本自检不传任何 inputs，条件分支不会被触发。
func defaultParams(m *modelSchema) map[string]any {
	out := map[string]any{}
	var walk func([]paramSpec)
	walk = func(specs []paramSpec) {
		for _, p := range specs {
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

// ── E. 资产库可见性 ──────────────────────────────────────────────────

// checkLibrary 体检探针账号的资产库——也就是「用户点开资产库看到的那一屏」。
//
// 两条断言各自钉着一个已经踩过的坑：
//
//	1. type=text 不该出现。它是管线的中间产物（分镜脚本的对话回复、合成任务
//	   的片段清单），没有可看的画面，混进瀑布流就是一块裂图。过滤做在 store
//	   层，这里守的是「别哪天又把它放回来」。
//	2. 视频必须有宽高与时长。上游多半不报这三项，靠本地 ffprobe 补；缺了的话
//	   卡片显示「▶ 0s」、瀑布流也算不出高度——看起来就跟假数据一样。
//
// 库是空的时候既不判红也不判绿：全新环境还没有产物，那不是故障。
func checkLibrary(ctx context.Context, r *report, cat *catalog) {
	const name = "资产库可见性"

	var page struct {
		Items []asset `json:"items"`
	}
	if err := call(ctx, cat.userToken, http.MethodGet, "/api/assets?limit=100", nil, &page); err != nil {
		r.fail(name, "GET /api/assets：%v", err)
		return
	}
	if len(page.Items) == 0 {
		fmt.Println("  探针账号的资产库是空的，跳过资产库体检（不判红）")
		return
	}

	var texts, blind []string
	var videos int
	for _, a := range page.Items {
		switch a.Type {
		case "text":
			texts = append(texts, a.ID)
		case "video":
			videos++
			if a.Width == nil || a.Height == nil || a.DurationMS == nil {
				blind = append(blind, fmt.Sprintf("%s(w=%s h=%s ms=%s)",
					a.ID, optInt(a.Width), optInt(a.Height), optInt(a.DurationMS)))
			}
		}
	}

	fmt.Printf("  资产库前 %d 项：video=%d text=%d\n", len(page.Items), videos, len(texts))
	switch {
	case len(texts) > 0:
		r.fail(name, "资产库里混进了 %d 件 text 产物（前端会拿 <img> 渲染成裂图）：%s",
			len(texts), strings.Join(texts, ", "))
	case len(blind) > 0:
		r.fail(name, "%d 件视频缺宽高或时长（卡片会显示「▶ 0s」，瀑布流算不出高度）：%s",
			len(blind), strings.Join(blind, "；"))
	default:
		r.pass(name, "%d 项全部渲染得出来：text 0 件，%d 件视频的宽高时长齐全",
			len(page.Items), videos)
	}
}

// optInt 把可空整型摊进错误信息，nil 记成 null——报「w=null」比报「w=0」
// 更能说明是没写回来，而不是写回了一个 0。
func optInt(p *int) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprint(*p)
}

// ── 汇总 ────────────────────────────────────────────────────────────

type result struct {
	name string
	ok   bool
	msg  string
}

type report struct{ items []result }

func (r *report) pass(name, f string, a ...any) {
	r.items = append(r.items, result{name, true, fmt.Sprintf(f, a...)})
	fmt.Printf("  \033[32m✓\033[0m %s：%s\n\n", name, fmt.Sprintf(f, a...))
}

func (r *report) fail(name, f string, a ...any) {
	r.items = append(r.items, result{name, false, fmt.Sprintf(f, a...)})
	fmt.Printf("  \033[31m✗\033[0m %s：%s\n\n", name, fmt.Sprintf(f, a...))
}

func (r *report) red() bool {
	for _, it := range r.items {
		if !it.ok {
			return true
		}
	}
	return false
}

func (r *report) print() {
	fmt.Println("──────── 主流程健康度 ────────")
	for _, it := range r.items {
		mark, color := "RED  ", "\033[31m"
		if it.ok {
			mark, color = "GREEN", "\033[32m"
		}
		fmt.Printf("%s%s\033[0m  %-16s %s\n", color, mark, it.name, it.msg)
	}
	if r.red() {
		fmt.Printf("\n\033[31mHEALTH: RED\033[0m\n")
		return
	}
	fmt.Printf("\n\033[32mHEALTH: GREEN\033[0m\n")
}

// ── HTTP ────────────────────────────────────────────────────────────

func login(ctx context.Context, username, password string) (string, error) {
	var auth authResponse
	if err := call(ctx, "", http.MethodPost, "/api/auth/login",
		map[string]any{"username": username, "password": password}, &auth); err != nil {
		return "", err
	}
	return auth.Token, nil
}

func listModels(ctx context.Context, token string) ([]modelSchema, error) {
	var page struct {
		Models []modelSchema `json:"models"`
	}
	err := call(ctx, token, http.MethodGet, "/api/models", nil, &page)
	return page.Models, err
}

func listAdminModels(ctx context.Context, token string) ([]modelConfig, error) {
	var page struct {
		Models []modelConfig `json:"models"`
	}
	err := call(ctx, token, http.MethodGet, "/api/admin/models", nil, &page)
	return page.Models, err
}

func listProviders(ctx context.Context, token string) ([]provider, error) {
	var page struct {
		Providers []provider `json:"providers"`
	}
	err := call(ctx, token, http.MethodGet, "/api/admin/providers", nil, &page)
	return page.Providers, err
}

func call(ctx context.Context, token, method, path string, body, out any) error {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return nil, "", err
	}
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
	ID       string `json:"id"`
	Username string `json:"username"`
	Credits  int64  `json:"credits"`
}

type modelSchema struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Modality string      `json:"modality"`
	Params   []paramSpec `json:"params"`
}

type paramSpec struct {
	Key         string      `json:"key"`
	Default     any         `json:"default"`
	VisibleWhen any         `json:"visible_when"`
	Fields      []paramSpec `json:"fields"`
}

type modelConfig struct {
	ID         string `json:"id"`
	ProviderID string `json:"provider_id"`
	Family     string `json:"protocol_family"`
	Visibility string `json:"visibility"`
}

type provider struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
}

type createTaskResponse struct {
	TaskID        string `json:"task_id"`
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
	ID         string `json:"id"`
	Type       string `json:"type"`
	Original   string `json:"original"`
	Width      *int   `json:"width"`
	Height     *int   `json:"height"`
	DurationMS *int   `json:"duration_ms"`
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

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
