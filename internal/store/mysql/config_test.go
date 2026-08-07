package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// TestUserRepoRoundTrip 走一遍用户的增改查，并钉住两条容易写错的规则：
// 重名返回 409 而不是 500，UpdateProfile 把角色改成它已有的值不算"用户不存在"。
func TestUserRepoRoundTrip(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewUserRepo(db)

	f := newFixture(t, 100, 1<<30)

	u, err := repo.GetByID(ctx, f.userID)
	requireNoErr(t, err, "get user by id")
	if u.Credits != 100 || u.StorageQuota != 1<<30 {
		t.Fatalf("credits/quota not persisted: %d / %d", u.Credits, u.StorageQuota)
	}
	if u.Role != domain.RoleUser || u.Status != domain.UserStatusActive {
		t.Errorf("defaults not applied: role=%q status=%q", u.Role, u.Status)
	}
	if u.CreatedAt.IsZero() || u.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at is not UTC: %v", u.CreatedAt)
	}

	byName, err := repo.GetByUsername(ctx, u.Username)
	requireNoErr(t, err, "get user by username")
	if byName.ID != u.ID {
		t.Errorf("GetByUsername returned %q, want %q", byName.ID, u.ID)
	}

	// 重名撞唯一键：必须是 409，不是驱动错误直接冒上来。
	_, err = repo.Create(ctx, domain.User{Username: u.Username, PasswordHash: "x"})
	requireCode(t, err, domain.CodeConflict)

	requireNoErr(t, repo.UpdatePassword(ctx, u.ID, "argon2id$new"), "update password")
	after, err := repo.GetByID(ctx, u.ID)
	requireNoErr(t, err, "re-read user")
	if after.PasswordHash != "argon2id$new" {
		t.Errorf("password hash not updated: %q", after.PasswordHash)
	}
	requireCode(t, repo.UpdatePassword(ctx, uid.New(), "x"), domain.CodeNotFound)

	// 同值 UPDATE 在 MySQL 下影响 0 行，这里不该被误判成 404。
	updated, err := repo.UpdateProfile(ctx, u.ID, domain.RoleUser, domain.UserStatusActive, 1<<30)
	requireNoErr(t, err, "no-op update profile")
	if updated.Role != domain.RoleUser {
		t.Errorf("role changed unexpectedly: %q", updated.Role)
	}
	promoted, err := repo.UpdateProfile(ctx, u.ID, domain.RoleAdmin, domain.UserStatusDisabled, 4096)
	requireNoErr(t, err, "update profile")
	if promoted.Role != domain.RoleAdmin || promoted.Status != domain.UserStatusDisabled || promoted.StorageQuota != 4096 {
		t.Errorf("profile not persisted: %+v", promoted)
	}
	_, err = repo.UpdateProfile(ctx, u.ID, "superuser", domain.UserStatusActive, 1)
	requireCode(t, err, domain.CodeInvalidParam)

	at := time.Now().UTC().Truncate(time.Millisecond)
	requireNoErr(t, repo.TouchLastActive(ctx, u.ID, at), "touch last active")
	touched, err := repo.GetByID(ctx, u.ID)
	requireNoErr(t, err, "re-read user")
	if touched.LastActiveAt == nil || !touched.LastActiveAt.Equal(at) {
		t.Fatalf("last_active_at = %v, want %v", touched.LastActiveAt, at)
	}
	// 迟到的请求不该把 last_active_at 往回拨。
	requireNoErr(t, repo.TouchLastActive(ctx, u.ID, at.Add(-time.Hour)), "stale touch")
	stale, err := repo.GetByID(ctx, u.ID)
	requireNoErr(t, err, "re-read user")
	if !stale.LastActiveAt.Equal(at) {
		t.Errorf("last_active_at moved backwards to %v", stale.LastActiveAt)
	}

	requireCode(t, func() error { _, err := repo.GetByID(ctx, ""); return err }(), domain.CodeInvalidParam)
	requireCode(t, func() error { _, err := repo.GetByID(ctx, uid.New()); return err }(), domain.CodeNotFound)
}

