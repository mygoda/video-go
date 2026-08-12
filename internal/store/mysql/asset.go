package mysql

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// assetRepo 是 store.AssetRepo 的 MySQL 实现。
type assetRepo struct{ db *sql.DB }

// NewAssetRepo 构造 store.AssetRepo。
func NewAssetRepo(db *sql.DB) store.AssetRepo { return &assetRepo{db: db} }

// assetColumns 是资产的完整投影。
//
// Original / Thumb512 / Poster 三个 URL 字段不在这里：它们由 httpapi 层按
// storage_key / thumb_key / poster_key 投影出来（见 domain.Asset 的注释）。
const assetColumns = `id, user_id, task_id, type, storage_key, thumb_key, poster_key,
	mime, bytes, width, height, duration_ms, ` + "`source`" + `, deleted_at, created_at`

func scanAsset(s rowScanner) (domain.Asset, error) {
	var (
		a          domain.Asset
		taskID     sql.NullString
		thumbKey   sql.NullString
		posterKey  sql.NullString
		width      sql.NullInt64
		height     sql.NullInt64
		durationMS sql.NullInt64
		sourceRaw  []byte
		deletedAt  sql.NullTime
	)
	err := s.Scan(
		&a.ID, &a.UserID, &taskID, &a.Type, &a.StorageKey, &thumbKey, &posterKey,
		&a.MIME, &a.Bytes, &width, &height, &durationMS, &sourceRaw, &deletedAt, &a.CreatedAt,
	)
	if err != nil {
		return domain.Asset{}, err
	}
	a.TaskID = strPtr(taskID)
	a.ThumbKey = strPtr(thumbKey)
	a.PosterKey = strPtr(posterKey)
	a.Width = intPtr(width)
	a.Height = intPtr(height)
	a.DurationMS = intPtr(durationMS)
	a.DeletedAt = timePtr(deletedAt)
	a.CreatedAt = a.CreatedAt.UTC()

	if len(sourceRaw) > 0 && string(sourceRaw) != "null" {
		var src domain.AssetSource
		if err := scanJSON(sourceRaw, &src); err != nil {
			return domain.Asset{}, err
		}
		a.Source = &src
	}
	return a, nil
}

