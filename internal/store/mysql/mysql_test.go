package mysql

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// testDSNEnv 是集成测试的开关。
//
// **没设它时全部集成测试 t.Skip，而不是失败。** `go test ./...` 必须在一台
// 没有 MySQL 的机器上跑得通（CI 的 lint 阶段、同事第一次 clone 下来），
// 否则"跑一下测试"这件事就先卡在装数据库上。
const testDSNEnv = "AIGC_TEST_MYSQL_DSN"

// testDBHandle 是整个测试包共用的连接池。
//
// 每个测试各开一个池会在几十个用例后把 MySQL 的 max_connections 吃光——
// database/sql 的池不会因为 *sql.DB 被丢弃就立刻还连接。
var testDBHandle *sql.DB

func TestMain(m *testing.M) {
	if dsn := os.Getenv(testDSNEnv); dsn != "" {
		db, err := Open(dsn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mysql: cannot open %s: %v\n", testDSNEnv, err)
			os.Exit(1)
		}
		testDBHandle = db
	}
	code := m.Run()
	if testDBHandle != nil {
		_ = testDBHandle.Close()
	}
	os.Exit(code)
}

// requireDB 取共用连接池，没配 DSN 就跳过当前用例。
func requireDB(t *testing.T) *sql.DB {
	t.Helper()
	if testDBHandle == nil {
		t.Skipf("%s is not set; skipping MySQL integration test", testDSNEnv)
	}
	return testDBHandle
}

// codeOf 取出 domain.Error 的错误码，便于断言"是哪一类错"而不是比字符串。
func codeOf(err error) domain.ErrorCode {
	var de *domain.Error
	if err == nil {
		return ""
	}
	if !asDomainError(err, &de) {
		return ""
	}
	return de.Code
}

