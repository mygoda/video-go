package executor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
)

// ─────────────────────────────────────────────────────────────────────
// 内存假件。它们只实现被测路径真正会碰的方法，其余返回零值——
// 假件写全等于把 mysql 实现再写一遍，而那些方法在这些用例里一次都不会被调到。
// ─────────────────────────────────────────────────────────────────────

// fakeTasks 是 store.TaskRepo 的内存实现，UpdateStatus 严格模拟
// `UPDATE ... WHERE status = expect` 的 CAS 语义（这正是被测对象）。
type fakeTasks struct {
	mu    sync.Mutex
	tasks map[string]*domain.Task
	// transitions 按序记录状态迁移，用来断言"转存发生在置 succeeded 之前"。
	transitions []string
}

func newFakeTasks(ts ...domain.Task) *fakeTasks {
	f := &fakeTasks{tasks: make(map[string]*domain.Task)}
	for i := range ts {
		t := ts[i]
		f.tasks[t.ID] = &t
	}
	return f
}

func (f *fakeTasks) Get(_ context.Context, id string) (domain.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return domain.Task{}, &domain.Error{Code: domain.CodeNotFound, Message: "no task"}
	}
	return *t, nil
}

func (f *fakeTasks) UpdateStatus(_ context.Context, id string, expect, next domain.TaskStatus, terr *domain.TaskError) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return &domain.Error{Code: domain.CodeNotFound, Message: "no task"}
	}
	if t.Status != expect {
		// CAS 输了。真实实现返回 affected rows == 0 的冲突错误。
		return &domain.Error{Code: domain.CodeConflict, Message: "status changed"}
	}
	t.Status = next
	t.Error = terr
	f.transitions = append(f.transitions, "status:"+string(next))
	return nil
}

func (f *fakeTasks) UpdateProgress(context.Context, string, *float64, *int, *float64) error {
	return nil
}

func (f *fakeTasks) SetUpstreamRef(_ context.Context, id, ref, raw string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tasks[id]; ok {
		t.UpstreamRef = &ref
		t.UpstreamStatusRaw = &raw
	}
	return nil
}

func (f *fakeTasks) SetNextPoll(context.Context, string, time.Time) error { return nil }

func (f *fakeTasks) IncrementAttempt(_ context.Context, id string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return 0, errors.New("no task")
	}
	t.Attempt++
	return t.Attempt, nil
}

func (f *fakeTasks) SetActualCost(_ context.Context, id string, actual int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tasks[id]; ok {
		t.ActualCost = &actual
	}
	return nil
}

func (f *fakeTasks) note(s string) {
	f.mu.Lock()
	f.transitions = append(f.transitions, s)
	f.mu.Unlock()
}

func (f *fakeTasks) trace() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.transitions))
	copy(out, f.transitions)
	return out
}

// 以下是 TaskRepo 里本层用不到的方法，补齐接口。
func (f *fakeTasks) Create(_ context.Context, t domain.Task) (domain.Task, error) { return t, nil }
func (f *fakeTasks) GetByClientToken(context.Context, string, string) (domain.Task, bool, error) {
	return domain.Task{}, false, nil
}
func (f *fakeTasks) List(context.Context, domain.TaskFilter) (store.Page[domain.Task], error) {
	return store.Page[domain.Task]{}, nil
}
func (f *fakeTasks) ListActive(context.Context, string) ([]domain.Task, error) { return nil, nil }
func (f *fakeTasks) CountRunningByModel(context.Context, string, string) (int, error) {
	return 0, nil
}
func (f *fakeTasks) Requeue(context.Context, string) error { return nil }
func (f *fakeTasks) Stats(context.Context, time.Duration, string) (domain.TaskStats, error) {
	return domain.TaskStats{}, nil
}

// fakeLedger 记账，只关心 Charge / Refund 被调了几次、金额多少。
type fakeLedger struct {
	mu      sync.Mutex
	charged []int
	refunds int
}

func (l *fakeLedger) Hold(context.Context, string, string, int) (domain.LedgerEntry, error) {
	return domain.LedgerEntry{}, nil
}

func (l *fakeLedger) Charge(_ context.Context, _, _ string, amount int) (domain.LedgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.charged = append(l.charged, amount)
	return domain.LedgerEntry{}, nil
}

func (l *fakeLedger) Refund(context.Context, string, string, string) (domain.LedgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refunds++
	return domain.LedgerEntry{}, nil
}

func (l *fakeLedger) Topup(context.Context, string, int, string, string, string) (domain.LedgerEntry, error) {
	return domain.LedgerEntry{}, nil
}

func (l *fakeLedger) Balance(context.Context, string) (domain.Balance, error) {
	return domain.Balance{}, nil
}

func (l *fakeLedger) counts() (charges []int, refunds int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := make([]int, len(l.charged))
	copy(c, l.charged)
	return c, l.refunds
}

// fakeQueue 只需要能被 Release / Renew 调到而不报错。
type fakeQueue struct{}

func (q *fakeQueue) Claim(context.Context, string, time.Duration, []string) (Lease, bool, error) {
	return Lease{}, false, nil
}
func (q *fakeQueue) Renew(context.Context, string, string, time.Duration) error { return nil }
func (q *fakeQueue) Release(context.Context, string, string) error              { return nil }
func (q *fakeQueue) ClaimPoll(context.Context, string, time.Duration, int) ([]Lease, error) {
	return nil, nil
}