// Create 落库一件资产，并在同一事务里把字节数计进用户的存储用量。
//
// 用量与资产行同事务，理由与积分流水一致：分两步写，中间崩一次就会留下
// 一件不占配额的资产，用户的配额慢慢地越用越"空"，最后配额形同虚设。
func (r *assetRepo) Create(ctx context.Context, a domain.Asset) (domain.Asset, error) {
	if err := requireID("asset user_id", a.UserID); err != nil {
		return domain.Asset{}, err
	}
	if a.StorageKey == "" {
		return domain.Asset{}, invalidParam("asset storage_key must not be empty")
	}
	if a.MIME == "" {
		return domain.Asset{}, invalidParam("asset mime must not be empty")
	}
	switch a.Type {
	case domain.AssetTypeImage, domain.AssetTypeVideo, domain.AssetTypeAudio, domain.AssetTypeText:
	default:
		return domain.Asset{}, invalidParam("unknown asset type " + string(a.Type))
	}
	if a.ID == "" {
		a.ID = uid.New()
	}
	if a.Bytes < 0 {
		return domain.Asset{}, invalidParam("asset bytes must not be negative")
	}

	// source 是 NOT NULL 列。没有 Source 的资产在设计上不该存在（「做同款」与
	// 「单卡重跑」都读它），但 Promote 上来的输入素材确实没有生成参数，
	// 那种情况落一个空对象而不是拒绝落库。
	source, err := jsonRequired(a.Source)
	if err != nil {
		return domain.Asset{}, invalidParam("asset source is not valid JSON: " + err.Error())
	}
	if string(source) == "null" {
		source = []byte("{}")
	}

	createdAt := a.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	err = withTx(ctx, r.db, func(tx *sql.Tx) error {
		// **用量更新必须排在 INSERT 前面。** 反过来写会死锁：assets 有指向
		// users 的外键，INSERT 时 InnoDB 要对那一行 users 加 S 锁，随后的
		// UPDATE 又要 X 锁，单事务内是一次 S→X 升级。同一个用户的两件产物
		// 并发落库时两个事务都先拿到 S、又都等对方放掉 S 才升得了 X，
		// 互等成环，InnoDB 检出死锁回滚其中一个（Error 1213）。批量出首帧
		// 一次排 3 镜、出片一次排 N 段，撞上它是必然而不是偶然。
		//
		// 先 UPDATE 就没有升级这一步：X 锁一次到手，后面 INSERT 做外键检查
		// 要的 S 锁被同事务已持有的 X 覆盖，直接放行。所有事务的取锁顺序
		// 一致，环也就构不成。
		//
		// 用户不存在时这条 UPDATE 影响 0 行且不报错——拦下它的仍然是下面
		// INSERT 的外键，不必在这里再判一次行数。
		if a.Bytes > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET storage_used_bytes = storage_used_bytes + ? WHERE id = ?`,
				a.Bytes, a.UserID); err != nil {
				return wrap("charge storage usage", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO assets
			 (id, user_id, task_id, type, storage_key, thumb_key, poster_key,
			  mime, bytes, width, height, duration_ms, `+"`source`"+`, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, a.UserID, nullString(a.TaskID), string(a.Type), a.StorageKey,
			nullString(a.ThumbKey), nullString(a.PosterKey),
			a.MIME, a.Bytes, nullInt(a.Width), nullInt(a.Height), nullInt(a.DurationMS),
			source, createdAt.UTC()); err != nil {
			return wrap("create asset", err)
		}
		return nil
	})
	if err != nil {
		return domain.Asset{}, err
	}
	return r.Get(ctx, a.ID)
}

// Get 读一件资产。
//
// **软删的读不出来。** 用户删掉的资产在他自己看来就该是不存在的；
// 保留行只是为了不断血缘边（见 AssetRepo 的接口注释），不是为了还能被读到。
// 清理任务要看软删行走的是 ListSoftDeleted。
func (r *assetRepo) Get(ctx context.Context, id string) (domain.Asset, error) {
	if err := requireID("asset id", id); err != nil {
		return domain.Asset{}, err
	}
	row := r.db.QueryRowContext(ctx,
		`SELECT `+assetColumns+` FROM assets WHERE id = ? AND deleted_at IS NULL`, id)
	a, err := scanAsset(row)
	if err != nil {
		return domain.Asset{}, wrapScan("asset", id, "get asset", err)
	}
	return a, nil
}

// List 是资产瀑布流，按 (created_at DESC, id DESC) 游标分页，
// 对齐 idx_assets_user_created_id。
//
// 这里正是包注释里那条"分页一律游标"的由来：用户浏览瀑布流的同时任务还在
// 不断产出新资产，offset 分页会让下一页重复或漏掉条目。
//
// 不指定 type 时排除 text：assets 表同时兼着两个身份——管线的产物存放处，
// 和用户的资产库。text 只属于前者（分镜脚本的对话回复、合成任务的片段清单），
// 它没有可看的画面、下载和「做同款」对它也没有意义，混进瀑布流就是一块裂图。
// 过滤放在 SQL 里而不是取出来再筛：游标分页按行数切页，筛在外面会让每页
// 少几条、next_cursor 与实际条数对不上。显式传 type=text 仍然取得到。
func (r *assetRepo) List(ctx context.Context, userID string, typ domain.AssetType, composed, standalone *bool, taskID, cursor string, limit int) (store.Page[domain.Asset], error) {
	if err := requireID("user id", userID); err != nil {
		return store.Page[domain.Asset]{}, err
	}
	limit = clampLimit(limit)

	where := []string{`user_id = ?`, `deleted_at IS NULL`}
	args := []any{userID}
	if typ != "" {
		where = append(where, `type = ?`)
		args = append(args, string(typ))
	} else {
		where = append(where, `type <> 'text'`)
	}
	// composed 区分「短剧成品」与普通视频：成品由 compose 家族模型产出，其
	// source.model_id 落在 models(protocol_family='compose') 里。nil = 不筛。
	if composed != nil {
		expr := `COALESCE(JSON_UNQUOTE(JSON_EXTRACT(source, '$.model_id')), '')`
		if *composed {
			where = append(where, expr+` IN (SELECT id FROM models WHERE protocol_family = 'compose')`)
		} else {
			where = append(where, expr+` NOT IN (SELECT id FROM models WHERE protocol_family = 'compose')`)
		}
	}
	// standalone 区分「生成器直接产出」与「短剧创作过程中间物」：画布里产的图
	// （首帧、定妆图）和单镜成片都来自带 canvas_id 的任务；生成器出的图 / 视频
	// 与上传（无 task）都不带。true=只留独立产出，把短剧过程物挡在资产库外。
	if standalone != nil {
		if *standalone {
			where = append(where, `(task_id IS NULL OR task_id IN (SELECT id FROM tasks WHERE canvas_id IS NULL))`)
		} else {
			where = append(where, `task_id IN (SELECT id FROM tasks WHERE canvas_id IS NOT NULL)`)
		}
	}
	if taskID != "" {
		where = append(where, `task_id = ?`)
		args = append(args, taskID)
	}
	if c, ok := decodeCursor(cursor); ok {
		where = append(where, `(created_at < ? OR (created_at = ? AND id < ?))`)
		args = append(args, c.At, c.At, c.ID)
	}
	args = append(args, limit+1)

	items, err := r.queryAssets(ctx,
		`SELECT `+assetColumns+` FROM assets WHERE `+strings.Join(where, ` AND `)+
			` ORDER BY created_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return store.Page[domain.Asset]{}, err
	}

	items, hasMore := pageSlice(items, limit)
	page := store.Page[domain.Asset]{Items: items}
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		page.NextCursor = encodeTimeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// ListByTask 返回一次任务的全部产物，按创建顺序（= 上游的产物顺序）。
//
// 不分页：一次任务最多出几张图，且调用方要的就是"这次生成的全部结果"。
//
// 与 List 一样排除 text——调用方是任务卡片，它把每件产物渲染成一张图片。
// 只产出 text 的任务（分镜、合成清单）因此显示成一张没有产物的卡片，
// 那正是它的真相：中间物已经喂给下一步了，没有可看的东西交给用户。
func (r *assetRepo) ListByTask(ctx context.Context, taskID string) ([]domain.Asset, error) {
	if err := requireID("task id", taskID); err != nil {
		return nil, err
	}
	return r.queryAssets(ctx,
		`SELECT `+assetColumns+` FROM assets
		 WHERE task_id = ? AND deleted_at IS NULL AND type <> 'text'
		 ORDER BY created_at ASC, id ASC`, taskID)
}

func (r *assetRepo) queryAssets(ctx context.Context, query string, args ...any) ([]domain.Asset, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("query assets", err)
	}
	defer rows.Close()

	var out []domain.Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, wrap("scan asset", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("query assets", err)
	}
	return out, nil
}

// Delete 是**软删**：打删除标记 + 释放配额，字节留给 admin 的清理任务回收。
//
// 硬删会让指向它的血缘边立刻断掉，而血缘是「做同款」与「单卡重跑」的依据——
// 误删一件资产就会连带毁掉一整条创作链的可追溯性。配额则必须立刻释放：
// 用户点了删除却发现可用空间没变，那个删除在他看来就是没生效。
//
// 标记与配额释放同事务，且 WHERE 里带 deleted_at IS NULL：重复删同一件资产
// 只会释放一次配额，否则连点两下删除键就能把用量刷成负数。
func (r *assetRepo) Delete(ctx context.Context, id string) error {
	if err := requireID("asset id", id); err != nil {
		return err
	}

	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		var (
			userID string
			bytes  int64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT user_id, bytes FROM assets WHERE id = ? AND deleted_at IS NULL FOR UPDATE`,
			id).Scan(&userID, &bytes)
		if isNoRows(err) {
			return notFound("asset", id)
		}
		if err != nil {
			return wrap("lock asset for delete", err)
		}

		res, err := tx.ExecContext(ctx,
			`UPDATE assets SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
			time.Now().UTC(), id)
		if err != nil {
			return wrap("soft delete asset", err)
		}
		n, err := affected(res, "soft delete asset")
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound("asset", id)
		}

		if bytes > 0 {
			// GREATEST(0, ...) 兜住历史漂移：用量缓存要是因为某次崩溃偏低了，
			// 释放不该把它压成负数——负用量会让配额检查永远通过。
			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET storage_used_bytes = GREATEST(0, storage_used_bytes - ?) WHERE id = ?`,
				bytes, userID); err != nil {
				return wrap("release storage usage", err)
			}
		}
		return nil
	})
}

// AddLineage 批量写入血缘边。
//
// 用 INSERT IGNORE 而不是普通 INSERT：同一条边被重复记录（重跑同一个任务、
// 一次任务的产物互为输入）在业务上是幂等的，主键冲突不该让整批写入失败。
func (r *assetRepo) AddLineage(ctx context.Context, edges []domain.LineageEdge) error {
	if len(edges) == 0 {
		return nil
	}
	for _, e := range edges {
		if e.From == "" || e.To == "" {
			return invalidParam("lineage edge requires both from and to")
		}
		switch e.Relation {
		case domain.LineageInput, domain.LineageRerunOf, domain.LineageComposedFrom:
		default:
			return invalidParam("unknown lineage relation " + string(e.Relation))
		}
		// 自环会让任何深度限制都失去意义（遍历原地打转），且语义上讲不通。
		if e.From == e.To {
			return invalidParam("lineage edge must not point at itself: " + e.From)
		}
	}

	// 一条 INSERT 带 n 组占位符：占位符个数随 n 变，但每一组都是固定的
	// "(?, ?, ?)" 文本，值全部走绑定。
	placeholders := make([]string, len(edges))
	args := make([]any, 0, len(edges)*3)
	for i, e := range edges {
		placeholders[i] = "(?, ?, ?)"
		args = append(args, e.From, e.To, string(e.Relation))
	}

	_, err := r.db.ExecContext(ctx,
		`INSERT IGNORE INTO asset_lineage (parent_asset_id, child_asset_id, relation)
		 VALUES `+strings.Join(placeholders, ", "), args...)
	if err != nil {
		return wrap("add lineage", err)
	}
	return nil
}

// Lineage 按方向与深度遍历血缘图。
//
// 逐层查而不是一条递归 CTE：血缘图**可能带环**（A 生成 B，B 又被用来重跑 A
// 的同款），递归 CTE 在环上要靠 cycle 子句或路径去重才停得下来，而逐层 BFS
// 天然带着已访问集合，环在第二次遇到时就被剪掉。层数上限 5 由 openapi 定，
// 因此最多 5 轮往返，代价可控。
//
// 节点集合里**包含软删的资产**：血缘是"这条链是怎么来的"的记录，
// 中间某件产物被用户删了不该让整条链断成两截。是否在 UI 上灰掉由上层决定。
func (r *assetRepo) Lineage(ctx context.Context, assetID string, dir domain.LineageDirection, depth int) (domain.LineageGraph, error) {
	if err := requireID("asset id", assetID); err != nil {
		return domain.LineageGraph{}, err
	}
	const maxDepth = 5
	if depth <= 0 {
		depth = 1
	}
	if depth > maxDepth {
		depth = maxDepth
	}
	switch dir {
	case domain.LineageAncestors, domain.LineageDescendants, domain.LineageBoth:
	case "":
		dir = domain.LineageBoth
	default:
		return domain.LineageGraph{}, invalidParam("unknown lineage direction " + string(dir))
	}

	// 起点必须存在（含软删）：对一个不存在的 id 返回空图会让调用方
	// 分不清"没有血缘"和"这个 id 是错的"。
	var exists int
	switch err := r.db.QueryRowContext(ctx, `SELECT 1 FROM assets WHERE id = ?`, assetID).Scan(&exists); {
	case isNoRows(err):
		return domain.LineageGraph{}, notFound("asset", assetID)
	case err != nil:
		return domain.LineageGraph{}, wrap("check asset exists", err)
	}

	visited := map[string]bool{assetID: true}
	edgeSeen := map[domain.LineageEdge]bool{}
	var edges []domain.LineageEdge

	frontier := []string{assetID}
	for level := 0; level < depth && len(frontier) > 0; level++ {
		var next []string

		if dir == domain.LineageAncestors || dir == domain.LineageBoth {
			found, err := r.lineageStep(ctx, frontier, true)
			if err != nil {
				return domain.LineageGraph{}, err
			}
			for _, e := range found {
				if !edgeSeen[e] {
					edgeSeen[e] = true
					edges = append(edges, e)
				}
				if !visited[e.From] {
					visited[e.From] = true
					next = append(next, e.From)
				}
			}
		}

		if dir == domain.LineageDescendants || dir == domain.LineageBoth {
			found, err := r.lineageStep(ctx, frontier, false)
			if err != nil {
				return domain.LineageGraph{}, err
			}
			for _, e := range found {
				if !edgeSeen[e] {
					edgeSeen[e] = true
					edges = append(edges, e)
				}
				if !visited[e.To] {
					visited[e.To] = true
					next = append(next, e.To)
				}
			}
		}

		frontier = next
	}

	ids := make([]string, 0, len(visited))
	for id := range visited {
		ids = append(ids, id)
	}
	nodes, err := r.getAssetsByIDs(ctx, ids)
	if err != nil {
		return domain.LineageGraph{}, err
	}
	return domain.LineageGraph{Nodes: nodes, Edges: edges}, nil
}

// lineageStep 查一层边。up=true 追父（ancestors），false 追子（descendants）。
//
// 两个方向的 SQL 是两条独立的常量语句而不是拼出来的一条：走的索引也不同
// （追父用主键前缀，追子用 idx_asset_lineage_parent）。
func (r *assetRepo) lineageStep(ctx context.Context, ids []string, up bool) ([]domain.LineageEdge, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	var query string
	if up {
		query = `SELECT parent_asset_id, child_asset_id, relation FROM asset_lineage
			WHERE child_asset_id IN (` + placeholders + `)`
	} else {
		query = `SELECT parent_asset_id, child_asset_id, relation FROM asset_lineage
			WHERE parent_asset_id IN (` + placeholders + `)`
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("query lineage", err)
	}
	defer rows.Close()

	var out []domain.LineageEdge
	for rows.Next() {
		var e domain.LineageEdge
		if err := rows.Scan(&e.From, &e.To, &e.Relation); err != nil {
			return nil, wrap("scan lineage edge", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("query lineage", err)
	}
	return out, nil
}

// getAssetsByIDs 批量取资产（含软删，见 Lineage 的注释）。
func (r *assetRepo) getAssetsByIDs(ctx context.Context, ids []string) ([]domain.Asset, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return r.queryAssets(ctx,
		`SELECT `+assetColumns+` FROM assets WHERE id IN (`+placeholders+`)
		 ORDER BY created_at ASC, id ASC`, args...)
}

// SumBytes 汇总某用户未软删资产的字节数与件数。
//
// 逐条求和而不是读 users.storage_used_bytes：那个列是缓存，本方法是
// asset.QuotaGuard.Recompute 用来校正它的真相来源。
func (r *assetRepo) SumBytes(ctx context.Context, userID string) (int64, int, error) {
	if err := requireID("user id", userID); err != nil {
		return 0, 0, err
	}
	var (
		total sql.NullInt64
		count int
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(bytes), 0), COUNT(*) FROM assets
		 WHERE user_id = ? AND deleted_at IS NULL`, userID).Scan(&total, &count)
	if err != nil {
		return 0, 0, wrap("sum asset bytes", err)
	}
	return total.Int64, count, nil
}

