package mock

import (
	"bytes"
	"context"
	"encoding/base64"
	"image/png"
	"io"
	"os"
	"testing"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

func testProvider() domain.Provider {
	return domain.Provider{
		ID:      "p-mock",
		BaseURL: "http://localhost",
		// 环境变量名，不是密钥。mock 根本不读它。
		CredentialRef: "AIGC_PROVIDER_MOCK_KEY",
	}
}

func syncInput(taskID string, params map[string]any) adapter.SubmitInput {
	return adapter.SubmitInput{
		TaskID:   taskID,
		Provider: testProvider(),
		Model: domain.ModelConfig{
			ID:     "mock-image-1",
			Family: domain.FamilyMock,
		},
		Prompt: "一只在雨里的猫",
		Params: params,
	}
}

func asyncInput(taskID string) adapter.SubmitInput {
	proto := domain.VideoProtocolMock
	return adapter.SubmitInput{
		TaskID:   taskID,
		Provider: testProvider(),
		Model: domain.ModelConfig{
			ID:            "mock-video-1",
			Family:        domain.FamilyVideo,
			VideoProtocol: &proto,
		},
		Prompt: "一只在雨里的猫",
	}
}

// TestNormalize 钉死 mock 的状态归一化。
//
// mock 的这张表在字面上是恒等映射，但它必须存在:L2 分辨不出手上的驱动
// 是不是 mock，靠的就是每个驱动在同一个位置做同一件事。
// 未知值按 running 与三个真实驱动完全一致。
func TestNormalize(t *testing.T) {
	cases := []struct {
		raw  string
		want adapter.Status
	}{
		{"queued", adapter.StatusQueued},
		{"running", adapter.StatusRunning},
		{"succeeded", adapter.StatusSucceeded},
		{"failed", adapter.StatusFailed},
		{"canceled", adapter.StatusCanceled},
		{"something_new", adapter.StatusRunning},
		{"", adapter.StatusRunning},
	}
	for _, c := range cases {
		if got := normalize(c.raw); got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
	if len(statusMap) != 5 {
		t.Errorf("statusMap 有 %d 条，期望 5 条", len(statusMap))
	}
}

// TestDriverIdentity 钉死注册键与协议族。
//
// 异步 mock 的 Family 是 video 而不是 mock:种子数据里 mock-video-1 就是
// protocol_family='video' + video_protocol='mock'，Family() 报错会让 L2
// 把它送进同步那条路。
func TestDriverIdentity(t *testing.T) {
	s := NewSyncDriver(Driver{})
	if s.Name() != Name || s.Name() != "mock" {
		t.Errorf("同步 Name() = %q, want mock", s.Name())
	}
	if s.Family() != domain.FamilyMock {
		t.Errorf("同步 Family() = %q, want mock", s.Family())
	}

	a := NewAsyncDriver(Driver{})
	if a.Name() != Name {
		t.Errorf("异步 Name() = %q, want mock", a.Name())
	}
	if a.Family() != domain.FamilyVideo {
		t.Errorf("异步 Family() = %q, want video（种子数据里它是 video + video_protocol=mock）", a.Family())
	}
}

// TestSyncInvokeProducesRealPNG 钉死 mock 的核心承诺：产出的是**真 PNG**，
// 不是一段假字节。零凭证演示时前端要真的把它渲染出来。
func TestSyncInvokeProducesRealPNG(t *testing.T) {
	d := NewSyncDriver(Driver{})
	res, err := d.Invoke(context.Background(), syncInput("t-png", map[string]any{"seed": 7}))
	if err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}
	if res.Status != adapter.StatusSucceeded || res.RawStatus != "succeeded" {
		t.Errorf("状态 = %q / raw %q", res.Status, res.RawStatus)
	}
	if res.UpstreamRef != "" {
		t.Error("同步族的 UpstreamRef 必须为空，否则 L2 会给它排轮询")
	}
	if len(res.Inline) != 1 {
		t.Fatalf("产物数 = %d", len(res.Inline))
	}

	a := res.Inline[0]
	if a.Kind != adapter.KindBase64 || a.Type != domain.AssetTypeImage || a.MIME != PlaceholderImageMIME {
		t.Errorf("产物 = %+v", a)
	}
	raw, err := base64.StdEncoding.DecodeString(a.Base64)
	if err != nil {
		t.Fatalf("产物不是合法 base64: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("产物不是合法 PNG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != 1024 || b.Dy() != 1024 {
		t.Errorf("默认画幅 = %dx%d, want 1024x1024", b.Dx(), b.Dy())
	}
	if a.Width == nil || *a.Width != b.Dx() || a.Height == nil || *a.Height != b.Dy() {
		t.Errorf("ArtifactRef 上的画幅与真实像素不符: %+v / %+v vs %v", a.Width, a.Height, b)
	}
	if a.Bytes != int64(len(raw)) {
		t.Errorf("Bytes = %d, want %d", a.Bytes, len(raw))
	}
}

// TestSyncImageVariesWithSeed 保证画面随参数变化。
//
// 如果所有 mock 产物长得一样，画布上 20 张卡全是同一张图，
// "参数确实传到了底层"这件事在肉眼上完全无法证伪。
func TestSyncImageVariesWithSeed(t *testing.T) {
	d := NewSyncDriver(Driver{})
	render := func(params map[string]any) string {
		res, err := d.Invoke(context.Background(), syncInput("t-seed", params))
		if err != nil {
			t.Fatalf("Invoke 失败: %v", err)
		}
		return res.Inline[0].Base64
	}

	a := render(map[string]any{"seed": 1})
	b := render(map[string]any{"seed": 2})
	if a == b {
		t.Error("换了 seed 却出同一张图：参数没有传到画图那一步")
	}
	// 同 seed 同参数必须复现——真实模型就是这个语义。
	if render(map[string]any{"seed": 1}) != a {
		t.Error("同一个 seed 两次出图不一致，seed 锁定构图这条产品行为在本地验证不了")
	}
}

// TestSyncImageHonorsAspectAndSize 验证画幅来源的优先级：
// 显式 width/height > size > aspect。
func TestSyncImageHonorsAspectAndSize(t *testing.T) {
	d := NewSyncDriver(Driver{})
	decode := func(params map[string]any) (int, int) {
		res, err := d.Invoke(context.Background(), syncInput("t-size", params))
		if err != nil {
			t.Fatalf("Invoke 失败: %v", err)
		}
		raw, _ := base64.StdEncoding.DecodeString(res.Inline[0].Base64)
		cfg, err := png.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("PNG 解析失败: %v", err)
		}
		return cfg.Width, cfg.Height
	}

	if w, h := decode(map[string]any{"size": "1024x768"}); w != 1024 || h != 768 {
		t.Errorf("size=1024x768 → %dx%d", w, h)
	}
	if w, h := decode(map[string]any{"width": 512, "height": 256}); w != 512 || h != 256 {
		t.Errorf("显式 width/height → %dx%d", w, h)
	}

	// 比例按恒定面积换算，16:9 与 9:16 应当互为转置且都不至于极端。
	w1, h1 := decode(map[string]any{"aspect": "16:9"})
	w2, h2 := decode(map[string]any{"aspect": "9:16"})
	if w1 <= h1 {
		t.Errorf("16:9 应当是横图，实际 %dx%d", w1, h1)
	}
	if w2 != h1 || h2 != w1 {
		t.Errorf("9:16(%dx%d) 应当是 16:9(%dx%d) 的转置", w2, h2, w1, h1)
	}
	if w1%8 != 0 || h1%8 != 0 {
		t.Errorf("边长应当对齐到 8 的倍数，实际 %dx%d", w1, h1)
	}
}

// TestSyncMultipleImages 验证一次出多张，且 Index 递增——
// Index 决定前端展示顺序，只出一张的话排序逻辑本地永远走不到。
func TestSyncMultipleImages(t *testing.T) {
	d := NewSyncDriver(Driver{})
	res, err := d.Invoke(context.Background(), syncInput("t-multi", map[string]any{"count": 3}))
	if err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}
	if len(res.Inline) != 3 {
		t.Fatalf("产物数 = %d, want 3", len(res.Inline))
	}
	seen := map[string]bool{}
	for i, a := range res.Inline {
		if a.Index != i {
			t.Errorf("第 %d 件产物的 Index = %d", i, a.Index)
		}
		if seen[a.Base64] {
			t.Errorf("第 %d 件产物与前面重复：同一批的多张图应当互不相同", i)
		}
		seen[a.Base64] = true
	}

	// 上限保护：mock 是本地路径，不该被一个参数拖去画 100 张图。
	res, err = d.Invoke(context.Background(), syncInput("t-cap", map[string]any{"count": 99}))
	if err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}
	if len(res.Inline) != 8 {
		t.Errorf("count=99 时产物数 = %d, want 8（封顶）", len(res.Inline))
	}
}

