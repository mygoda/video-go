package asset

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
)

// recordingAssets 记下 Create 收到的资产与 Delete 被调的键，用来断言回滚。
type recordingAssets struct {
	fakeAssets

	mu        sync.Mutex
	created   []domain.Asset
	deleted   []string
	failAfter int // 第几次 Create 之后开始失败，0 表示不失败
	calls     int
}

func (r *recordingAssets) Create(_ context.Context, a domain.Asset) (domain.Asset, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.failAfter > 0 && r.calls > r.failAfter {
		return domain.Asset{}, errors.New("落库失败")
	}
	r.created = append(r.created, a)
	return a, nil
}

func (r *recordingAssets) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, id)
	return nil
}

func (r *recordingAssets) snapshot() (created []domain.Asset, deleted []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := make([]domain.Asset, len(r.created))
	copy(c, r.created)
	d := make([]string, len(r.deleted))
	copy(d, r.deleted)
	return c, d
}

// countingQuota 记账，用来验证"预占在下载之前、失败要释放"。
type countingQuota struct {
	mu                                sync.Mutex
	reserved, committed, releasedCall int
	reserveErr                        error
}

func (q *countingQuota) Reserve(context.Context, string, int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.reserveErr != nil {
		return q.reserveErr
	}
	q.reserved++
	return nil
}

func (q *countingQuota) Commit(context.Context, string, int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.committed++
	return nil
}

func (q *countingQuota) Release(context.Context, string, int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.releasedCall++
	return nil
}

func (q *countingQuota) Usage(context.Context, string) (Usage, error)     { return Usage{}, nil }
func (q *countingQuota) Recompute(context.Context, string) (Usage, error) { return Usage{}, nil }

func (q *countingQuota) counts() (res, com, rel int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.reserved, q.committed, q.releasedCall
}

// artifactDriver 是能吐产物字节的最小异步驱动。
type artifactDriver struct {
	bodies map[string]string
	failOn string
}

func (d *artifactDriver) Name() string                       { return "ark" }
func (d *artifactDriver) Family() domain.ProtocolFamily      { return domain.FamilyVideo }
func (d *artifactDriver) DefaultPollInterval() time.Duration { return time.Second }
func (d *artifactDriver) Submit(context.Context, adapter.SubmitInput) (adapter.SubmitResult, error) {
	return adapter.SubmitResult{}, nil
}
func (d *artifactDriver) Poll(context.Context, adapter.PollRequest) (adapter.PollResult, error) {
	return adapter.PollResult{}, nil
}

func (d *artifactDriver) FetchArtifact(_ context.Context, ref adapter.ArtifactRef, _ adapter.PollRequest) (adapter.ArtifactStream, error) {
	if d.failOn != "" && ref.URL == d.failOn {
		return adapter.ArtifactStream{}, errors.New("上游 URL 已过期")
	}
	body := d.bodies[ref.URL]
	return adapter.ArtifactStream{
		Body:  io.NopCloser(strings.NewReader(body)),
		MIME:  ref.MIME,
		Bytes: int64(len(body)),
	}, nil
}

type oneDriverRegistry struct{ drv *artifactDriver }

func (r oneDriverRegistry) Sync(string) (adapter.SyncDriver, bool) { return nil, false }
func (r oneDriverRegistry) Async(name string) (adapter.AsyncDriver, bool) {
	if name != r.drv.Name() {
		return nil, false
	}
	return r.drv, true
}
func (r oneDriverRegistry) Names() []string { return []string{"ark"} }