// SetVariants 落库派生规格的存储键。
//
// 派生（缩略图 / 封面帧）是转存之后的独立一步且允许失败，因此它不能挤进
// Create 的参数里——那会逼着调用方在缩略图还没生成时就把 asset 攥在手上不落库，
// 而转存成功却没落库就是一件永远找不回来的产物。
//
// 传 nil 表示"这一档没有"，会把列写成 NULL；派生失败时原样保持 NULL，
// 上层回落到 Original。
func (r *assetRepo) SetVariants(ctx context.Context, assetID string, thumbKey, posterKey *string) error {
	if err := requireID("asset id", assetID); err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE assets SET thumb_key = ?, poster_key = ? WHERE id = ? AND deleted_at IS NULL`,
		nullString(thumbKey), nullString(posterKey), assetID)
	if err != nil {
		return wrap("set asset variants", err)
	}
	n, err := affected(res, "set asset variants")
	if err != nil {
		return err
	}
	if n == 0 {
		// 0 行有两种可能：写进去的 key 与库里已有的值逐位相同（重试落同一份
		// 派生结果，属于正常），或者这条资产压根不在（软删了 / ID 是错的）。
		// 后者必须报出来——静默成功会让 Deriver 以为缩略图已经挂上去了，
		// 而前端永远拿不到那个 URL，也没人会去重试。
		var exists int
		switch err := r.db.QueryRowContext(ctx,
			`SELECT 1 FROM assets WHERE id = ? AND deleted_at IS NULL`, assetID).Scan(&exists); {
		case isNoRows(err):
			return notFound("asset", assetID)
		case err != nil:
			return wrap("check asset exists", err)
		}
	}
	return nil
}

// SetMedia 补写宽高与时长。
//
// 与 SetVariants 同理，它不能挤进 Create：视频的宽高时长要解开容器才知道，
// 而解容器与抽封面帧是同一步，发生在字节已经落地之后。
//
// 每一项都用 COALESCE(?, 列) 写：传 nil 表示"这一项仍然不知道"，
// 保持列的原值。直接赋值会让一次只探出时长的补写把上游报过的宽高抹成 NULL。
//
// 与 SetVariants 不同，这里不追查 0 行：宽高时长是排版用的锦上添花，
// 补不上只是瀑布流少一档参差，没有"前端永远拿不到 URL"那种静默失效。
func (r *assetRepo) SetMedia(ctx context.Context, assetID string, width, height, durationMS *int) error {
	if err := requireID("asset id", assetID); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE assets
		 SET width = COALESCE(?, width), height = COALESCE(?, height),
		     duration_ms = COALESCE(?, duration_ms)
		 WHERE id = ? AND deleted_at IS NULL`,
		nullInt(width), nullInt(height), nullInt(durationMS), assetID)
	if err != nil {
		return wrap("set asset media", err)
	}
	return nil
}