// TestSyncFailureInjection 验证失败注入，且注入是确定性的:
// 同一个 TaskID 反复重放必得同一个结局，否则"这条为什么失败了"不可复现。
func TestSyncFailureInjection(t *testing.T) {
	d := NewSyncDriver(Driver{FailureRate: 1, FailureCode: domain.TaskErrorContentRejected})
	res, err := d.Invoke(context.Background(), syncInput("t-fail", nil))
	if err == nil {
		t.Fatal("FailureRate=1 时应当失败")
	}
	if res.Status != adapter.StatusFailed || res.RawStatus != "failed" {
		t.Errorf("状态 = %q / raw %q", res.Status, res.RawStatus)
	}
	de, ok := err.(*domain.Error)
	if !ok {
		t.Fatalf("期望 *domain.Error，得到 %T", err)
	}
	if de.Code != domain.ErrorCode(domain.TaskErrorContentRejected) {
		t.Errorf("错误码 = %q, want content_rejected", de.Code)
	}

	if decideOutcome("t-x", 0.5) != decideOutcome("t-x", 0.5) {
		t.Error("同一个 TaskID 两次判定不一致：失败路径必须可复现")
	}
	if decideOutcome("t-y", 0) != outcomeSucceeded {
		t.Error("FailureRate=0 时不该失败")
	}
}