// TestUserRepoListPaging 钉住游标分页：两页不重不漏，且过滤条件跨页保持。
func TestUserRepoListPaging(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewUserRepo(db)

	// 共享前缀让这批用户能被 q 精确圈出来，不受库里其他数据影响。
	prefix := "page_" + uid.Token(6)
	const total = 5
	for i := 0; i < total; i++ {
		newUserNamed(t, prefix)
	}

	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 10; page++ {
		p, err := repo.List(ctx, prefix, cursor, 2)
		requireNoErr(t, err, "list users")
		for _, u := range p.Items {
			if seen[u.ID] {
				t.Fatalf("user %s appeared on two pages", u.ID)
			}
			seen[u.ID] = true
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != total {
		t.Fatalf("paged over %d users, want %d", len(seen), total)
	}

	// 垃圾游标降级成第一页，不是错误。
	p, err := repo.List(ctx, prefix, "!!!garbage!!!", 100)
	requireNoErr(t, err, "list with garbage cursor")
	if len(p.Items) != total {
		t.Errorf("garbage cursor returned %d users, want the full first page of %d", len(p.Items), total)
	}

	// LIKE 的通配符走绑定参数：用户输入里的 % 是字面量语义之外的通配符，
	// 但它绝不该改变语句结构。这里只验证它不炸且不越权返回全表。
	all, err := repo.List(ctx, prefix+"%' OR '1'='1", "", 100)
	requireNoErr(t, err, "list with sql-ish input")
	if len(all.Items) != 0 {
		t.Errorf("injection-shaped query matched %d users; the input must be a bound value", len(all.Items))
	}
}

// TestProviderRepoDeleteConflict 钉住接口注释里那条硬规则：
// 供应商仍被模型引用时返回 409，绝不级联。
func TestProviderRepoDeleteConflict(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewProviderRepo(db)
	f := newFixture(t, 0, 1<<30)

	err := repo.Delete(ctx, f.providerID)
	requireCode(t, err, domain.CodeConflict)
	// 错误信息要点出是哪个模型挡着，否则管理员无从下手。
	if !contains(err.Error(), f.modelID) {
		t.Errorf("conflict message should name the referencing model, got %q", err.Error())
	}

	// 模型还在：级联删是被明确禁止的。
	if _, err := repo.Get(ctx, f.providerID); err != nil {
		t.Fatalf("provider was deleted despite the conflict: %v", err)
	}
	if _, err := NewModelRepo(db).Get(ctx, f.modelID); err != nil {
		t.Fatalf("model was cascaded away: %v", err)
	}

	// 去掉引用之后就能删了。
	requireNoErr(t, NewModelRepo(db).Delete(ctx, f.modelID), "delete model")
	requireNoErr(t, repo.Delete(ctx, f.providerID), "delete provider")
	requireCode(t, repo.Delete(ctx, f.providerID), domain.CodeNotFound)
}

// TestProviderRepoUpsert 覆盖新增 / 覆盖 / 重名冲突与默认值填充。
func TestProviderRepoUpsert(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewProviderRepo(db)
	f := newFixture(t, 0, 1<<30)

	p, err := repo.Get(ctx, f.providerID)
	requireNoErr(t, err, "get provider")
	if p.AuthStyle != domain.AuthStyleBearer || p.TimeoutMS != 30000 || p.MaxConcurrency != 8 {
		t.Errorf("provider defaults not applied: %+v", p)
	}
	// credential_ref 指向的环境变量没设，因此 CredentialPresent 必须是 false。
	// 密钥本身不入库，这个布尔是它唯一的对外投影。
	if p.CredentialPresent {
		t.Errorf("credential_present must be false when %s is unset", p.CredentialRef)
	}

	p.TimeoutMS = 12345
	p.Enabled = false
	updated, err := repo.Upsert(ctx, p)
	requireNoErr(t, err, "upsert provider")
	if updated.TimeoutMS != 12345 || updated.Enabled {
		t.Errorf("upsert did not overwrite: %+v", updated)
	}

	// 重名撞唯一键 → 409。
	_, err = repo.Upsert(ctx, domain.Provider{
		Name: p.Name, BaseURL: "https://example.invalid", CredentialRef: "X",
	})
	requireCode(t, err, domain.CodeConflict)

	for _, bad := range []domain.Provider{
		{Name: "", BaseURL: "u", CredentialRef: "c"},
		{Name: "n", BaseURL: "", CredentialRef: "c"},
		{Name: "n", BaseURL: "u", CredentialRef: ""},
	} {
		_, err := repo.Upsert(ctx, bad)
		requireCode(t, err, domain.CodeInvalidParam)
	}

	list, err := repo.List(ctx)
	requireNoErr(t, err, "list providers")
	if len(list) == 0 {
		t.Error("provider list is empty")
	}
}

// TestModelRepoFingerprint 钉住任务书里最吃紧的一条：
// **加一条模型记录必须立刻改变指纹，不需要重启进程**。
func TestModelRepoFingerprint(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewModelRepo(db)
	f := newFixture(t, 0, 1<<30)

	before, err := repo.Fingerprint(ctx)
	requireNoErr(t, err, "fingerprint")
	// 直接作为强 ETag 下发，因此必须自带引号（RFC 9110）。
	if len(before) < 2 || before[0] != '"' || before[len(before)-1] != '"' {
		t.Fatalf("fingerprint %q is not a quoted strong ETag", before)
	}

	extraID := "test-model-" + uid.Token(8)
	_, err = repo.Upsert(ctx, testModelConfig(extraID, f.providerID))
	requireNoErr(t, err, "upsert extra model")
	t.Cleanup(func() { _ = repo.Delete(ctx, extraID) })

	after, err := repo.Fingerprint(ctx)
	requireNoErr(t, err, "fingerprint after upsert")
	if after == before {
		t.Fatalf("fingerprint did not change after adding a model: %q", after)
	}

	// 同一份配置连查两次必须稳定，否则 ETag 永不命中 304。
	again, err := repo.Fingerprint(ctx)
	requireNoErr(t, err, "fingerprint again")
	if again != after {
		t.Errorf("fingerprint is not stable: %q then %q", after, again)
	}

	// 本仓储不带进程内缓存：List 每次都查库，因此新模型立刻可见。
	enabled := true
	models, err := repo.List(ctx, domain.ModelFilter{Enabled: &enabled})
	requireNoErr(t, err, "list models")
	found := false
	for _, m := range models {
		if m.ID == extraID {
			found = true
		}
	}
	if !found {
		t.Error("newly added model is not visible without a restart")
	}
}

// TestModelRepoUpsertValidation 钉住 capability 派生列与几条落库前的校验。
func TestModelRepoUpsertValidation(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewModelRepo(db)
	f := newFixture(t, 0, 1<<30)

	m, err := repo.Get(ctx, f.modelID)
	requireNoErr(t, err, "get model")
	// Capability 是 any，必须以 json.Unmarshal 的默认形态回来（对象 → map）。
	capability, ok := m.Capability.(map[string]any)
	if !ok {
		t.Fatalf("capability came back as %T, want map[string]any", m.Capability)
	}
	if capability["id"] != f.modelID || capability["modality"] != "image" {
		t.Errorf("capability round trip lost fields: %+v", capability)
	}

	// capability.id 与行主键不一致 → 提交上来的任务会查不到模型，必须拦住。
	bad := testModelConfig(f.modelID, f.providerID)
	bad.Capability = map[string]any{
		"id": "some-other-id", "name": "n", "vendor": "v", "modality": "image",
	}
	_, err = repo.Upsert(ctx, bad)
	requireCode(t, err, domain.CodeInvalidParam)

	// video 族缺 video_protocol：库里有 CHECK，但这里在落库前就该报出来。
	video := testModelConfig("test-model-"+uid.Token(8), f.providerID)
	video.Family = domain.FamilyVideo
	_, err = repo.Upsert(ctx, video)
	requireCode(t, err, domain.CodeInvalidParam)

	// modality 不在 image/video 里。
	badModality := testModelConfig("test-model-"+uid.Token(8), f.providerID)
	badModality.Capability = map[string]any{
		"id": badModality.ID, "name": "n", "vendor": "v", "modality": "audio",
	}
	_, err = repo.Upsert(ctx, badModality)
	requireCode(t, err, domain.CodeInvalidParam)

	// 模型被任务引用后删不掉（tasks.model_id 是 RESTRICT），翻成 409。
	f.newTask(t, 1)
	requireCode(t, repo.Delete(ctx, f.modelID), domain.CodeConflict)
	requireCode(t, repo.Delete(ctx, "no-such-model"), domain.CodeNotFound)
}

// TestSkillRepo 覆盖 List / Get / Upsert / Delete 与 SystemPromptRef 的派生规则。
func TestSkillRepo(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewSkillRepo(db)

	created, err := repo.Upsert(ctx, domain.Skill{
		Name:          "Test Skill " + uid.Token(4),
		Description:   "for tests",
		SystemPrompt:  "you are a test",
		DefaultParams: map[string]any{"aspect": "1:1"},
		Enabled:       true,
		Order:         900,
	})
	requireNoErr(t, err, "upsert skill")
	t.Cleanup(func() { _ = repo.Delete(ctx, created.ID) })

	if created.SystemPromptRef == "" {
		t.Fatal("system_prompt_ref must be present; it is all the client ever gets")
	}
	if created.SystemPrompt != "you are a test" {
		t.Errorf("system_prompt not persisted: %q", created.SystemPrompt)
	}

	// 提示词改了，ref 必须跟着变——那正是前端该让缓存失效的时刻。
	edited := created
	edited.SystemPrompt = "you are a different test"
	edited, err = repo.Upsert(ctx, edited)
	requireNoErr(t, err, "edit skill prompt")
	if edited.SystemPromptRef == created.SystemPromptRef {
		t.Error("system_prompt_ref did not change after the prompt was edited")
	}

	// 同一份内容重复读要给出同一个 ref，否则它做不了缓存键。
	reread, err := repo.Get(ctx, edited.ID)
	requireNoErr(t, err, "get skill")
	if reread.SystemPromptRef != edited.SystemPromptRef {
		t.Errorf("ref is not stable: %q vs %q", reread.SystemPromptRef, edited.SystemPromptRef)
	}
	// default_params 在 API 里是 required：NULL 也要投影成空对象。
	if reread.DefaultParams == nil {
		t.Error("default_params must never come back nil")
	}

	// 禁用之后：List(false) 看不到，但 Get 仍读得到——
	// 管理端要能打开它改回来，历史消息上的 skill_id 也要能显示。
	disabled := reread
	disabled.Enabled = false
	_, err = repo.Upsert(ctx, disabled)
	requireNoErr(t, err, "disable skill")

	if _, err := repo.Get(ctx, disabled.ID); err != nil {
		t.Errorf("Get must not filter on enabled: %v", err)
	}
	enabledOnly, err := repo.List(ctx, false)
	requireNoErr(t, err, "list enabled skills")
	for _, s := range enabledOnly {
		if s.ID == disabled.ID {
			t.Error("disabled skill leaked into the user-facing list")
		}
	}
	all, err := repo.List(ctx, true)
	requireNoErr(t, err, "list all skills")
	foundDisabled := false
	for _, s := range all {
		if s.ID == disabled.ID {
			foundDisabled = true
		}
	}
	if !foundDisabled {
		t.Error("includeDisabled=true did not return the disabled skill")
	}

	// 空名 / 空提示词都不该落库：后者会让这条记录退化成一个空壳。
	_, err = repo.Upsert(ctx, domain.Skill{Name: "", SystemPrompt: "p"})
	requireCode(t, err, domain.CodeInvalidParam)
	_, err = repo.Upsert(ctx, domain.Skill{Name: "n", SystemPrompt: "   "})
	requireCode(t, err, domain.CodeInvalidParam)

	requireNoErr(t, repo.Delete(ctx, disabled.ID), "delete skill")
	requireCode(t, repo.Delete(ctx, disabled.ID), domain.CodeNotFound)
	requireCode(t, func() error { _, err := repo.Get(ctx, disabled.ID); return err }(), domain.CodeNotFound)
}
