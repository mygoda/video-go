package asset

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
)

func at() time.Time { return time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC) }

// TestBuildAssetKeyNeverEscapesRoot 是这个包里安全性最要紧的一条断言。
//
// 存储键**永远不由用户文本拼成**，但 user_id / asset_id 仍是外部传进来的字符串。
// 这里把各种路径穿越、绝对路径、控制字符喂进去，要求产出的键在
// filepath.Clean 之后仍然落在 users/ 之下——不是"通常如此"，是结构上不可能不如此。
func TestBuildAssetKeyNeverEscapesRoot(t *testing.T) {
	evil := []string{
		"../../../etc/passwd",
		"..",
		"....//....//etc",
		"/absolute/path",
		"a/b/c",
		"..\\..\\windows",
		"with space",
		"\x00null",
		"名字",
		strings.Repeat("A", 5000),
		"",
		".",
		"./.",
	}

	for _, uid := range evil {
		for _, aid := range evil {
			key := BuildAssetKey(uid, aid, VariantOriginal, "image/png", at())

			if strings.Contains(key, "..") {
				t.Fatalf("键里出现 ..：uid=%q aid=%q → %q", uid, aid, key)
			}
			if strings.HasPrefix(key, "/") {
				t.Fatalf("键不该是绝对路径：uid=%q aid=%q → %q", uid, aid, key)
			}
			if strings.ContainsRune(key, 0) {
				t.Fatalf("键里出现 NUL：uid=%q aid=%q → %q", uid, aid, key)
			}
			if !strings.HasPrefix(key, "users/") {
				t.Fatalf("键应落在 users/ 下：uid=%q aid=%q → %q", uid, aid, key)
			}
			// Clean 之后仍在 users/ 之下才算真的关得住。
			if c := filepath.Clean(key); !strings.HasPrefix(c, "users/") {
				t.Fatalf("Clean 后逃出 users/：%q → %q", key, c)
			}
		}
	}
}

// TestBuildUploadKeyNeverEscapesRoot 对 upload 键做同样的断言。
func TestBuildUploadKeyNeverEscapesRoot(t *testing.T) {
	for _, bad := range []string{"../../x", "/etc/passwd", "..", "a/b"} {
		key := BuildUploadKey(bad, bad, "image/jpeg", at())
		if strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
			t.Fatalf("upload 键不安全：%q", key)
		}
		if !strings.HasPrefix(key, "uploads/") {
			t.Fatalf("upload 键应落在 uploads/ 下：%q", key)
		}
	}
}

// TestBuildAssetKeyIsDeterministicAndVariantScoped 验证同一资产的三档
// 落在同一目录下的不同文件名——这让"删一件资产"变成删一个目录。
func TestBuildAssetKeyIsDeterministicAndVariantScoped(t *testing.T) {
	orig := BuildAssetKey("u1", "a1", VariantOriginal, "video/mp4", at())
	thumb := BuildAssetKey("u1", "a1", VariantThumb512, "image/jpeg", at())
	poster := BuildAssetKey("u1", "a1", VariantPoster, "image/jpeg", at())

	if orig != BuildAssetKey("u1", "a1", VariantOriginal, "video/mp4", at()) {
		t.Fatal("同样输入应给出同样的键")
	}
	if orig == thumb || thumb == poster || orig == poster {
		t.Fatalf("三档不能撞键：%q %q %q", orig, thumb, poster)
	}
	dir := filepath.Dir(orig)
	if filepath.Dir(thumb) != dir || filepath.Dir(poster) != dir {
		t.Fatalf("三档应同目录：%q %q %q", orig, thumb, poster)
	}
	if !strings.HasSuffix(orig, ".mp4") {
		t.Fatalf("扩展名应由 MIME 推出，got %q", orig)
	}
}