// TestAsyncFullCycle 走完整的 submit → poll(queued) → poll(running) →
// poll(succeeded) → FetchArtifact 一圈。
//
// 这是 mock 存在的全部理由:零凭证下把 L2 的状态机与 L3 的转存
// 从头到尾真的跑一遍。用极短的 SimulatedDelay 把 8 秒压成毫秒级，
// 状态机的形状不变。
func TestAsyncFullCycle(t *testing.T) {
	const total = 300 * time.Millisecond
	d := NewAsyncDriver(Driver{SimulatedDelay: total, PollInterval: 10 * time.Millisecond})

	if d.DefaultPollInterval() != 10*time.Millisecond {
		t.Errorf("DefaultPollInterval() = %v", d.DefaultPollInterval())
	}

	sub, err := d.Submit(context.Background(), asyncInput("t-cycle"))
	if err != nil {
		t.Fatalf("Submit 失败: %v", err)
	}
	if sub.UpstreamRef == "" {
		t.Fatal("异步族必须返回 UpstreamRef")
	}
	if sub.Status != adapter.StatusQueued || sub.RawStatus != "queued" {
		t.Errorf("提交后状态 = %q / raw %q", sub.Status, sub.RawStatus)
	}
	if sub.ETASeconds == nil || *sub.ETASeconds <= 0 {
		t.Errorf("提交应当给出 ETA: %+v", sub.ETASeconds)
	}

	req := adapter.PollRequest{
		TaskID:      "t-cycle",
		Provider:    testProvider(),
		UpstreamRef: sub.UpstreamRef,
	}

	// 立刻轮询：还在排队。真实上游几乎总要排一会儿队，
	// 前端的"排队中"这一态如果本地永远看不到，就等于没实现过。
	first, err := d.Poll(context.Background(), req)
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if first.Status != adapter.StatusQueued || first.RawStatus != "queued" {
		t.Fatalf("首次轮询状态 = %q / raw %q, want queued", first.Status, first.RawStatus)
	}

	// 走到中段：running，且进度在推进。
	time.Sleep(total / 2)
	mid, err := d.Poll(context.Background(), req)
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if mid.Status != adapter.StatusRunning || mid.RawStatus != "running" {
		t.Fatalf("中段轮询状态 = %q / raw %q, want running", mid.Status, mid.RawStatus)
	}
	if mid.Progress == nil || *mid.Progress <= 0 || *mid.Progress >= 1 {
		t.Errorf("running 的进度必须在 (0,1) 内: %+v", mid.Progress)
	}
	if mid.ETASeconds == nil || *mid.ETASeconds < 0 {
		t.Errorf("running 应当给出 ETA: %+v", mid.ETASeconds)
	}

	// 走完：succeeded 且带产物。
	time.Sleep(total)
	last, err := d.Poll(context.Background(), req)
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if last.Status != adapter.StatusSucceeded || last.RawStatus != "succeeded" {
		t.Fatalf("终态 = %q / raw %q, want succeeded", last.Status, last.RawStatus)
	}
	if last.Progress == nil || *last.Progress != 1 {
		t.Errorf("成功时进度应当满格: %+v", last.Progress)
	}
	if len(last.Artifacts) != 1 {
		t.Fatalf("产物数 = %d", len(last.Artifacts))
	}

	a := last.Artifacts[0]
	// KindBinary 与 Sora 同形：产物没有 URL，要靠 FetchArtifact 取字节。
	// mock 走这一种正是为了把那条代码路径也真的跑上。
	if a.Kind != adapter.KindBinary {
		t.Errorf("产物 Kind = %q, want binary", a.Kind)
	}
	if a.URL != "" {
		t.Error("KindBinary 的产物不该带 URL")
	}
	if a.Type != domain.AssetTypeVideo || a.MIME != PlaceholderVideoMIME {
		t.Errorf("产物 = %+v", a)
	}
	if a.DurationMS == nil || *a.DurationMS != MockVideoDurationMS {
		t.Errorf("时长 = %+v", a.DurationMS)
	}
	if a.Width == nil || *a.Width != MockVideoWidth || a.Height == nil || *a.Height != MockVideoHeight {
		t.Errorf("画幅 = %+v / %+v", a.Width, a.Height)
	}

	// 终态是稳定的：再轮询一次结果必须完全一致（Poll 幂等）。
	again, err := d.Poll(context.Background(), req)
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if again.Status != last.Status || again.RawStatus != last.RawStatus {
		t.Errorf("重复轮询结果不一致: %q vs %q", again.Status, last.Status)
	}

	stream, err := d.FetchArtifact(context.Background(), a, req)
	if err != nil {
		t.Fatalf("FetchArtifact 失败: %v", err)
	}
	defer stream.Body.Close()
	raw, err := io.ReadAll(stream.Body)
	if err != nil {
		t.Fatalf("读取产物流失败: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("产物流为空：内嵌样片没有被打进二进制")
	}
	if stream.Bytes != int64(len(raw)) || a.Bytes != int64(len(raw)) {
		t.Errorf("Bytes = %d / %d, 实际读到 %d", stream.Bytes, a.Bytes, len(raw))
	}
	if stream.MIME != PlaceholderVideoMIME {
		t.Errorf("MIME = %q", stream.MIME)
	}
	// ISO BMFF 的 ftyp box:第 5-8 字节必须是 "ftyp"，
	// 否则前端 <video> 根本播不了，"零凭证也能看到视频"这条承诺就是假的。
	if len(raw) < 12 || string(raw[4:8]) != "ftyp" {
		t.Errorf("内嵌样片不是合法 MP4（缺少 ftyp box），前 12 字节 = %q", raw[:minInt(12, len(raw))])
	}
}

// TestAsyncFailurePath 验证异步失败路径：失败在提交时就定死并编进 ref，
// 因此每次轮询给出的结局一致。
func TestAsyncFailurePath(t *testing.T) {
	d := NewAsyncDriver(Driver{
		SimulatedDelay: time.Millisecond,
		FailureRate:    1,
		FailureCode:    domain.TaskErrorUpstreamRateLimited,
	})
	sub, err := d.Submit(context.Background(), asyncInput("t-async-fail"))
	if err != nil {
		t.Fatalf("Submit 失败: %v", err)
	}
	// 提交本身不失败——真实上游也是先收下任务再在轮询里报失败。
	if sub.Status != adapter.StatusQueued {
		t.Errorf("提交状态 = %q, want queued", sub.Status)
	}

	time.Sleep(10 * time.Millisecond)
	res, err := d.Poll(context.Background(), adapter.PollRequest{
		Provider: testProvider(), UpstreamRef: sub.UpstreamRef,
	})
	if err != nil {
		// 失败是任务的结局而不是调用错误：Poll 本身必须成功返回。
		t.Fatalf("Poll 不该返回调用错误: %v", err)
	}
	if res.Status != adapter.StatusFailed || res.RawStatus != "failed" {
		t.Fatalf("状态 = %q / raw %q", res.Status, res.RawStatus)
	}
	if res.Err == nil || res.Err.Code != domain.TaskErrorUpstreamRateLimited {
		t.Fatalf("错误 = %+v", res.Err)
	}
	if res.Err.Charged {
		t.Error("失败三分类一律不扣费")
	}
	if len(res.Artifacts) != 0 {
		t.Error("失败的任务不该带产物")
	}
}

// TestUpstreamRefSurvivesRestart 钉死"状态编码在 ref 里"这条设计。
//
// 真实驱动的状态在上游那边，进程重启不影响。mock 若把状态放进内存 map，
// 重启后在途任务永远卡在 running——而"进程随时可重启、在途任务不丢"
// 正是 upstream_ref 落库要验证的东西。这里用一个全新的驱动实例
// 去轮询另一个实例提交的 ref 来模拟重启。
func TestUpstreamRefSurvivesRestart(t *testing.T) {
	submitter := NewAsyncDriver(Driver{SimulatedDelay: 50 * time.Millisecond})
	sub, err := submitter.Submit(context.Background(), asyncInput("t-restart"))
	if err != nil {
		t.Fatalf("Submit 失败: %v", err)
	}

	time.Sleep(80 * time.Millisecond)

	// 全新实例，没有任何共享内存。
	poller := NewAsyncDriver(Driver{})
	res, err := poller.Poll(context.Background(), adapter.PollRequest{
		Provider: testProvider(), UpstreamRef: sub.UpstreamRef,
	})
	if err != nil {
		t.Fatalf("换个实例轮询失败: %v", err)
	}
	if res.Status != adapter.StatusSucceeded {
		t.Errorf("状态 = %q, want succeeded：状态必须完全由 ref 与时间推出", res.Status)
	}
}

// TestPollRejectsForeignRef 保证拿到别的驱动的 ref 时报一句能看懂的话，
// 而不是解出一堆零值再装作任务还在排队。
func TestPollRejectsForeignRef(t *testing.T) {
	d := NewAsyncDriver(Driver{})
	if _, err := d.Poll(context.Background(), adapter.PollRequest{UpstreamRef: "cgt-123"}); err == nil {
		t.Fatal("非本驱动生成的 ref 应当报错")
	}
}

// TestFetchArtifactRejectsWrongKind 保证 mock 不去处理它根本不产出的形态。
func TestFetchArtifactRejectsWrongKind(t *testing.T) {
	d := NewAsyncDriver(Driver{})
	_, err := d.FetchArtifact(context.Background(),
		adapter.ArtifactRef{Kind: adapter.KindURL, URL: "https://cdn.example/a.mp4"},
		adapter.PollRequest{})
	if err == nil {
		t.Fatal("KindURL 应当被拒绝")
	}
}

// TestPlaceholderRootDump 验证配了 PlaceholderRoot 时产物额外落一份磁盘，
// 且任务 id 里的路径分隔符不会把文件写到目录外面去。
func TestPlaceholderRootDump(t *testing.T) {
	root := t.TempDir()
	d := NewSyncDriver(Driver{PlaceholderRoot: root})
	if _, err := d.Invoke(context.Background(), syncInput("../escape", nil)); err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("落盘文件数 = %d", len(entries))
	}
	if entries[0].Name() != "___escape-0.png" {
		t.Errorf("文件名 = %q，路径分隔符必须被替换掉", entries[0].Name())
	}
}
