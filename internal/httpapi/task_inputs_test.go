package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/capability"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store/mysql"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// 本文件守的是一条契约，不是一个函数：**inputs 里的值既可以是 upload_id，
// 也可以是已有的 asset_id。**
//
// domain.Task.Inputs 的注释一直这么写，executor.resolveOne 也一直这么实现，
// 唯独提交这一关只查 uploads——于是「拿画布上已有的图当首帧」在提交那一刻就是
// 400。绕过去的唯一办法是把资产的字节取回来重新上传一遍，代价是同一份图在存储里
// 躺好几份、各计一次配额，任务的血缘边还指向一个用户从没见过的副本 id。
//
// 因此这些用例断的是「提交进去之后库里发生了什么」，不是「handler 调了哪个仓储」：
// 关键证据是 assets 表的行数**没有增加**，以及落库的 inputs 里存的就是原 asset id。

// assetInputsDSNEnv 与 store/mysql 的集成测试用同一个开关。
//
// 没设它时 t.Skip 而不是失败：`go test ./...` 必须在一台没有 MySQL 的机器上
// 跑得通，否则"跑一下测试"这件事就先卡在装数据库上。
const assetInputsDSNEnv = "AIGC_TEST_MYSQL_DSN"

// inputsFixture 是一套连着真库的最小提交环境：两个用户（用来验归属）、
// 一个带 first_frame 槽的模型，以及一个直接调 submitTask 的 server。
//
// 走 submitTask 而不是 HTTP：被测的是提交这一关的校验与落库，
// 套一层认证中间件只会把失败点从"校验错了"挪到"令牌签错了"。
type inputsFixture struct {
	db      *sql.DB
	srv     *server
	userID  string
	otherID string
	modelID string
	provID  string
}

func newInputsFixture(t *testing.T) *inputsFixture {
	t.Helper()
	dsn := os.Getenv(assetInputsDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set; skipping MySQL integration test", assetInputsDSNEnv)
	}
	db, err := mysql.Open(dsn)
	if err != nil {
		t.Fatalf("open %s: %v", assetInputsDSNEnv, err)
	}
	ctx := context.Background()
	f := &inputsFixture{db: db}

	users := mysql.NewUserRepo(db)
	owner, err := users.Create(ctx, domain.User{
		Username: "dem99_owner_" + uid.Token(8), PasswordHash: "argon2id$test",
		Credits: 1000, StorageQuota: 1 << 30,
	})
	requireNoTestErr(t, err, "create owner")
	f.userID = owner.ID

	other, err := users.Create(ctx, domain.User{
		Username: "dem99_other_" + uid.Token(8), PasswordHash: "argon2id$test",
		Credits: 1000, StorageQuota: 1 << 30,
	})
	requireNoTestErr(t, err, "create other user")
	f.otherID = other.ID

	prov, err := mysql.NewProviderRepo(db).Upsert(ctx, domain.Provider{
		Name: "dem99-provider-" + uid.Token(8), BaseURL: "https://example.invalid",
		CredentialRef: "AIGC_TEST_CREDENTIAL", Enabled: true,
	})
	requireNoTestErr(t, err, "create provider")
	f.provID = prov.ID

	modelID := "dem99-model-" + uid.Token(8)
	m, err := mysql.NewModelRepo(db).Upsert(ctx, domain.ModelConfig{
		ID: modelID, ProviderID: prov.ID, UpstreamModel: "upstream-" + modelID,
		Family: domain.FamilyMock, Enabled: true,
		// mock 族只能是 internal（仓储与库里的 CHECK 各拦一道）。
		Visibility: domain.VisibilityInternal,
		Capability: firstFrameCapability(modelID),
	})
	requireNoTestErr(t, err, "create model")
	f.modelID = m.ID

	eval := capability.NewEvaluator()
	f.srv = &server{deps: Deps{
		Users:     users,
		Models:    mysql.NewModelRepo(db),
		Tasks:     mysql.NewTaskRepo(db),
		Uploads:   mysql.NewUploadRepo(db),
		Assets:    mysql.NewAssetRepo(db),
		Evaluator: eval,
		Pricer:    capability.NewPricer(eval),
		Validator: capability.NewValidator(eval),
		Ledger:    mysql.NewLedger(db),
	}}

	t.Cleanup(func() { f.drop(t); _ = db.Close() })
	return f
}