// ListMissingMedia 列出宽高（视频还包括时长）缺失的资产，供回填任务补写。
//
// 只挑 image / video：text 与 audio 本来就没有宽高，把它们列进来会让回填
// 任务每跑一次都对同一批行做一次无用的探测，且永远清不空。
func (r *assetRepo) ListMissingMedia(ctx context.Context, limit int) ([]domain.Asset, error) {
	limit = clampLimit(limit)
	return r.queryAssets(ctx,
		`SELECT `+assetColumns+` FROM assets
		 WHERE deleted_at IS NULL AND type IN ('image', 'video')
		   AND (width IS NULL OR height IS NULL
		        OR (type = 'video' AND duration_ms IS NULL))
		 ORDER BY created_at ASC, id ASC LIMIT ?`, limit)
}

// ListSoftDeleted 列出软删早于 before 的资产，供 admin 清理任务回收字节。
func (r *assetRepo) ListSoftDeleted(ctx context.Context, before time.Time, limit int) ([]domain.Asset, error) {
	limit = clampLimit(limit)
	return r.queryAssets(ctx,
		`SELECT `+assetColumns+` FROM assets
		 WHERE deleted_at IS NOT NULL AND deleted_at < ?
		 ORDER BY deleted_at ASC LIMIT ?`, before.UTC(), limit)
}