// TestContentURL 验证对外地址的拼法。
func TestContentURL(t *testing.T) {
	got := ContentURL("https://x.test", "a1", VariantThumb512)
	if !strings.HasPrefix(got, "https://x.test/") {
		t.Fatalf("应以 base 开头：%q", got)
	}
	if !strings.Contains(got, "a1") || !strings.Contains(got, string(VariantThumb512)) {
		t.Fatalf("应带上资产 id 与档位：%q", got)
	}
	// base 末尾多一个斜杠不该产生 //。
	if strings.Contains(strings.TrimPrefix(ContentURL("https://x.test/", "a1", VariantOriginal), "https://"), "//") {
		t.Fatal("base 末尾斜杠应被规整掉")
	}
}

// ── FS Store ──────────────────────────────────────────────────────────

func newTestStore(t *testing.T) Store {
	t.Helper()
	s, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	return s
}

// TestStorePutGetRoundTrip 验证写读往返与 ObjectInfo。
func TestStorePutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	body := []byte("hello-artifact-bytes")
	info, err := s.Put(ctx, "users/u1/2026/03/a1/original.png", bytes.NewReader(body), "image/png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if info.Bytes != int64(len(body)) {
		t.Fatalf("Bytes = %d, want %d", info.Bytes, len(body))
	}

	rc, got, err := s.Get(ctx, "users/u1/2026/03/a1/original.png")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	read, _ := io.ReadAll(rc)
	if !bytes.Equal(read, body) {
		t.Fatalf("读回的字节不一致：%q", read)
	}
	if got.Bytes != int64(len(body)) {
		t.Fatalf("Get 的 Bytes = %d", got.Bytes)
	}
}

// TestStoreRejectsTraversalKeys 验证 Store 这一层独立地拒绝穿越键。
//
// 键构造那一层已经做了白名单，这里是第二道：即便调用方绕过 BuildAssetKey
// 自己拼了一个键，也进不去根目录之外。
func TestStoreRejectsTraversalKeys(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := NewFSStore(root)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}

	// 在根目录旁边放一个"外部机密"，验证它读不到也写不着。
	outside := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("准备外部文件: %v", err)
	}
	defer os.Remove(outside)

	bad := []string{
		"../secret.txt",
		"../../secret.txt",
		"a/../../secret.txt",
		"/etc/passwd",
		"",
		"x\x00y",
		"./../secret.txt",
	}
	for _, key := range bad {
		if _, err := s.Put(ctx, key, strings.NewReader("x"), "text/plain"); err == nil {
			t.Errorf("Put(%q) 应被拒绝", key)
		}
		if _, _, err := s.Get(ctx, key); err == nil {
			t.Errorf("Get(%q) 应被拒绝", key)
		}
		if _, err := s.Stat(ctx, key); err == nil {
			t.Errorf("Stat(%q) 应被拒绝", key)
		}
	}

	if b, _ := os.ReadFile(outside); string(b) != "secret" {
		t.Fatal("根目录之外的文件被改写了")
	}
}

// TestStoreGetRange 验证分段读，视频拖动进度条靠它。
func TestStoreGetRange(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	body := []byte("0123456789")
	if _, err := s.Put(ctx, "users/u1/2026/03/a1/original.mp4", bytes.NewReader(body), "video/mp4"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, info, err := s.GetRange(ctx, "users/u1/2026/03/a1/original.mp4", 3, 4)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "3456" {
		t.Fatalf("分段内容 = %q, want %q", got, "3456")
	}
	// ObjectInfo 描述的是整个对象，不是这一段——Content-Range 的分母要用它。
	if info.Bytes != 10 {
		t.Fatalf("ObjectInfo.Bytes 应是整体大小 10，实际 %d", info.Bytes)
	}
}

// TestStoreDeleteIsIdempotent 验证删不存在的对象不算错。
//
// 转存回滚、清理任务重跑都会重复删同一个键；把"已经没了"当成错误，
// 会让这些收尾路径卡在一个本该无操作的地方。
func TestStoreDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	key := "users/u1/2026/03/a1/original.png"

	if _, err := s.Put(ctx, key, strings.NewReader("x"), "image/png"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("首次 Delete: %v", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("重复 Delete 不该报错：%v", err)
	}
}

// TestStoreStatNotFound 验证缺失对象归到 not_found 而不是内部错误。
func TestStoreStatNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Stat(context.Background(), "users/u1/2026/03/none/original.png")
	var derr *domain.Error
	if !errors.As(err, &derr) || derr.Code != domain.CodeNotFound {
		t.Fatalf("应为 not_found，实际 %v", err)
	}
}

