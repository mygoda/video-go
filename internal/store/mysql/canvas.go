package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// canvasRepo 是 store.CanvasRepo 的 MySQL 实现。
type canvasRepo struct{ db *sql.DB }

// NewCanvasRepo 构造 store.CanvasRepo。
func NewCanvasRepo(db *sql.DB) store.CanvasRepo { return &canvasRepo{db: db} }

const projectColumns = `p.id, p.user_id, p.name, p.revision, p.cover_asset_id,
	p.created_at, p.updated_at,
	(SELECT COUNT(*) FROM canvas_cards c WHERE c.project_id = p.id AND c.deleted_at IS NULL)`

func scanProject(s rowScanner) (domain.Project, error) {
	var (
		p            domain.Project
		coverAssetID sql.NullString
	)
	err := s.Scan(&p.ID, &p.UserID, &p.Name, &p.Revision, &coverAssetID,
		&p.CreatedAt, &p.UpdatedAt, &p.CardCount)
	if err != nil {
		return domain.Project{}, err
	}
	p.CoverAssetID = strPtr(coverAssetID)
	p.CreatedAt = p.CreatedAt.UTC()
	p.UpdatedAt = p.UpdatedAt.UTC()
	return p, nil
}

func (r *canvasRepo) CreateProject(ctx context.Context, p domain.Project) (domain.Project, error) {
	if err := requireID("project user_id", p.UserID); err != nil {
		return domain.Project{}, err
	}
	if p.Name == "" {
		return domain.Project{}, invalidParam("project name must not be empty")
	}
	if p.ID == "" {
		p.ID = uid.New()
	}

	_, err := r.db.ExecContext(ctx,
		`INSERT INTO projects (id, user_id, name, revision, cover_asset_id)
		 VALUES (?, ?, ?, 0, ?)`,
		p.ID, p.UserID, p.Name, nullString(p.CoverAssetID))
	if err != nil {
		return domain.Project{}, wrap("create project", err)
	}
	return r.GetProject(ctx, p.ID)
}