func newTestTransferor(t *testing.T, assets store.AssetRepo, q QuotaGuard, drv *artifactDriver) (Transferor, Store) {
	t.Helper()
	blobs, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	tr, err := NewTransferor(TransferorDeps{
		Blobs:   blobs,
		Assets:  assets,
		Drivers: oneDriverRegistry{drv: drv},
		Quota:   q,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewTransferor: %v", err)
	}
	return tr, blobs
}

func videoReq() adapter.PollRequest {
	vp := domain.VideoProtocolArk
	return adapter.PollRequest{
		Model: domain.ModelConfig{Family: domain.FamilyVideo, VideoProtocol: &vp},
	}
}

// TestTransferWritesBytesBeforeReturningAsset 验证 Transfer 返回的资产背后
// 真的有本地字节——这是「转存先于置 succeeded」这条不变量的另一半。
//
// 只断言"没报错"是不够的：一个只写了库、没写存储的实现同样不报错，
// 而它产出的正是那种第二天打不开的卡片。
func TestTransferWritesBytesBeforeReturningAsset(t *testing.T) {
	ctx := context.Background()
	ra := &recordingAssets{}
	q := &countingQuota{}
	drv := &artifactDriver{bodies: map[string]string{"u://1": "video-bytes"}}
	tr, blobs := newTestTransferor(t, ra, q, drv)

	a, err := tr.Transfer(ctx, "u1", "task-1",
		adapter.ArtifactRef{Kind: adapter.KindURL, Type: domain.AssetTypeVideo, MIME: "video/mp4", URL: "u://1"},
		videoReq())
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	if a.StorageKey == "" {
		t.Fatal("资产必须带上本地存储键")
	}
	rc, info, err := blobs.Get(ctx, a.StorageKey)
	if err != nil {
		t.Fatalf("资产落库了但存储里没有字节：%v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "video-bytes" {
		t.Fatalf("存储里的字节不对：%q", got)
	}
	if a.Bytes != info.Bytes || a.Bytes != int64(len("video-bytes")) {
		t.Fatalf("Bytes 应等于实际落盘大小，asset=%d info=%d", a.Bytes, info.Bytes)
	}

	res, com, rel := q.counts()
	if res != 1 || com != 1 || rel != 0 {
		t.Fatalf("配额应预占并结算各一次、不释放，实际 res=%d com=%d rel=%d", res, com, rel)
	}
}

// TestTransferAllOrdersByIndex 验证产物按 Index 排序，那就是前端的展示顺序。
func TestTransferAllOrdersByIndex(t *testing.T) {
	ctx := context.Background()
	ra := &recordingAssets{}
	drv := &artifactDriver{bodies: map[string]string{"a": "A", "b": "BB", "c": "CCC"}}
	tr, _ := newTestTransferor(t, ra, nil, drv)

	refs := []adapter.ArtifactRef{
		{Kind: adapter.KindURL, Type: domain.AssetTypeImage, MIME: "image/png", URL: "c", Index: 2},
		{Kind: adapter.KindURL, Type: domain.AssetTypeImage, MIME: "image/png", URL: "a", Index: 0},
		{Kind: adapter.KindURL, Type: domain.AssetTypeImage, MIME: "image/png", URL: "b", Index: 1},
	}
	out, err := tr.TransferAll(ctx, "u1", "task-1", refs, videoReq())
	if err != nil {
		t.Fatalf("TransferAll: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("应有 3 件产物，实际 %d", len(out))
	}
	for i, want := range []int64{1, 2, 3} {
		if out[i].Bytes != want {
			t.Fatalf("第 %d 件的顺序不对：Bytes=%d want %d", i, out[i].Bytes, want)
		}
	}
}

// TestTransferAllRollsBackOnPartialFailure 验证"4 张里成了 3 张"不会留下来。
//
// 这条任务紧接着就要被判失败并退款；留下半套产物会让用户以为少的那张
// 是模型的问题，而账面上他一分钱没付。两边对不上比干脆没有更糟。
func TestTransferAllRollsBackOnPartialFailure(t *testing.T) {
	ctx := context.Background()
	ra := &recordingAssets{}
	drv := &artifactDriver{
		bodies: map[string]string{"a": "A", "b": "BB", "c": "CCC"},
		failOn: "c",
	}
	tr, _ := newTestTransferor(t, ra, nil, drv)

	refs := []adapter.ArtifactRef{
		{Kind: adapter.KindURL, Type: domain.AssetTypeImage, MIME: "image/png", URL: "a", Index: 0},
		{Kind: adapter.KindURL, Type: domain.AssetTypeImage, MIME: "image/png", URL: "b", Index: 1},
		{Kind: adapter.KindURL, Type: domain.AssetTypeImage, MIME: "image/png", URL: "c", Index: 2},
	}
	if _, err := tr.TransferAll(ctx, "u1", "task-1", refs, videoReq()); err == nil {
		t.Fatal("有一件取不到时整体应失败")
	}

	created, deleted := ra.snapshot()
	if len(created) != 2 {
		t.Fatalf("失败前落库了 2 件，实际 %d", len(created))
	}
	if len(deleted) != 2 {
		t.Fatalf("已落库的 2 件都应被回滚，实际删了 %d 件：%v", len(deleted), deleted)
	}
}

// TestTransferReleasesQuotaWhenPersistFails 验证落库失败时预占的配额被释放。
//
// 不释放的话，反复失败的任务会把用户的配额一点点吃光，而他的资产库里
// 一件东西都没多——那是一种查不出原因的"空间莫名其妙满了"。
func TestTransferReleasesQuotaWhenPersistFails(t *testing.T) {
	ctx := context.Background()
	failing := &alwaysFailAssets{}
	q := &countingQuota{}
	drv := &artifactDriver{bodies: map[string]string{"a": "A"}}
	tr, blobs := newTestTransferor(t, failing, q, drv)

	_, err := tr.Transfer(ctx, "u1", "task-1",
		adapter.ArtifactRef{Kind: adapter.KindURL, Type: domain.AssetTypeImage, MIME: "image/png", URL: "a"},
		videoReq())
	if err == nil {
		t.Fatal("落库失败时 Transfer 应报错")
	}

	res, com, rel := q.counts()
	if res != 1 || rel != 1 {
		t.Fatalf("预占后失败应释放，实际 res=%d rel=%d", res, rel)
	}
	if com != 0 {
		t.Fatalf("落库失败不该结算配额，实际 %d 次", com)
	}

	// 无主字节应被立刻删掉，而不是留给兜底清理。
	if failing.lastKey != "" {
		if _, err := blobs.Stat(ctx, failing.lastKey); err == nil {
			t.Fatal("落库失败后刚写的对象应被删除")
		}
	}
}

// alwaysFailAssets 的 Create 永远失败，并记下对方打算落库的存储键。
type alwaysFailAssets struct {
	fakeAssets
	lastKey string
}

func (a *alwaysFailAssets) Create(_ context.Context, as domain.Asset) (domain.Asset, error) {
	a.lastKey = as.StorageKey
	return domain.Asset{}, errors.New("落库失败")
}

// TestTransferRejectsQuotaExceededBeforeDownload 验证配额检查在下载之前。
//
// 产物一旦落盘字节就已经占掉了，那时再发现超配额只能删掉重来，
// 白白浪费一次上游生成的钱和一次带宽。
func TestTransferRejectsQuotaExceededBeforeDownload(t *testing.T) {
	ctx := context.Background()
	ra := &recordingAssets{}
	q := &countingQuota{reserveErr: &domain.Error{Code: domain.CodeQuotaExceeded, Message: "满了"}}
	drv := &artifactDriver{bodies: map[string]string{"a": "A"}}
	tr, _ := newTestTransferor(t, ra, q, drv)

	_, err := tr.Transfer(ctx, "u1", "task-1",
		adapter.ArtifactRef{Kind: adapter.KindURL, Type: domain.AssetTypeImage, MIME: "image/png", URL: "a"},
		videoReq())

	var derr *domain.Error
	if !errors.As(err, &derr) || derr.Code != domain.CodeQuotaExceeded {
		t.Fatalf("应返回 quota_exceeded，实际 %v", err)
	}
	if created, _ := ra.snapshot(); len(created) != 0 {
		t.Fatalf("超配额时不该落库任何资产，实际 %d 件", len(created))
	}
}

// TestTransferAllRejectsEmpty 验证"报告成功却没有产物"被当作错误。
func TestTransferAllRejectsEmpty(t *testing.T) {
	ra := &recordingAssets{}
	tr, _ := newTestTransferor(t, ra, nil, &artifactDriver{})
	if _, err := tr.TransferAll(context.Background(), "u1", "task-1", nil, videoReq()); err == nil {
		t.Fatal("空产物列表应报错，否则会留下一条没有任何产物的 succeeded 任务")
	}
}