// firstFrameCapability 是一份最小可用的能力声明，只声明本票关心的那一个槽。
// min_pixels 300×300 与 DEM-94 验过的那条尺寸下限用例保持一致。
func firstFrameCapability(modelID string) map[string]any {
	return map[string]any{
		"id": modelID, "name": "DEM-99 Test Model", "vendor": "test",
		"modality": "video", "enabled": true, "order": 1,
		"inputs": []any{map[string]any{
			"key": "first_frame", "label": "首帧", "kind": "image",
			"required": false, "min_count": 0, "max_count": 1,
			"accept":     []any{"image/png", "image/jpeg"},
			"max_bytes":  8 << 20,
			"min_pixels": map[string]any{"width": 300, "height": 300},
		}},
		"params":  []any{},
		"pricing": map[string]any{"currency": "credit", "base": 1, "rounding": "ceil"},
		"eta":     map[string]any{"p50_seconds": 10, "p90_seconds": 20},
		"limits":  map[string]any{"max_concurrent_per_user": 0, "prompt_max_length": 0},
	}
}

// drop 按外键顺序清掉固件造出来的全部数据，两个用户都要清。
func (f *inputsFixture) drop(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, userID := range []string{f.userID, f.otherID} {
		for _, q := range []string{
			`DELETE FROM uploads WHERE user_id = ?`,
			`DELETE FROM asset_lineage
			 WHERE parent_asset_id IN (SELECT id FROM assets WHERE user_id = ?)
			    OR child_asset_id  IN (SELECT id FROM assets WHERE user_id = ?)`,
			`DELETE FROM assets WHERE user_id = ?`,
			`DELETE FROM task_events WHERE user_id = ?`,
			`DELETE FROM credit_ledger WHERE user_id = ?`,
			`DELETE FROM projects WHERE user_id = ?`,
			`DELETE FROM tasks WHERE user_id = ?`,
			`DELETE FROM users WHERE id = ?`,
		} {
			if _, err := f.db.ExecContext(ctx, q, repeatArg(userID, placeholders(q))...); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	}
	for _, q := range []string{
		`DELETE FROM models WHERE provider_id = ?`,
		`DELETE FROM providers WHERE id = ?`,
	} {
		if _, err := f.db.ExecContext(ctx, q, f.provID); err != nil {
			t.Logf("cleanup %q: %v", q, err)
		}
	}
}

func placeholders(query string) int {
	return strings.Count(query, "?")
}

func repeatArg(v string, n int) []any {
	args := make([]any, n)
	for i := range args {
		args[i] = v
	}
	return args
}

// newAsset 造一件属于 userID 的图片资产。
func (f *inputsFixture) newAsset(t *testing.T, userID string, w, h int) domain.Asset {
	t.Helper()
	a, err := mysql.NewAssetRepo(f.db).Create(context.Background(), domain.Asset{
		UserID: userID, Type: domain.AssetTypeImage,
		StorageKey: "dem99/" + uid.Token(8) + ".png",
		MIME:       "image/png", Bytes: 4096, Width: &w, Height: &h,
		Source: &domain.AssetSource{ModelID: f.modelID, Prompt: "首帧"},
	})
	requireNoTestErr(t, err, "create asset")
	return a
}

// newUpload 造一件属于 userID 的上传对象。
func (f *inputsFixture) newUpload(t *testing.T, userID string, w, h int) domain.Upload {
	t.Helper()
	u, err := mysql.NewUploadRepo(f.db).Create(context.Background(), domain.Upload{
		UserID: userID, Slot: "first_frame",
		StorageKey: "dem99/" + uid.Token(8) + ".png",
		MIME:       "image/png", Bytes: 4096, Width: &w, Height: &h,
	})
	requireNoTestErr(t, err, "create upload")
	return u
}

func (f *inputsFixture) countAssets(t *testing.T) int {
	t.Helper()
	var n int
	err := f.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM assets WHERE user_id = ?`, f.userID).Scan(&n)
	requireNoTestErr(t, err, "count assets")
	return n
}

func (f *inputsFixture) submit(userID, slotValue string) (domain.Task, error) {
	task, _, err := f.srv.submitTask(context.Background(), userID, createTaskRequest{
		ModelID:     f.modelID,
		Prompt:      "让首帧动起来",
		Inputs:      map[string][]string{"first_frame": {slotValue}},
		ClientToken: uid.Token(16),
	})
	return task, err
}

func requireNoTestErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// fieldErrorsOf 取出一次提交失败带回的字段错误，方便断言"错在哪个槽、说了什么"。
func fieldErrorsOf(err error) []domain.FieldError {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.FieldErrors
	}
	return nil
}

func fieldMessages(err error) string {
	return fmt.Sprint(fieldErrorsOf(err))
}

// TestSubmitAcceptsExistingAssetID 是本票的主验收：直接喂一个已有的 asset_id
// 必须提交成功，且**不产生任何新的 asset 行**。
//
// 行数这条断言就是整张票的经济账：旧行为下用户只能把资产取回来重传，
// 每提交一次就多一行 assets、多一次 users.storage_used_bytes 累加。
func TestSubmitAcceptsExistingAssetID(t *testing.T) {
	f := newInputsFixture(t)
	a := f.newAsset(t, f.userID, 1024, 576)

	before := f.countAssets(t)
	task, err := f.submit(f.userID, a.ID)
	if err != nil {
		t.Fatalf("用已有 asset_id 提交被拒了，这正是 DEM-99 要修的：%v / %s", err, fieldMessages(err))
	}
	after := f.countAssets(t)
	t.Logf("assets COUNT(*) for user %s: before=%d after=%d", f.userID, before, after)

	if before != after {
		t.Errorf("assets 行数 %d → %d：提交一个已有 asset 不该产生新资产", before, after)
	}
	if task.Status != domain.TaskStatusQueued {
		t.Errorf("task status = %q, want queued", task.Status)
	}

	// 落库的 inputs 必须还是那个原 asset id：执行器 resolveOne 拿它去查 assets，
	// 存成别的东西就等于血缘指向了一件用户没见过的资产。
	stored, err := mysql.NewTaskRepo(f.db).Get(context.Background(), task.ID)
	requireNoTestErr(t, err, "reload task")
	got := stored.Inputs["first_frame"]
	if len(got) != 1 || got[0] != a.ID {
		t.Fatalf("落库 inputs[first_frame] = %v, want [%s]", got, a.ID)
	}

	// 执行器解析这个 id 拿到的就是那一件原资产。
	resolved, err := mysql.NewAssetRepo(f.db).Get(context.Background(), got[0])
	requireNoTestErr(t, err, "resolve asset")
	if resolved.ID != a.ID || resolved.StorageKey != a.StorageKey {
		t.Errorf("解析到 %s(%s)，want %s(%s)", resolved.ID, resolved.StorageKey, a.ID, a.StorageKey)
	}
}

// TestSubmitStillAcceptsUploadID 钉住老路径没被新分支改坏。
func TestSubmitStillAcceptsUploadID(t *testing.T) {
	f := newInputsFixture(t)
	u := f.newUpload(t, f.userID, 1024, 576)

	if _, err := f.submit(f.userID, u.ID); err != nil {
		t.Fatalf("用 upload_id 提交被拒了：%v / %s", err, fieldMessages(err))
	}
}

// TestSubmitEnforcesSlotLimitsOnAssets 是最容易被偷掉的那条：
// 走 asset 分支时，按槽声明的文件约束必须一条不少地生效。
//
// 200×200 的**资产**喂进首帧槽，与 200×200 的上传对象一样要在提交那一刻被拒，
// 且报错要指出尺寸下限——否则换个来源就能绕过校验，用户为一张注定失败的图付钱。
func TestSubmitEnforcesSlotLimitsOnAssets(t *testing.T) {
	f := newInputsFixture(t)
	small := f.newAsset(t, f.userID, 200, 200)

	_, err := f.submit(f.userID, small.ID)
	if err == nil {
		t.Fatal("200×200 的 asset 喂进首帧槽居然过了，槽约束在 asset 分支漏掉了")
	}
	fields := fieldErrorsOf(err)
	if len(fields) == 0 {
		t.Fatalf("没有 field_errors，用户不知道该改哪个槽：%v", err)
	}
	if fields[0].Key != "first_frame" {
		t.Errorf("field_errors[0].Key = %q, want first_frame", fields[0].Key)
	}
	want := "尺寸 200×200 小于下限 300×300"
	if fields[0].Message != want {
		t.Errorf("field_errors[0].Message = %q, want %q", fields[0].Message, want)
	}
}

// TestSubmitHidesForeignAssetExistence 钉住归属校验，以及它的措辞。
//
// 拿别人的 asset_id 提交，报错必须与「这个 id 根本不存在」**逐字相同**：
// 只要两者有一个字不一样，拿 id 挨个试就能读出哪些 id 在库里真实存在。
func TestSubmitHidesForeignAssetExistence(t *testing.T) {
	f := newInputsFixture(t)
	foreign := f.newAsset(t, f.otherID, 1024, 576)
	ghost := uid.New()

	_, foreignErr := f.submit(f.userID, foreign.ID)
	if foreignErr == nil {
		t.Fatal("用户 A 拿到了用户 B 的 asset")
	}
	_, ghostErr := f.submit(f.userID, ghost)
	if ghostErr == nil {
		t.Fatal("不存在的 id 居然过了")
	}

	// 逐字比较：id 本身当然不同，所以比的是把 id 抠掉之后的模板。
	gotForeign := fieldErrorsOf(foreignErr)
	gotGhost := fieldErrorsOf(ghostErr)
	if len(gotForeign) != 1 || len(gotGhost) != 1 {
		t.Fatalf("field_errors 条数不一致：foreign=%v ghost=%v", gotForeign, gotGhost)
	}
	if gotForeign[0].Key != gotGhost[0].Key {
		t.Errorf("Key 不同：%q vs %q", gotForeign[0].Key, gotGhost[0].Key)
	}
	if gotForeign[0].Message != inputRefNotFound(foreign.ID) {
		t.Errorf("别人的 asset 报错 = %q，泄露了存在性", gotForeign[0].Message)
	}
	if gotGhost[0].Message != inputRefNotFound(ghost) {
		t.Errorf("不存在的 id 报错 = %q", gotGhost[0].Message)
	}
}

// TestSubmitHidesForeignUploadExistence 是上一条在 upload 分支的对照，
// 保证新增 asset 分支后两条来源的口径仍然一致。
func TestSubmitHidesForeignUploadExistence(t *testing.T) {
	f := newInputsFixture(t)
	foreign := f.newUpload(t, f.otherID, 1024, 576)

	_, err := f.submit(f.userID, foreign.ID)
	if err == nil {
		t.Fatal("用户 A 拿到了用户 B 的 upload")
	}
	fields := fieldErrorsOf(err)
	if len(fields) != 1 || fields[0].Message != inputRefNotFound(foreign.ID) {
		t.Errorf("别人的 upload 报错 = %v，与 asset 分支口径不一致", fields)
	}
}