// TestStorePutIsAtomic 验证 Put 之后不会留下半截文件。
//
// 一个 Stat 得出来、读起来却被截断的对象，比一个干脆不存在的对象糟得多：
// 后者会走转存失败的重试路径，前者会被当成完好产物交付给用户。
func TestStorePutIsAtomic(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := NewFSStore(root)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}

	key := "users/u1/2026/03/a1/original.bin"
	// 读到一半就报错的 reader，模拟转存途中上游断流。
	r := io.MultiReader(strings.NewReader("head"), errReader{errors.New("上游断流")})
	if _, err := s.Put(ctx, key, r, "application/octet-stream"); err == nil {
		t.Fatal("读流失败时 Put 应报错")
	}

	// 失败后既不该留下目标对象，也不该留下临时文件。
	if _, err := s.Stat(ctx, key); err == nil {
		t.Fatal("失败的 Put 不该留下可 Stat 的对象")
	}
	var leftovers []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d != nil && !d.IsDir() {
			leftovers = append(leftovers, p)
		}
		return nil
	})
	if len(leftovers) != 0 {
		t.Fatalf("失败的 Put 留下了残留文件：%v", leftovers)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// ── Lineage ──────────────────────────────────────────────────────────

// TestInputEdges 验证输入边是「每个输入 × 每个产物」的笛卡尔积。
func TestInputEdges(t *testing.T) {
	edges := InputEdges([]string{"in1", "in2"}, []domain.Asset{{ID: "out1"}, {ID: "out2"}})
	if len(edges) != 4 {
		t.Fatalf("应有 4 条边，实际 %d：%v", len(edges), edges)
	}
	for _, e := range edges {
		if e.Relation != domain.LineageInput {
			t.Fatalf("关系应为 input，实际 %s", e.Relation)
		}
	}
	if len(InputEdges(nil, []domain.Asset{{ID: "o"}})) != 0 {
		t.Fatal("没有输入时不该产生边")
	}
	if len(InputEdges([]string{"i"}, nil)) != 0 {
		t.Fatal("没有产物时不该产生边")
	}
}

// TestRerunAndComposedEdges 验证另外两种关系。
func TestRerunAndComposedEdges(t *testing.T) {
	r := RerunEdges("old", []domain.Asset{{ID: "new"}})
	if len(r) != 1 || r[0].Relation != domain.LineageRerunOf {
		t.Fatalf("rerun 边不对：%v", r)
	}
	c := ComposedEdges([]string{"seg1", "seg2"}, domain.Asset{ID: "film"})
	if len(c) != 2 {
		t.Fatalf("composed 边应有 2 条，实际 %v", c)
	}
	for _, e := range c {
		if e.Relation != domain.LineageComposedFrom {
			t.Fatalf("关系应为 composed_from，实际 %s", e.Relation)
		}
	}
}

// fakeAssets 记下 AddLineage 收到了什么，并用一个可编程的图回答 Lineage。
type fakeAssets struct {
	recorded []domain.LineageEdge
	graph    domain.LineageGraph
	lastDep  int
}

func (f *fakeAssets) AddLineage(_ context.Context, edges []domain.LineageEdge) error {
	f.recorded = append(f.recorded, edges...)
	return nil
}

func (f *fakeAssets) Lineage(_ context.Context, _ string, _ domain.LineageDirection, depth int) (domain.LineageGraph, error) {
	f.lastDep = depth
	return f.graph, nil
}

func (f *fakeAssets) Create(_ context.Context, a domain.Asset) (domain.Asset, error) { return a, nil }
func (f *fakeAssets) Get(context.Context, string) (domain.Asset, error) {
	return domain.Asset{}, &domain.Error{Code: domain.CodeNotFound}
}
func (f *fakeAssets) List(context.Context, string, domain.AssetType, string, string, int) (store.Page[domain.Asset], error) {
	return store.Page[domain.Asset]{}, nil
}
func (f *fakeAssets) ListByTask(context.Context, string) ([]domain.Asset, error) { return nil, nil }
func (f *fakeAssets) Delete(context.Context, string) error                       { return nil }
func (f *fakeAssets) SumBytes(context.Context, string) (int64, int, error)       { return 0, 0, nil }
func (f *fakeAssets) SetVariants(context.Context, string, *string, *string) error {
	return nil
}
func (f *fakeAssets) SetMedia(context.Context, string, *int, *int, *int) error { return nil }
func (f *fakeAssets) ListMissingMedia(context.Context, int) ([]domain.Asset, error) {
	return nil, nil
}
func (f *fakeAssets) ListSoftDeleted(context.Context, time.Time, int) ([]domain.Asset, error) {
	return nil, nil
}
func (f *fakeAssets) HardDelete(context.Context, string) error { return nil }
func (f *fakeAssets) StorageUsage(context.Context, int) (domain.StorageUsage, error) {
	return domain.StorageUsage{}, nil
}

// TestLineageRecorderDedupesAndDropsSelfLoops 验证脏边被挡在落库之前。
//
// 自环（自己指向自己）会让血缘查询的 BFS 原地打转；重复边会让「做同款」
// 的候选列表里同一件素材出现好几次。
func TestLineageRecorderDedupesAndDropsSelfLoops(t *testing.T) {
	fa := &fakeAssets{}
	rec, err := NewLineageRecorder(LineageDeps{Assets: fa})
	if err != nil {
		t.Fatalf("NewLineageRecorder: %v", err)
	}

	err = rec.Record(context.Background(), []domain.LineageEdge{
		{From: "a", To: "b", Relation: domain.LineageInput},
		{From: "a", To: "b", Relation: domain.LineageInput}, // 重复
		{From: "a", To: "a", Relation: domain.LineageInput}, // 自环
		{From: "", To: "b", Relation: domain.LineageInput},  // 缺端点
		{From: "a", To: "c", Relation: "编造的关系"},             // 未知关系
		{From: "a", To: "c", Relation: domain.LineageRerunOf},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	if len(fa.recorded) != 2 {
		t.Fatalf("应只落库 2 条干净的边，实际 %d：%v", len(fa.recorded), fa.recorded)
	}
	for _, e := range fa.recorded {
		if e.From == e.To || e.From == "" || e.To == "" {
			t.Fatalf("落库了脏边：%v", e)
		}
	}
}

// TestLineageDepthIsClamped 验证深度被夹在 [1, MaxLineageDepth]。
//
// 血缘图可能有环（A 重跑成 B，B 又被用作 A 的输入），不封顶的遍历会走不完。
func TestLineageDepthIsClamped(t *testing.T) {
	fa := &fakeAssets{}
	rec, _ := NewLineageRecorder(LineageDeps{Assets: fa})
	ctx := context.Background()

	for _, in := range []int{-3, 0} {
		if _, err := rec.Graph(ctx, "a1", domain.LineageBoth, in); err != nil {
			t.Fatalf("Graph: %v", err)
		}
		if fa.lastDep != 1 {
			t.Fatalf("depth=%d 应被夹到 1，实际传下去 %d", in, fa.lastDep)
		}
	}
	if _, err := rec.Graph(ctx, "a1", domain.LineageBoth, 999); err != nil {
		t.Fatalf("Graph: %v", err)
	}
	if fa.lastDep != MaxLineageDepth {
		t.Fatalf("超限深度应被夹到 %d，实际 %d", MaxLineageDepth, fa.lastDep)
	}
}