func asDomainError(err error, target **domain.Error) bool {
	for err != nil {
		if de, ok := err.(*domain.Error); ok {
			*target = de
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func requireCode(t *testing.T, err error, want domain.ErrorCode) {
	t.Helper()
	if got := codeOf(err); got != want {
		t.Fatalf("want error code %q, got %q (err=%v)", want, got, err)
	}
}

func requireNoErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// ── 固件 ─────────────────────────────────────────────────────────────
//
// 每个用例造自己的用户 / 供应商 / 模型，用随机后缀避免撞唯一键，
// 并在 t.Cleanup 里按外键顺序删干净：留下垃圾会让 Stats / StorageUsage
// 这类**全表聚合**的断言在第二次运行时失败。

type fixture struct {
	db         *sql.DB
	userID     string
	providerID string
	modelID    string
}

func newFixture(t *testing.T, credits int, quota int64) *fixture {
	t.Helper()
	db := requireDB(t)
	ctx := context.Background()

	f := &fixture{db: db}

	users := NewUserRepo(db)
	u, err := users.Create(ctx, domain.User{
		Username:     "test_" + uid.Token(8),
		PasswordHash: "argon2id$test",
		Credits:      credits,
		StorageQuota: quota,
	})
	requireNoErr(t, err, "create test user")
	f.userID = u.ID

	providers := NewProviderRepo(db)
	p, err := providers.Upsert(ctx, domain.Provider{
		Name:          "test-provider-" + uid.Token(8),
		BaseURL:       "https://example.invalid",
		CredentialRef: "AIGC_TEST_CREDENTIAL",
		Enabled:       true,
	})
	requireNoErr(t, err, "create test provider")
	f.providerID = p.ID

	models := NewModelRepo(db)
	modelID := "test-model-" + uid.Token(8)
	m, err := models.Upsert(ctx, testModelConfig(modelID, p.ID))
	requireNoErr(t, err, "create test model")
	f.modelID = m.ID

	t.Cleanup(func() { f.drop(t) })
	return f
}

// newUserNamed 只造一个用户，名字带指定前缀。
//
// 分页用例要把"这一批"用户和库里已有的数据分开，靠的就是这个前缀 + List 的 q。
// 它不走 newFixture 是因为那还会顺手造供应商和模型，五个用户就是五套，
// 纯属浪费；清理也只需要删这一行。
func newUserNamed(t *testing.T, usernamePrefix string) domain.User {
	t.Helper()
	db := requireDB(t)
	ctx := context.Background()

	u, err := NewUserRepo(db).Create(ctx, domain.User{
		Username:     usernamePrefix + "_" + uid.Token(8),
		PasswordHash: "argon2id$test",
		StorageQuota: 1 << 30,
	})
	requireNoErr(t, err, "create named test user")
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM users WHERE id = ?`, u.ID)
	})
	return u
}

func testModelConfig(modelID, providerID string) domain.ModelConfig {
	return domain.ModelConfig{
		ID:            modelID,
		ProviderID:    providerID,
		UpstreamModel: "upstream-" + modelID,
		Family:        domain.FamilyMock,
		Enabled:       true,
		// mock 族只能是 internal（仓储与库里的 CHECK 各拦一道）。
		Visibility: domain.VisibilityInternal,
		Capability: map[string]any{
			"id":       modelID,
			"name":     "Test Model",
			"vendor":   "test",
			"modality": "image",
			"order":    1,
		},
	}
}

// drop 按外键顺序清掉固件造出来的全部数据。
//
// 顺序是被外键定死的：uploads 指向 assets（SET NULL 也要先删，否则
// assets 的 RESTRICT 拦不住的那部分会留下悬空引用），assets 与
// credit_ledger 指向 tasks，tasks 指向 users / models / providers。
func (f *fixture) drop(t *testing.T) {
	t.Helper()
	stmts := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM uploads WHERE user_id = ?`, []any{f.userID}},
		{`DELETE FROM asset_lineage
		  WHERE parent_asset_id IN (SELECT id FROM assets WHERE user_id = ?)
		     OR child_asset_id  IN (SELECT id FROM assets WHERE user_id = ?)`, []any{f.userID, f.userID}},
		{`DELETE FROM assets WHERE user_id = ?`, []any{f.userID}},
		{`DELETE FROM task_events WHERE user_id = ?`, []any{f.userID}},
		{`DELETE FROM credit_ledger WHERE user_id = ?`, []any{f.userID}},
		{`DELETE FROM projects WHERE user_id = ?`, []any{f.userID}},
		{`DELETE FROM tasks WHERE user_id = ?`, []any{f.userID}},
		{`DELETE FROM users WHERE id = ?`, []any{f.userID}},
		{`DELETE FROM models WHERE provider_id = ?`, []any{f.providerID}},
		{`DELETE FROM providers WHERE id = ?`, []any{f.providerID}},
	}
	for _, s := range stmts {
		if _, err := f.db.ExecContext(context.Background(), s.query, s.args...); err != nil {
			t.Logf("cleanup %q: %v", s.query, err)
		}
	}
}

// newTask 造一条 queued 任务。
func (f *fixture) newTask(t *testing.T, estimated int) domain.Task {
	t.Helper()
	task, err := NewTaskRepo(f.db).Create(context.Background(), domain.Task{
		UserID:        f.userID,
		ModelID:       f.modelID,
		ProviderID:    f.providerID,
		Prompt:        "a test prompt",
		Params:        map[string]any{"aspect": "1:1"},
		EstimatedCost: estimated,
	})
	requireNoErr(t, err, "create task")
	return task
}

// newAsset 造一件资产。
func (f *fixture) newAsset(t *testing.T, bytes int64) domain.Asset {
	t.Helper()
	a, err := NewAssetRepo(f.db).Create(context.Background(), domain.Asset{
		UserID:     f.userID,
		Type:       domain.AssetTypeImage,
		StorageKey: "test/" + uid.Token(8) + ".png",
		MIME:       "image/png",
		Bytes:      bytes,
		Source:     &domain.AssetSource{ModelID: f.modelID, Prompt: "p"},
	})
	requireNoErr(t, err, "create asset")
	return a
}

// ── 不依赖数据库的用例 ────────────────────────────────────────────────

func TestClampLimit(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, defaultLimit}, {-5, defaultLimit}, {1, 1},
		{maxLimit, maxLimit}, {maxLimit + 1, maxLimit}, {1 << 20, maxLimit},
	}
	for _, c := range cases {
		if got := clampLimit(c.in); got != c.want {
			t.Errorf("clampLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	at := time.UnixMilli(1_700_000_000_123).UTC()
	raw := encodeTimeCursor(at, "01890000-0000-7000-8000-000000000001")
	got, ok := decodeCursor(raw)
	if !ok {
		t.Fatalf("decodeCursor(%q) reported failure", raw)
	}
	if !got.At.Equal(at) {
		t.Errorf("cursor time = %s, want %s", got.At, at)
	}
	if got.ID != "01890000-0000-7000-8000-000000000001" {
		t.Errorf("cursor id = %q", got.ID)
	}

	idOnly, ok := decodeCursor(encodeIDCursor("abc"))
	if !ok || !idOnly.At.IsZero() || idOnly.ID != "abc" {
		t.Errorf("id-only cursor round trip failed: %+v ok=%v", idOnly, ok)
	}
}

// TestCursorRejectsGarbage 钉住"垃圾游标降级成第一页而不是报错"这条规则。
func TestCursorRejectsGarbage(t *testing.T) {
	garbage := []string{
		"",
		"!!!not base64!!!",
		base64.RawURLEncoding.EncodeToString([]byte("foo")),         // 合法 base64 但不是本格式
		encodeCursor(cursor{}),                                      // 全零锚点
		base64.RawURLEncoding.EncodeToString([]byte("v2|1|abc|0")),  // 版本号不是 v1
		base64.RawURLEncoding.EncodeToString([]byte("v1|nope|x|0")), // 时间部分不是数字
		base64.RawURLEncoding.EncodeToString([]byte("v1|1|x|nope")), // seq 不是数字
		base64.RawURLEncoding.EncodeToString([]byte("v1|1|x")),      // 段数不足
	}
	for _, g := range garbage {
		if c, ok := decodeCursor(g); ok {
			t.Errorf("decodeCursor(%q) accepted garbage: %+v", g, c)
		}
	}
}

// TestNormalizeDSN 钉住三条连接参数规则，其中 multiStatements 那条是安全约束：
// 业务连接开着多语句等于给每条 SQL 接一根注入放大器。
func TestNormalizeDSN(t *testing.T) {
	got, err := normalizeDSN("u:p@tcp(127.0.0.1:3306)/db?multiStatements=true")
	requireNoErr(t, err, "normalize dsn")

	for _, want := range []string{"parseTime=true", "loc=UTC", "charset=utf8mb4", "time_zone=%27%2B00%3A00%27"} {
		if !contains(got, want) {
			t.Errorf("normalized DSN %q is missing %q", got, want)
		}
	}
	if contains(got, "multiStatements=true") {
		t.Errorf("normalized DSN must never enable multiStatements: %q", got)
	}

	// 归一化的结果必须能再被驱动解析回来：这串 DSN 会被日志/配置复用，
	// 它自己解释不了自己就等于没归一化。
	if _, err := normalizeDSN(got); err != nil {
		t.Errorf("normalized DSN does not round trip: %v", err)
	}

	if _, err := normalizeDSN("this is not a dsn"); err == nil {
		t.Error("normalizeDSN accepted a malformed DSN")
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