// GetProject 读一个项目。软删的读不出来，与 AssetRepo.Get 同理。
func (r *canvasRepo) GetProject(ctx context.Context, id string) (domain.Project, error) {
	if err := requireID("project id", id); err != nil {
		return domain.Project{}, err
	}
	row := r.db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects p WHERE p.id = ? AND p.deleted_at IS NULL`, id)
	p, err := scanProject(row)
	if err != nil {
		return domain.Project{}, wrapScan("project", id, "get project", err)
	}
	return p, nil
}

// ListProjects 按 (updated_at DESC, id DESC) 游标分页，
// 对齐 idx_projects_user_deleted_updated。
//
// 按 updated_at 而不是 created_at 排：项目列表要的是"最近动过的在最前"，
// 用户回到首页第一眼该看到刚才在画的那个。
func (r *canvasRepo) ListProjects(ctx context.Context, userID, cursor string, limit int) (store.Page[domain.Project], error) {
	if err := requireID("user id", userID); err != nil {
		return store.Page[domain.Project]{}, err
	}
	limit = clampLimit(limit)

	var (
		rows *sql.Rows
		err  error
	)
	if c, ok := decodeCursor(cursor); ok {
		rows, err = r.db.QueryContext(ctx,
			`SELECT `+projectColumns+` FROM projects p
			 WHERE p.user_id = ? AND p.deleted_at IS NULL
			   AND (p.updated_at < ? OR (p.updated_at = ? AND p.id < ?))
			 ORDER BY p.updated_at DESC, p.id DESC LIMIT ?`,
			userID, c.At, c.At, c.ID, limit+1)
	} else {
		rows, err = r.db.QueryContext(ctx,
			`SELECT `+projectColumns+` FROM projects p
			 WHERE p.user_id = ? AND p.deleted_at IS NULL
			 ORDER BY p.updated_at DESC, p.id DESC LIMIT ?`,
			userID, limit+1)
	}
	if err != nil {
		return store.Page[domain.Project]{}, wrap("list projects", err)
	}
	defer rows.Close()

	var items []domain.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return store.Page[domain.Project]{}, wrap("scan project", err)
		}
		items = append(items, p)
	}
	if err := rows.Err(); err != nil {
		return store.Page[domain.Project]{}, wrap("list projects", err)
	}

	items, hasMore := pageSlice(items, limit)
	page := store.Page[domain.Project]{Items: items}
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		page.NextCursor = encodeTimeCursor(last.UpdatedAt, last.ID)
	}
	return page, nil
}

// UpdateProject 改项目的可编辑字段（名称、封面）。
//
// **不动 revision**：revision 是画布内容的乐观锁，改个项目名不该让所有
// 打开着这个画布的标签页收到 409。
func (r *canvasRepo) UpdateProject(ctx context.Context, p domain.Project) (domain.Project, error) {
	if err := requireID("project id", p.ID); err != nil {
		return domain.Project{}, err
	}
	if p.Name == "" {
		return domain.Project{}, invalidParam("project name must not be empty")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE projects SET name = ?, cover_asset_id = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		p.Name, nullString(p.CoverAssetID), p.ID)
	if err != nil {
		return domain.Project{}, wrap("update project", err)
	}
	if _, err := affected(res, "update project"); err != nil {
		return domain.Project{}, err
	}
	return r.GetProject(ctx, p.ID)
}

// DeleteProject 是软删。
//
// 与资产同理：卡片上挂着任务与资产的引用，硬删会让 canvas_cards 连同
// canvas_ops 一起被外键级联抹掉，「创作过程可回放」的数据基础就没了。
func (r *canvasRepo) DeleteProject(ctx context.Context, id string) error {
	if err := requireID("project id", id); err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE projects SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		time.Now().UTC(), id)
	if err != nil {
		return wrap("delete project", err)
	}
	n, err := affected(res, "delete project")
	if err != nil {
		return err
	}
	if n == 0 {
		return notFound("project", id)
	}
	return nil
}

const cardColumns = `id, kind, title, x, y, w, h, z, task_id, asset_id, ` + "`text`" + `,
	model_id, prompt, params, refs, history, auto_placed, created_at`

func scanCard(s rowScanner) (domain.Card, error) {
	var (
		c          domain.Card
		taskID     sql.NullString
		assetID    sql.NullString
		text       sql.NullString
		modelID    sql.NullString
		prompt     sql.NullString
		paramsRaw  []byte
		refsRaw    []byte
		historyRaw []byte
	)
	err := s.Scan(&c.ID, &c.Kind, &c.Title, &c.X, &c.Y, &c.W, &c.H, &c.Z,
		&taskID, &assetID, &text, &modelID, &prompt,
		&paramsRaw, &refsRaw, &historyRaw, &c.AutoPlaced, &c.CreatedAt)
	if err != nil {
		return domain.Card{}, err
	}
	c.TaskID = strPtr(taskID)
	c.AssetID = strPtr(assetID)
	c.Text = strPtr(text)
	c.ModelID = strPtr(modelID)
	c.Prompt = strPtr(prompt)
	if err := scanJSON(paramsRaw, &c.Params); err != nil {
		return domain.Card{}, err
	}
	if err := scanJSON(refsRaw, &c.Refs); err != nil {
		return domain.Card{}, err
	}
	if err := scanJSON(historyRaw, &c.History); err != nil {
		return domain.Card{}, err
	}
	// Refs 是血缘（「多卡引用为下次输入」靠它），History 是版本切换器的数据源，
	// 前端都按数组渲染并直接读 .length。
	// nil 与 [] 在 JSON 里是 null 与 []，后者才是"没有引用/没有历史"的正确表达。
	if c.Refs == nil {
		c.Refs = []string{}
	}
	if c.History == nil {
		c.History = []domain.CardVersion{}
	}
	c.CreatedAt = c.CreatedAt.UTC()
	return c, nil
}

// Snapshot 拉画布全量：未删除的卡片 + 全部对话 + 视口 + 当前 revision。
//
// 在一个事务里读，否则 revision 与卡片可能来自两个不同的时刻——
// 客户端拿着一个比卡片更新的 revision 去提交 op，服务端会认为它是最新的
// 而放行，但它的本地卡片其实少了一张。
func (r *canvasRepo) Snapshot(ctx context.Context, projectID string) (domain.CanvasSnapshot, error) {
	if err := requireID("project id", projectID); err != nil {
		return domain.CanvasSnapshot{}, err
	}

	var snap domain.CanvasSnapshot
	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		var viewportRaw []byte
		switch err := tx.QueryRowContext(ctx,
			`SELECT revision, viewport FROM projects WHERE id = ? AND deleted_at IS NULL`,
			projectID).Scan(&snap.Revision, &viewportRaw); {
		case isNoRows(err):
			return notFound("project", projectID)
		case err != nil:
			return wrap("read project revision", err)
		}
		if len(viewportRaw) > 0 && string(viewportRaw) != "null" {
			var vp domain.Viewport
			if err := scanJSON(viewportRaw, &vp); err != nil {
				return wrap("decode viewport", err)
			}
			snap.Viewport = &vp
		}

		cards, err := readCards(ctx, tx, projectID)
		if err != nil {
			return err
		}
		snap.Cards = cards

		messages, err := readMessages(ctx, tx, projectID)
		if err != nil {
			return err
		}
		snap.Conversation = messages
		return nil
	})
	if err != nil {
		return domain.CanvasSnapshot{}, err
	}
	return snap, nil
}

func readCards(ctx context.Context, q querier, projectID string) ([]domain.Card, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+cardColumns+` FROM canvas_cards
		 WHERE project_id = ? AND deleted_at IS NULL
		 ORDER BY z ASC, created_at ASC, id ASC`, projectID)
	if err != nil {
		return nil, wrap("read cards", err)
	}
	defer rows.Close()

	var out []domain.Card
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, wrap("scan card", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("read cards", err)
	}
	return out, nil
}

const messageColumns = `id, project_id, role, content, skill_id, ref_card_ids, task_ids, created_at`

func scanMessage(s rowScanner) (domain.Message, error) {
	var (
		m          domain.Message
		skillID    sql.NullString
		refCards   []byte
		taskIDsRaw []byte
	)
	if err := s.Scan(&m.ID, &m.ProjectID, &m.Role, &m.Content, &skillID,
		&refCards, &taskIDsRaw, &m.CreatedAt); err != nil {
		return domain.Message{}, err
	}
	m.SkillID = strPtr(skillID)
	if err := scanJSON(refCards, &m.RefCardIDs); err != nil {
		return domain.Message{}, err
	}
	if err := scanJSON(taskIDsRaw, &m.TaskIDs); err != nil {
		return domain.Message{}, err
	}
	m.CreatedAt = m.CreatedAt.UTC()
	return m, nil
}

func readMessages(ctx context.Context, q querier, projectID string) ([]domain.Message, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM canvas_messages
		 WHERE project_id = ? ORDER BY created_at ASC, id ASC`, projectID)
	if err != nil {
		return nil, wrap("read messages", err)
	}
	defer rows.Close()

	var out []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, wrap("scan message", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("read messages", err)
	}
	return out, nil
}

