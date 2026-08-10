package executor

import (
	"context"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
)

// resolveOne 的顺序（先 asset 后 upload）是提交侧 httpapi.assertInputRefs
// 的对照物：两边必须一致，否则同一个 id 在校验时是 A、在执行时是 B。
//
// 这里断的是**执行器不会为一个已有 asset 走提升**——提升会写一行新的 assets
// 并累加一次 users.storage_used_bytes，那正是 DEM-99 要消灭的那份重复。

// stubAssets 是只实现 Get 的资产仓储，其余方法一次都不会被调到。
type stubAssets struct {
	store.AssetRepo
	byID map[string]domain.Asset
}

func (s stubAssets) Get(_ context.Context, id string) (domain.Asset, error) {
	if a, ok := s.byID[id]; ok {
		return a, nil
	}
	return domain.Asset{}, &domain.Error{Code: domain.CodeNotFound, Message: "no asset"}
}

// countingTransferor 只数 Promote 被调了几次。
type countingTransferor struct {
	promoted []string
	out      domain.Asset
}

func (t *countingTransferor) Transfer(context.Context, string, string, adapter.ArtifactRef, adapter.PollRequest) (domain.Asset, error) {
	return domain.Asset{}, nil
}

func (t *countingTransferor) TransferAll(context.Context, string, string, []adapter.ArtifactRef, adapter.PollRequest) ([]domain.Asset, error) {
	return nil, nil
}

func (t *countingTransferor) Promote(_ context.Context, uploadID string) (domain.Asset, error) {
	t.promoted = append(t.promoted, uploadID)
	return t.out, nil
}

func TestResolveOnePrefersExistingAsset(t *testing.T) {
	existing := domain.Asset{
		ID: "asset-1", UserID: "u1", Type: domain.AssetTypeImage,
		StorageKey: "assets/first-frame.png", MIME: "image/png", Bytes: 4096,
	}
	tr := &countingTransferor{out: domain.Asset{ID: "asset-promoted"}}
	s := &Service{deps: Deps{
		Assets:     stubAssets{byID: map[string]domain.Asset{existing.ID: existing}},
		Transferor: tr,
	}}

	got, err := s.resolveOne(context.Background(), existing.ID)
	if err != nil {
		t.Fatalf("resolveOne(%s): %v", existing.ID, err)
	}
	if got.ID != existing.ID || got.StorageKey != existing.StorageKey {
		t.Errorf("解析到 %s(%s)，want %s(%s)", got.ID, got.StorageKey, existing.ID, existing.StorageKey)
	}
	if len(tr.promoted) != 0 {
		t.Errorf("已有 asset 不该走提升，却提升了 %v——那会多写一行 assets 并重复计一次配额", tr.promoted)
	}

	// 不是 asset 的 id 仍然回落到 upload 提升，老路径没被改坏。
	if _, err := s.resolveOne(context.Background(), "upload-1"); err != nil {
		t.Fatalf("resolveOne(upload-1): %v", err)
	}
	if len(tr.promoted) != 1 || tr.promoted[0] != "upload-1" {
		t.Errorf("upload 分支没走提升：promoted=%v", tr.promoted)
	}
}