// fakeTransferor 转存时向 tasks 打一个时间戳记号，用来验证顺序不变量。
type fakeTransferor struct {
	tasks  *fakeTasks
	err    error
	assets []domain.Asset
}

func (t *fakeTransferor) Transfer(context.Context, string, string, adapter.ArtifactRef, adapter.PollRequest) (domain.Asset, error) {
	return domain.Asset{}, nil
}

func (t *fakeTransferor) TransferAll(_ context.Context, _, _ string, _ []adapter.ArtifactRef, _ adapter.PollRequest) ([]domain.Asset, error) {
	t.tasks.note("transfer")
	if t.err != nil {
		return nil, t.err
	}
	return t.assets, nil
}

func (t *fakeTransferor) Promote(context.Context, string) (domain.Asset, error) {
	return domain.Asset{}, &domain.Error{Code: domain.CodeNotFound}
}

// stubModels / stubProviders 返回一份最小可用的配置：video 族 + ark 协议，
// 因为异步路径（提交 → 轮询 → 转存）才是本层的主干。
type stubModels struct{ pollSeconds *int }

func (m stubModels) Get(context.Context, string) (domain.ModelConfig, error) {
	vp := domain.VideoProtocolArk
	return domain.ModelConfig{
		ID:                  "model-1",
		ProviderID:          "prov-1",
		UpstreamModel:       "seedance-1",
		Family:              domain.FamilyVideo,
		VideoProtocol:       &vp,
		PollIntervalSeconds: m.pollSeconds,
		Enabled:             true,
	}, nil
}
func (m stubModels) List(context.Context, domain.ModelFilter) ([]domain.ModelConfig, error) {
	return nil, nil
}
func (m stubModels) Upsert(_ context.Context, c domain.ModelConfig) (domain.ModelConfig, error) {
	return c, nil
}
func (m stubModels) Delete(context.Context, string) error        { return nil }
func (m stubModels) Fingerprint(context.Context) (string, error) { return "", nil }

type stubProviders struct{}

func (stubProviders) List(context.Context) ([]domain.Provider, error) {
	return []domain.Provider{{ID: "prov-1", Enabled: true}}, nil
}
func (stubProviders) Get(context.Context, string) (domain.Provider, error) {
	return domain.Provider{ID: "prov-1", Name: "ark", Enabled: true}, nil
}
func (stubProviders) Upsert(_ context.Context, p domain.Provider) (domain.Provider, error) {
	return p, nil
}
func (stubProviders) Delete(context.Context, string) error { return nil }

// stubDriver 是可编程的异步驱动：每次 Submit / Poll 依次吐出预置的结果。
type stubDriver struct {
	mu sync.Mutex

	submits    []stubSubmit
	submitCall int

	polls    []stubPoll
	pollCall int

	canceled int
}

type stubSubmit struct {
	res adapter.SubmitResult
	err error
}

type stubPoll struct {
	res adapter.PollResult
	err error
}

func (d *stubDriver) Name() string                       { return string(domain.VideoProtocolArk) }
func (d *stubDriver) Family() domain.ProtocolFamily      { return domain.FamilyVideo }
func (d *stubDriver) DefaultPollInterval() time.Duration { return 5 * time.Second }

func (d *stubDriver) Submit(context.Context, adapter.SubmitInput) (adapter.SubmitResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.submitCall >= len(d.submits) {
		return adapter.SubmitResult{}, errors.New("stub: 没有更多 submit 结果")
	}
	s := d.submits[d.submitCall]
	d.submitCall++
	return s.res, s.err
}

func (d *stubDriver) Poll(context.Context, adapter.PollRequest) (adapter.PollResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pollCall >= len(d.polls) {
		return adapter.PollResult{Status: adapter.StatusRunning}, nil
	}
	p := d.polls[d.pollCall]
	d.pollCall++
	return p.res, p.err
}

func (d *stubDriver) FetchArtifact(context.Context, adapter.ArtifactRef, adapter.PollRequest) (adapter.ArtifactStream, error) {
	return adapter.ArtifactStream{}, errors.New("stub: 不取产物")
}

func (d *stubDriver) CancelUpstream(context.Context, adapter.PollRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.canceled++
	return nil
}

func (d *stubDriver) submitCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.submitCall
}

func (d *stubDriver) cancelCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.canceled
}

// stubRegistry 只认一个驱动名。
type stubRegistry struct{ drv *stubDriver }

func (r stubRegistry) Sync(string) (adapter.SyncDriver, bool) { return nil, false }
func (r stubRegistry) Async(name string) (adapter.AsyncDriver, bool) {
	if r.drv == nil || name != r.drv.Name() {
		return nil, false
	}
	return r.drv, true
}
func (r stubRegistry) Names() []string { return []string{string(domain.VideoProtocolArk)} }

// discardLogger 让测试输出干净：被测路径大量记日志，那是设计意图不是噪音，
// 但它不该淹没 go test 的失败信息。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestService 组装一个只接了被测依赖的 Service。
func newTestService(t *testing.T, tasks *fakeTasks, drv *stubDriver, mut func(*Deps)) *Service {
	t.Helper()
	d := Deps{
		Tasks:     tasks,
		Models:    stubModels{},
		Providers: stubProviders{},
		Queue:     &fakeQueue{},
		Drivers:   stubRegistry{drv: drv},
		Logger:    discardLogger(),
	}
	if mut != nil {
		mut(&d)
	}
	s, err := New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}