// HardDelete 物理删除一条资产记录，血缘边由外键级联删掉。
//
// **只允许 admin 的清理任务调用**，用户侧的删除一律是 Delete 的软删。
// 这里不再动配额：软删那一步已经释放过了，再减一次会把用量刷成负数。
//
// 只接受已软删的行：一条从没被软删过的资产被硬删掉，指向它的血缘边会
// 静默消失，那正是软删机制要防的事故。
func (r *assetRepo) HardDelete(ctx context.Context, id string) error {
	if err := requireID("asset id", id); err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM assets WHERE id = ? AND deleted_at IS NOT NULL`, id)
	if err != nil {
		return wrap("hard delete asset", err)
	}
	n, err := affected(res, "hard delete asset")
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}

	var deletedAt sql.NullTime
	switch err := r.db.QueryRowContext(ctx, `SELECT deleted_at FROM assets WHERE id = ?`, id).Scan(&deletedAt); {
	case isNoRows(err):
		return notFound("asset", id)
	case err != nil:
		return wrap("read asset deleted_at", err)
	}
	return conflict("asset "+id+" is not soft-deleted; hard delete is only for the cleanup path", nil)
}

// StorageUsage 汇总全站存储用量，供 admin 总览。
//
// UploadPendingBytes 与 SoftDeletedBytes 单列，是因为这两块是**可回收**的：
// 只给一个总数，管理员看到磁盘满了也不知道有多少是能清掉的（见 domain 注释）。
func (r *assetRepo) StorageUsage(ctx context.Context, topN int) (domain.StorageUsage, error) {
	var usage domain.StorageUsage

	const totalsQ = `SELECT
		  COALESCE(SUM(CASE WHEN deleted_at IS NULL     THEN bytes END), 0),
		  COALESCE(SUM(CASE WHEN deleted_at IS NULL     THEN 1     END), 0),
		  COALESCE(SUM(CASE WHEN deleted_at IS NOT NULL THEN bytes END), 0)
		FROM assets`
	if err := r.db.QueryRowContext(ctx, totalsQ).Scan(
		&usage.TotalBytes, &usage.AssetCount, &usage.SoftDeletedBytes); err != nil {
		return domain.StorageUsage{}, wrap("storage usage totals", err)
	}

	const uploadsQ = `SELECT COALESCE(SUM(bytes), 0) FROM uploads
		WHERE status = 'pending' AND promoted_asset_id IS NULL`
	if err := r.db.QueryRowContext(ctx, uploadsQ).Scan(&usage.UploadPendingBytes); err != nil {
		return domain.StorageUsage{}, wrap("storage usage uploads", err)
	}

	if topN <= 0 {
		return usage, nil
	}
	if topN > maxLimit {
		topN = maxLimit
	}

	const topQ = `SELECT u.id, u.username,
		  COALESCE(SUM(CASE WHEN a.deleted_at IS NULL THEN a.bytes END), 0) AS used,
		  u.storage_quota_bytes
		FROM users u LEFT JOIN assets a ON a.user_id = u.id
		GROUP BY u.id, u.username, u.storage_quota_bytes
		ORDER BY used DESC, u.id ASC LIMIT ?`
	rows, err := r.db.QueryContext(ctx, topQ, topN)
	if err != nil {
		return domain.StorageUsage{}, wrap("storage usage top users", err)
	}
	defer rows.Close()

	for rows.Next() {
		var e domain.StorageUserUsage
		if err := rows.Scan(&e.UserID, &e.Username, &e.UsedBytes, &e.QuotaBytes); err != nil {
			return domain.StorageUsage{}, wrap("scan storage usage", err)
		}
		usage.TopUsers = append(usage.TopUsers, e)
	}
	if err := rows.Err(); err != nil {
		return domain.StorageUsage{}, wrap("storage usage top users", err)
	}
	return usage, nil
}