// ApplyOps 在一个事务里校验 baseRevision、按序落库 ops、推进 revision。
//
// 三件事必须同事务：校验通过后到写入之间要是被另一个标签页插进来一批 op，
// 两边的 op 就会以一个谁也没预期的顺序交错，而画布是有序状态机——
// "先删卡片再移动它"和"先移动再删"的结果不一样。
//
// baseRevision 不匹配返回 domain.CodeRevisionConflict（409），
// 前端据此拉全量覆盖本地。MVP 明确不做 CRDT（不做协作）。
//
// 整批 op 原样存进 canvas_ops 一行，**这就是「创作过程可回放」的数据基础**，
// 因此这里存的是调用方给的 op 序列本身，不做任何归并或化简：
// 化简掉的中间状态正是回放要看的东西。
func (r *canvasRepo) ApplyOps(ctx context.Context, projectID string, baseRevision int64, ops []domain.CanvasOp) (int64, error) {
	if err := requireID("project id", projectID); err != nil {
		return 0, err
	}

	var newRevision int64
	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		var (
			current int64
			userID  string
		)
		switch err := tx.QueryRowContext(ctx,
			`SELECT revision, user_id FROM projects WHERE id = ? AND deleted_at IS NULL FOR UPDATE`,
			projectID).Scan(&current, &userID); {
		case isNoRows(err):
			return notFound("project", projectID)
		case err != nil:
			return wrap("lock project", err)
		}

		if current != baseRevision {
			return revisionConflict(
				"project " + projectID + " is at revision " + strconv.FormatInt(current, 10) +
					", client sent base revision " + strconv.FormatInt(baseRevision, 10))
		}

		// 空 op 批次是合法的空操作：客户端的心跳式提交不该推进 revision，
		// 否则每次心跳都会让别的标签页收到一次无意义的 409。
		if len(ops) == 0 {
			newRevision = current
			return nil
		}

		for i, op := range ops {
			if err := applyOp(ctx, tx, projectID, op); err != nil {
				return err
			}
			_ = i
		}

		newRevision = current + 1
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET revision = ? WHERE id = ?`, newRevision, projectID); err != nil {
			return wrap("bump project revision", err)
		}

		opsJSON, err := json.Marshal(ops)
		if err != nil {
			return invalidParam("canvas ops are not valid JSON: " + err.Error())
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO canvas_ops (project_id, revision, ops, actor_user_id) VALUES (?, ?, ?, ?)`,
			projectID, newRevision, opsJSON, userID); err != nil {
			return wrap("append canvas ops", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newRevision, nil
}

// cardMaxVersions 是一张卡片的成片 / 出图版本历史上限。剧本版本上限在 domain
// （ScriptMaxVersions=50），成片是资产、比文本重，留少一点。
const cardMaxVersions = 20

// SetCardResult 把任务产物回填到卡片上，并推进 revision。见 store.CanvasRepo。
//
// 同样往 canvas_ops 里追一条 card.update：那张表是「创作过程可回放」的数据
// 基础，而"这张卡在第 N 版拿到了产物"正是回放最需要的一帧。少记这一条，
// 回放出来的画布会从头到尾都是空卡片。
//
// actor_user_id 记项目主人而不是留空：这一列非空，且产物确实是他花积分生成的。
func (r *canvasRepo) SetCardResult(ctx context.Context, projectID, cardID string, assetID *string, prompt string) error {
	if err := requireID("project id", projectID); err != nil {
		return err
	}
	if err := requireID("card id", cardID); err != nil {
		return err
	}

	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		var (
			current int64
			userID  string
		)
		switch err := tx.QueryRowContext(ctx,
			`SELECT revision, user_id FROM projects WHERE id = ? AND deleted_at IS NULL FOR UPDATE`,
			projectID).Scan(&current, &userID); {
		case isNoRows(err):
			// 画布被删了。任务本身没问题，产物已经在资产库里，无处回填而已。
			return nil
		case err != nil:
			return wrap("lock project", err)
		}

		// 读一眼现有 history，把这一版产物追加进去。每次成功出片 / 出图都留一版，
		// 供卡片左下角的版本切换器 ‹2/3› 来回切；asset_id 始终指向当前这一版。
		// 全程在项目行锁里，「读旧 history—追加—写回」插不进另一次回填。
		var historyRaw []byte
		switch err := tx.QueryRowContext(ctx,
			`SELECT history FROM canvas_cards WHERE id = ? AND project_id = ? AND deleted_at IS NULL`,
			cardID, projectID).Scan(&historyRaw); {
		case isNoRows(err):
			// 卡片被用户删掉了。什么也不做。
			return nil
		case err != nil:
			return wrap("load card history", err)
		}

		setCols := "asset_id = ?"
		args := []any{nullString(assetID)}
		if assetID != nil {
			var history []domain.CardVersion
			if err := scanJSON(historyRaw, &history); err != nil {
				return wrap("read card history", err)
			}
			history = append(history, domain.CardVersion{
				AssetID: *assetID, Prompt: prompt, At: time.Now().UTC(),
			})
			// 只留最近 cardMaxVersions 版：老到没人会翻的成片再留着只是撑大这一列。
			if len(history) > cardMaxVersions {
				history = history[len(history)-cardMaxVersions:]
			}
			raw, err := jsonValue(history)
			if err != nil {
				return wrap("marshal card history", err)
			}
			setCols += ", history = ?"
			args = append(args, raw)
		}
		args = append(args, cardID, projectID)

		res, err := tx.ExecContext(ctx,
			`UPDATE canvas_cards SET `+setCols+
				` WHERE id = ? AND project_id = ? AND deleted_at IS NULL`, args...)
		if err != nil {
			return wrap("set card result", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return wrap("set card result", err)
		} else if n == 0 {
			// 卡片被用户删掉了。不推进 revision——什么也没变。
			return nil
		}

		next := current + 1
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET revision = ? WHERE id = ?`, next, projectID); err != nil {
			return wrap("bump project revision", err)
		}

		ops := []domain.CanvasOp{{
			Type:  domain.OpCardUpdate,
			ID:    cardID,
			Patch: map[string]any{"asset_id": assetID},
		}}
		opsJSON, err := json.Marshal(ops)
		if err != nil {
			return wrap("marshal card result op", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO canvas_ops (project_id, revision, ops, actor_user_id) VALUES (?, ?, ?, ?)`,
			projectID, next, opsJSON, userID); err != nil {
			return wrap("append canvas ops", err)
		}
		return nil
	})
}

