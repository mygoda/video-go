package adapter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

type fakeSync struct{ name string }

func (f fakeSync) Name() string                  { return f.name }
func (f fakeSync) Family() domain.ProtocolFamily { return domain.FamilyImages }
func (f fakeSync) Invoke(context.Context, SubmitInput) (SubmitResult, error) {
	return SubmitResult{}, nil
}

type fakeAsync struct{ name string }

func (f fakeAsync) Name() string                  { return f.name }
func (f fakeAsync) Family() domain.ProtocolFamily { return domain.FamilyVideo }
func (f fakeAsync) Submit(context.Context, SubmitInput) (SubmitResult, error) {
	return SubmitResult{}, nil
}
func (f fakeAsync) Poll(context.Context, PollRequest) (PollResult, error) { return PollResult{}, nil }
func (f fakeAsync) FetchArtifact(context.Context, ArtifactRef, PollRequest) (ArtifactStream, error) {
	return ArtifactStream{}, nil
}
func (f fakeAsync) DefaultPollInterval() time.Duration { return time.Second }

// 既同步又异步的驱动是合法的（mock 就长这样：同一个包同时注册两个实例）。
type fakeBoth struct {
	fakeSync
	fakeAsync
}

func (f fakeBoth) Name() string                  { return "both" }
func (f fakeBoth) Family() domain.ProtocolFamily { return domain.FamilyMock }

func mutable(t *testing.T) Mutable {
	t.Helper()
	r, ok := NewRegistry().(Mutable)
	if !ok {
		t.Fatal("NewRegistry 的返回值必须实现 Mutable，否则装配方无从注册")
	}
	return r
}

// TestRegisterAndResolve 验证注册与按名查找的基本行为，
// 以及同步表与异步表相互独立——一个名字在同步表里查不到是个明确的答案。
func TestRegisterAndResolve(t *testing.T) {
	r := mutable(t)
	if err := r.Register(fakeSync{name: "images"}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := r.Register(fakeAsync{name: "ark"}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	if _, ok := r.Sync("images"); !ok {
		t.Error("同步驱动 images 应当查得到")
	}
	if _, ok := r.Async("images"); ok {
		t.Error("images 只注册了同步实现，异步表里不该有它")
	}
	if _, ok := r.Async("ark"); !ok {
		t.Error("异步驱动 ark 应当查得到")
	}
	if _, ok := r.Sync("ark"); ok {
		t.Error("ark 只注册了异步实现，同步表里不该有它")
	}
	if _, ok := r.Sync("nope"); ok {
		t.Error("未注册的名字不该查到")
	}
}

// TestRegisterBothInterfaces 验证一个驱动同时实现两个接口时两张表都进。
func TestRegisterBothInterfaces(t *testing.T) {
	r := mutable(t)
	if err := r.Register(fakeBoth{}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if _, ok := r.Sync("both"); !ok {
		t.Error("同步表里应当有 both")
	}
	if _, ok := r.Async("both"); !ok {
		t.Error("异步表里应当有 both")
	}
}

// TestRegisterRejectsBadInput 钉死装配期的三种错误。
//
// 重复注册报错而不是静默覆盖：两个驱动抢同一个名字是配置错误，
// 让进程起不来比让线上随机走到其中一个要好。
func TestRegisterRejectsBadInput(t *testing.T) {
	r := mutable(t)
	if err := r.Register(nil); err == nil {
		t.Error("nil 驱动应当被拒绝")
	}
	if err := r.Register(fakeSync{name: ""}); err == nil {
		t.Error("空名字应当被拒绝")
	}
	if err := r.Register(fakeSync{name: "dup"}); err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}
	if err := r.Register(fakeSync{name: "dup"}); err == nil {
		t.Error("同名重复注册应当报错，而不是静默覆盖")
	}
}

func TestMustRegisterPanicsOnDuplicate(t *testing.T) {
	r := mutable(t)
	r.MustRegister(fakeSync{name: "x"})
	defer func() {
		if recover() == nil {
			t.Error("MustRegister 遇到重复注册应当 panic")
		}
	}()
	r.MustRegister(fakeSync{name: "x"})
}

// TestNamesIsSortedUnion 保证 Names 稳定有序。
// map 迭代顺序随机会让 admin 的协议下拉框在两次刷新之间抖动，看着像配置变了。
func TestNamesIsSortedUnion(t *testing.T) {
	r := mutable(t)
	r.MustRegister(fakeAsync{name: "openai_video"})
	r.MustRegister(fakeSync{name: "images"})
	r.MustRegister(fakeAsync{name: "ark"})
	r.MustRegister(fakeBoth{})

	got := r.Names()
	want := []string{"ark", "both", "images", "openai_video"}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
}

// TestDriverName 钉死"模型配置 → 驱动查找键"这条唯一的翻译规则。
//
// video 族按 video_protocol 再分一层，其余族按 protocol_family 本身查。
// 这个 if 必须只存在于 DriverName 里：一旦调用方各自拼这个键，
// "video 内部还要再分一层"这条知识就散到了全栈。
func TestDriverName(t *testing.T) {
	ark := domain.VideoProtocolArk
	mockProto := domain.VideoProtocolMock

	cases := []struct {
		name  string
		model domain.ModelConfig
		want  string
	}{
		{"images", domain.ModelConfig{Family: domain.FamilyImages}, "images"},
		{"chat", domain.ModelConfig{Family: domain.FamilyChat}, "chat"},
		{"mock", domain.ModelConfig{Family: domain.FamilyMock}, "mock"},
		{"video/ark", domain.ModelConfig{Family: domain.FamilyVideo, VideoProtocol: &ark}, "ark"},
		// mock 的视频模型在种子数据里是 video + video_protocol=mock，
		// 查找键因此是 "mock" 而不是 "video"。
		{"video/mock", domain.ModelConfig{Family: domain.FamilyVideo, VideoProtocol: &mockProto}, "mock"},
		// 配置缺 video_protocol 时退回族名，让报错说"video 驱动未注册"
		// 而不是拿一个空串去查。
		{"video 缺协议", domain.ModelConfig{Family: domain.FamilyVideo}, "video"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DriverName(c.model); got != c.want {
				t.Errorf("DriverName() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestResolveReportsMissingDriver 保证找不到驱动时的报错点名模型与查找键，
// 而不是回一个 nil 让调用方在下一行空指针。
func TestResolveReportsMissingDriver(t *testing.T) {
	r := mutable(t)
	r.MustRegister(fakeSync{name: "images"})
	ark := domain.VideoProtocolArk

	if _, err := ResolveSync(r, domain.ModelConfig{ID: "m1", Family: domain.FamilyImages}); err != nil {
		t.Fatalf("已注册的驱动应当解析成功: %v", err)
	}
	_, err := ResolveAsync(r, domain.ModelConfig{ID: "m2", Family: domain.FamilyVideo, VideoProtocol: &ark})
	if err == nil {
		t.Fatal("未注册的异步驱动应当报错")
	}
	for _, want := range []string{"m2", "ark"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误消息 %q 应当包含 %q", err.Error(), want)
		}
	}
}

// TestConcurrentRegisterAndLookup 覆盖"启动后热注册"与并发查找并存的场景。
func TestConcurrentRegisterAndLookup(t *testing.T) {
	r := mutable(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			r.Sync("images")
			r.Async("ark")
			r.Names()
		}
	}()
	r.MustRegister(fakeSync{name: "images"})
	r.MustRegister(fakeAsync{name: "ark"})
	<-done
}