// applyOp 把一条 op 落到物化的卡片 / 视口上。//
// 判别逻辑本该集中在 canvas.Applier（见 domain.CanvasOp 的注释），但那一层
// 委托给了本仓储（canvas.applier 自己不写 SQL，因为事务边界属于仓储），
// 因此分派实际发生在这里。每个分支只碰它该碰的列。
func applyOp(ctx context.Context, tx *sql.Tx, projectID string, op domain.CanvasOp) error {
	switch op.Type {
	case domain.OpCardCreate:
		if op.Card == nil {
			return invalidParam("card.create requires a card")
		}
		return insertCard(ctx, tx, projectID, *op.Card)

	case domain.OpCardMove:
		if op.ID == "" {
			return invalidParam("card.move requires a card id")
		}
		// COALESCE 让只给 x/y 不给 z 的移动保持 z 不变。
		// auto_placed 一律置 0：用户手动移动过之后该卡片所在区域
		// 不再参与自动重排（见 domain.Card 的注释）。
		res, err := tx.ExecContext(ctx,
			`UPDATE canvas_cards SET
			   x = COALESCE(?, x), y = COALESCE(?, y), z = COALESCE(?, z),
			   auto_placed = 0
			 WHERE id = ? AND project_id = ? AND deleted_at IS NULL`,
			nullFloat(op.X), nullFloat(op.Y), nullFloat(op.Z), op.ID, projectID)
		if err != nil {
			return wrap("apply card.move", err)
		}
		return requireCardTouched(res, op.ID)

	case domain.OpCardUpdate:
		if op.ID == "" {
			return invalidParam("card.update requires a card id")
		}
		return updateCard(ctx, tx, projectID, op.ID, op.Patch)

	case domain.OpCardDelete:
		if op.ID == "" {
			return invalidParam("card.delete requires a card id")
		}
		// 软删：卡片上挂着 task_id / asset_id，硬删会让 canvas_ops 里
		// 引用它的历史 op 指向一个不存在的卡片，回放就断了。
		res, err := tx.ExecContext(ctx,
			`UPDATE canvas_cards SET deleted_at = ?
			 WHERE id = ? AND project_id = ? AND deleted_at IS NULL`,
			time.Now().UTC(), op.ID, projectID)
		if err != nil {
			return wrap("apply card.delete", err)
		}
		return requireCardTouched(res, op.ID)

	case domain.OpViewportSet:
		if op.Viewport == nil {
			return invalidParam("viewport.set requires a viewport")
		}
		// k ∈ [0.1, 4]，见 domain.Viewport。越界的缩放会让前端算出一个
		// 谁也回不去的视口。
		if op.Viewport.K < 0.1 || op.Viewport.K > 4 {
			return invalidParam("viewport k must be within [0.1, 4]")
		}
		raw, err := json.Marshal(op.Viewport)
		if err != nil {
			return invalidParam("viewport is not valid JSON: " + err.Error())
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET viewport = ? WHERE id = ?`, raw, projectID); err != nil {
			return wrap("apply viewport.set", err)
		}
		return nil

	default:
		return invalidParam("unknown canvas op type " + string(op.Type))
	}
}

func requireCardTouched(res sql.Result, cardID string) error {
	n, err := affected(res, "apply canvas op")
	if err != nil {
		return err
	}
	if n == 0 {
		return notFound("card", cardID)
	}
	return nil
}

func insertCard(ctx context.Context, tx *sql.Tx, projectID string, c domain.Card) error {
	if c.ID == "" {
		c.ID = uid.New()
	}
	if !c.Kind.Valid() {
		return invalidParam("unknown card kind " + string(c.Kind))
	}
	// shot / character 卡的 params 有 schema（见 domain.ShotParams、
	// domain.CharacterParams），建卡这一刻就校验：kind 不在 card.update 的
	// 白名单里，一张卡建错了改不回来。
	switch c.Kind {
	case domain.CardKindShot:
		if _, err := domain.ParseShotParams(c.Params); err != nil {
			return err
		}
	case domain.CardKindScript:
		if _, err := domain.ParseScriptParams(c.Params); err != nil {
			return err
		}
	case domain.CardKindCharacter:
		if _, err := domain.ParseCharacterParams(c.Params); err != nil {
			return err
		}
	}

	params, err := jsonValue(c.Params)
	if err != nil {
		return invalidParam("card params is not valid JSON: " + err.Error())
	}
	refs, err := jsonValue(c.Refs)
	if err != nil {
		return invalidParam("card refs is not valid JSON: " + err.Error())
	}
	history, err := jsonValue(c.History)
	if err != nil {
		return invalidParam("card history is not valid JSON: " + err.Error())
	}

	// ON DUPLICATE KEY UPDATE：同一批 op 被重发时（客户端超时重试）
	// 不该因为卡片已存在而整批失败。
	_, err = tx.ExecContext(ctx,
		`INSERT INTO canvas_cards
		 (id, project_id, kind, title, x, y, w, h, z, task_id, asset_id, `+"`text`"+`,
		  model_id, prompt, params, refs, history, auto_placed)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		   title = VALUES(title),
		   x = VALUES(x), y = VALUES(y), w = VALUES(w), h = VALUES(h), z = VALUES(z),
		   task_id = VALUES(task_id), asset_id = VALUES(asset_id), `+"`text`"+` = VALUES(`+"`text`"+`),
		   model_id = VALUES(model_id), prompt = VALUES(prompt), params = VALUES(params),
		   refs = VALUES(refs), history = VALUES(history), auto_placed = VALUES(auto_placed),
		   deleted_at = NULL`,
		c.ID, projectID, string(c.Kind), c.Title, c.X, c.Y, c.W, c.H, c.Z,
		nullString(c.TaskID), nullString(c.AssetID), nullString(c.Text),
		nullString(c.ModelID), nullString(c.Prompt), params, refs, history, c.AutoPlaced)
	if err != nil {
		return wrap("insert card", err)
	}
	return nil
}

// cardPatchColumns 是 card.update 允许改的列的白名单。
//
// 白名单而不是"把 patch 的 key 当列名拼进 SQL"：后者是一条直接的注入通道，
// 而且会让客户端能改 project_id 这种它根本不该碰的列。
var cardPatchColumns = map[string]string{
	"title":       "title = ?",
	"w":           "w = ?",
	"h":           "h = ?",
	"task_id":     "task_id = ?",
	"asset_id":    "asset_id = ?",
	"text":        "`text` = ?",
	"model_id":    "model_id = ?",
	"prompt":      "prompt = ?",
	"params":      "params = ?",
	"refs":        "refs = ?",
	"history":     "history = ?",
	"auto_placed": "auto_placed = ?",
}

// updateCard 应用 card.update 的 patch。
//
// patch 的每个 key 查白名单拿到一段**固定的** SQL 片段，值走绑定参数。
// 未知 key 直接报错而不是忽略：静默丢弃会让客户端以为改成功了，
// 下次拉全量时发现没变，而那时已经无从追查是哪一次 patch 被吃掉了。
func updateCard(ctx context.Context, tx *sql.Tx, projectID, cardID string, patch map[string]any) error {
	if len(patch) == 0 {
		return nil
	}

	// map 迭代顺序随机，而 SET 子句的顺序不影响结果，因此这里不排序；
	// 但为了让错误信息稳定，未知 key 一发现就返回。
	var (
		sets []string
		args []any
	)
	for key, value := range patch {
		frag, ok := cardPatchColumns[key]
		if !ok {
			return invalidParam("card.update does not allow patching " + key)
		}
		arg, err := cardPatchArg(key, value)
		if err != nil {
			return err
		}
		sets = append(sets, frag)
		args = append(args, arg)
	}

	// shot 卡就地改台词 / 改镜头描述走 params 这一路，script 卡就地改正文走
	// text 这一路，两者写进去之前都要先看一眼卡片本身是什么 kind——建卡时
	// 校验过而改卡时不校验，等于留了一道后门。多这一次查询只发生在真的动了
	// text / params 的 patch 上，拖卡片、改标题都不受影响。
	extraSet, extraArg, err := patchByKind(ctx, tx, projectID, cardID, patch)
	if err != nil {
		return err
	}
	if extraSet != "" {
		sets = append(sets, extraSet)
		args = append(args, extraArg)
	}

	args = append(args, cardID, projectID)

	res, err := tx.ExecContext(ctx,
		`UPDATE canvas_cards SET `+joinComma(sets)+
			` WHERE id = ? AND project_id = ? AND deleted_at IS NULL`, args...)
	if err != nil {
		return wrap("apply card.update", err)
	}
	n, err := affected(res, "apply card.update")
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	// MySQL 的 affected rows 是"真的改了几行"，把同样的值存回去会报 0 行。
	// 因此 0 行有两种可能：卡片没了，或者这次保存什么也没改。剧本卡的编辑框
	// 点进去再点出来就会原样重发一次 patch，把它当 not_found 报会让前端以为
	// 卡片被别人删了，进而拉全量覆盖掉用户正在编辑的内容。
	return requireCardExists(ctx, tx, projectID, cardID)
}

func requireCardExists(ctx context.Context, tx *sql.Tx, projectID, cardID string) error {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM canvas_cards
		  WHERE id = ? AND project_id = ? AND deleted_at IS NULL`,
		cardID, projectID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound("card", cardID)
	}
	if err != nil {
		return wrap("check card exists", err)
	}
	return nil
}

// patchByKind 做 card.update 里依赖卡片自身 kind 的那部分处理，必要时返回
// 一段额外的 SET 片段与它的绑定值。
//
// 卡片不存在时返回空：紧随其后的 UPDATE 会因为影响 0 行而报 not_found，
// 由它来报比在这里抢先报一个语义相同的错更少一处分叉。
func patchByKind(ctx context.Context, tx *sql.Tx, projectID, cardID string, patch map[string]any) (string, any, error) {
	newText, patchesText := patch["text"]
	newParams, patchesParams := patch["params"]
	if !patchesText && !patchesParams {
		return "", nil, nil
	}

	var (
		kind      string
		title     string
		text      sql.NullString
		paramsRaw []byte
	)
	err := tx.QueryRowContext(ctx,
		"SELECT kind, title, `text`, params FROM canvas_cards"+
			` WHERE id = ? AND project_id = ? AND deleted_at IS NULL`,
		cardID, projectID).Scan(&kind, &title, &text, &paramsRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, wrap("load card for patch", err)
	}

	switch domain.CardKind(kind) {
	case domain.CardKindShot:
		if !patchesParams {
			return "", nil, nil
		}
		return "", nil, checkShotParams(newParams)
	case domain.CardKindScript:
		if patchesParams {
			return "", nil, errScriptParamsNotPatchable()
		}
		return scriptVersionSet(title, text.String, paramsRaw, newText)
	case domain.CardKindCharacter:
		if !patchesParams {
			return "", nil, nil
		}
		return "", nil, checkCharacterParams(newParams)
	}
	return "", nil, nil
}

// scriptVersionSet 把剧本卡**改动之前**的标题与正文追加成一个历史版本。
//
// 版本在服务端追加而不是让前端连 text 带 params 一起发：靠前端配合意味着
// 任何一个忘了带 params 的客户端（或一次直接打 API 的调试请求）都会让这次
// 修改无声地不留版本，而"旧版本没丢"是这张卡存在的一半理由。同一画布的
// 所有 op 都在 ApplyOps 的项目行锁里串行，所以"读旧值—算新值—写回"这三步
// 之间插不进另一次保存。
//
// 正文没变就不留版本：前端的自动保存会在用户只拖了一下卡片、或者点进编辑
// 框又原样点出来时照发 patch，为此攒一堆一模一样的版本会把版本列表变成
// 噪音，用户反而找不到真正改过的那几版。
func scriptVersionSet(oldTitle, oldText string, paramsRaw []byte, newText any) (string, any, error) {
	next, _ := newText.(string) // 非字符串在上面的白名单循环里已经被顶回去了
	if next == oldText {
		return "", nil, nil
	}

	params := map[string]any{}
	if err := scanJSON(paramsRaw, &params); err != nil {
		return "", nil, wrap("read script params", err)
	}
	sp, err := domain.ParseScriptParams(params)
	if err != nil {
		return "", nil, err
	}

	arg, err := jsonValue(sp.AppendVersion(oldTitle, oldText, time.Now().UTC()))
	if err != nil {
		return "", nil, wrap("marshal script versions", err)
	}
	return "params = ?", arg, nil
}

// errScriptParamsNotPatchable 挡住直接改剧本卡 params 的请求。
//
// 版本列表是只增不改的日志，放开它等于让客户端能把"上一版写了什么"改成
// 别的内容，那这份历史就不再是证据了。回退某一版走的是正常的改正文：
// 把旧版正文 patch 回 text，当前这版会作为新的一版留下来，什么也不会丢。
func errScriptParamsNotPatchable() error {
	return &domain.Error{
		Code:    domain.CodeInvalidParam,
		Message: "script 卡的 params 存的是版本历史，由服务端维护；改正文请直接 patch text",
		FieldErrors: []domain.FieldError{{
			Key: "params", Message: "script 卡不接受直接改 params",
		}},
	}
}

// checkShotParams 按 domain.ShotParams 校验一份新的 params。
func checkShotParams(value any) error {
	params, err := paramsMap(value)
	if err != nil {
		return err
	}
	_, err = domain.ParseShotParams(params)
	return err
}

// checkCharacterParams 按 domain.CharacterParams 校验一份新的 params。
func checkCharacterParams(value any) error {
	params, err := paramsMap(value)
	if err != nil {
		return err
	}
	_, err = domain.ParseCharacterParams(params)
	return err
}

// paramsMap 把 patch 里的 params 值 normalize 成 map。
//
// 值可能是 JSON 解出来的 map，也可能是调用方在 Go 侧直接构造的
// domain.ShotParams 这类结构体。统一走一次 JSON 往返，两种形态在校验函数里
// 就没有分支了。
func paramsMap(value any) (map[string]any, error) {
	params := map[string]any{}
	if value == nil {
		return params, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, invalidParam("card.params is not valid JSON: " + err.Error())
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParam("card.params must be an object")
	}
	return params, nil
}

// cardPatchArg 把 patch 里的值转成可绑定的形态。
//
// JSON 列要先 Marshal，标量原样传。走 JSON 的那几个 key 单列出来，
// 是因为直接把 map[string]any 绑给驱动会被拒（不是它认识的类型），
// 报出来的错跟"这个 patch 哪里不对"毫无关系。
func cardPatchArg(key string, value any) (any, error) {
	switch key {
	case "params", "refs", "history":
		arg, err := jsonValue(value)
		if err != nil {
			return nil, invalidParam("card." + key + " is not valid JSON: " + err.Error())
		}
		return arg, nil
	case "title":
		// title 那一列是 NOT NULL DEFAULT ''，因此 null 落空串而不是 NULL：
		// "把标题清空"和"这张卡没有标题"在产品上是同一件事，
		// 让它们在库里也只有一种表示，读回来就不必再判两种空。
		if value == nil {
			return "", nil
		}
		s, ok := value.(string)
		if !ok {
			return nil, invalidParam("card.title must be a string or null")
		}
		return s, nil
	case "w", "h":
		f, ok := toFloat(value)
		if !ok {
			return nil, invalidParam("card." + key + " must be a number")
		}
		return f, nil
	case "auto_placed":
		b, ok := value.(bool)
		if !ok {
			return nil, invalidParam("card.auto_placed must be a boolean")
		}
		return b, nil
	default:
		// 其余全是可空字符串列：nil 落 NULL（清空引用），字符串原样。
		if value == nil {
			return nil, nil
		}
		s, ok := value.(string)
		if !ok {
			return nil, invalidParam("card." + key + " must be a string or null")
		}
		return s, nil
	}
}

// toFloat 接受 JSON 解出来的数值形态。encoding/json 把数字解成 float64，
// 但调用方在 Go 侧直接构造 patch 时给的可能是 int。
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// AppendMessage 追加一条画布对话消息。
func (r *canvasRepo) AppendMessage(ctx context.Context, m domain.Message) (domain.Message, error) {
	if err := requireID("message project_id", m.ProjectID); err != nil {
		return domain.Message{}, err
	}
	switch m.Role {
	case domain.MessageRoleUser, domain.MessageRoleAssistant, domain.MessageRoleSystem:
	default:
		return domain.Message{}, invalidParam("unknown message role " + string(m.Role))
	}
	if m.ID == "" {
		m.ID = uid.New()
	}

	refCards, err := jsonValue(m.RefCardIDs)
	if err != nil {
		return domain.Message{}, invalidParam("message ref_card_ids is not valid JSON: " + err.Error())
	}
	taskIDs, err := jsonValue(m.TaskIDs)
	if err != nil {
		return domain.Message{}, invalidParam("message task_ids is not valid JSON: " + err.Error())
	}
	createdAt := m.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	// 先确认项目还在（且没被软删）再插。
	//
	// 只靠外键有两个问题：一是外键报的是 1452，被翻成 409，而同一个"项目不在"
	// 在 ApplyOps / Snapshot 那边是 404，同一件事给出两种码会逼调用方分别处理；
	// 二是软删只写 deleted_at，行还在，外键照样满足——于是消息能被追加到一个
	// 已经删掉的画布上，谁也看不见它。
	var exists int
	switch err := r.db.QueryRowContext(ctx,
		`SELECT 1 FROM projects WHERE id = ? AND deleted_at IS NULL`, m.ProjectID).Scan(&exists); {
	case isNoRows(err):
		return domain.Message{}, notFound("project", m.ProjectID)
	case err != nil:
		return domain.Message{}, wrap("check project exists", err)
	}

	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO canvas_messages
		 (id, project_id, role, content, skill_id, ref_card_ids, task_ids, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ProjectID, string(m.Role), m.Content, nullString(m.SkillID),
		refCards, taskIDs, createdAt.UTC()); err != nil {
		return domain.Message{}, wrap("append message", err)
	}

	row := r.db.QueryRowContext(ctx,
		`SELECT `+messageColumns+` FROM canvas_messages WHERE id = ?`, m.ID)
	out, err := scanMessage(row)
	if err != nil {
		return domain.Message{}, wrapScan("message", m.ID, "read message", err)
	}
	return out, nil
}
