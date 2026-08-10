package asset

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// defaultTransferTimeout 是单件产物转存的上限。
//
// 视频有几百 MB，慢的上游拉一分钟不奇怪；但没有上限的话，一个挂住的
// 连接会一直占着 worker 和租约，而那条任务的上游 URL 还在倒计时。
// 宁可失败重试，也不要挂到 URL 过期。
const defaultTransferTimeout = 10 * time.Minute

// estimatedArtifactBytes 是产物大小未知时给配额预占用的估值。
//
// 上游多数是 chunked 传输，Content-Length 拿不到，因此预占只能估。
// 估小了会让超配额的用户多写进去一点，估大了会把接近配额的用户挡在门外。
// 取 32MB：一张高清图远小于它，一段 5s 视频大致在这个量级。
// 估得准不准只影响这段占位窗口：预占在落库后原样撤掉，真实字节由落库那一步
// 自己记账，因此估值不会残留进用量。
const estimatedArtifactBytes int64 = 32 << 20

// TransferorDeps 是 Transferor 的依赖。
//
// 全部是接口，因此转存逻辑可以在没有数据库、没有真实上游的情况下测——
// 而「转存必须发生在置 succeeded 之前」这条约束恰恰只能靠测试守住。
type TransferorDeps struct {
	// Blobs 是本平台的二进制存储。
	Blobs Store
	// Assets 落 assets 行。
	Assets store.AssetRepo
	// Uploads 供 Promote 读取临时对象。
	Uploads store.UploadRepo
	// Tasks 供转存时回读任务，拼出 AssetSource（「做同款」要的那一半血缘）。
	//
	// Transferor 接口只给了 taskID，而 AssetSource 需要 prompt / params /
	// input_asset_ids。这些只能回表拿：把它们塞进接口签名会让 Transfer 的
	// 参数列表膨胀成一个隐式的 DTO，而 taskID 已经足以定位它们。
	Tasks store.TaskRepo
	// Drivers 用于按模型协议找到能取产物的驱动。
	// KindBinary / KindGCSURI 必须走驱动（要带上游凭证），
	// KindURL / KindBase64 在没有驱动时可由本包直接取。
	Drivers adapter.Registry
	// Quota 在转存前预占配额。为 nil 时不做配额控制（本包不实现它）。
	Quota QuotaGuard
	// HTTPClient 用于 KindURL 的兜底直取，为 nil 时用带超时的默认客户端。
	HTTPClient *http.Client
	// Logger 为 nil 时取 slog.Default()。
	Logger *slog.Logger
	// Now 供测试注入时钟，为 nil 时取 time.Now。
	Now func() time.Time
}

// transferor 是 Transferor 的默认实现。
type transferor struct {
	blobs   Store
	assets  store.AssetRepo
	uploads store.UploadRepo
	tasks   store.TaskRepo
	drivers adapter.Registry
	quota   QuotaGuard
	client  *http.Client
	log     *slog.Logger
	now     func() time.Time
}

// NewTransferor 组装一个转存器。
func NewTransferor(d TransferorDeps) (Transferor, error) {
	if d.Blobs == nil {
		return nil, fmt.Errorf("asset: TransferorDeps.Blobs 不能为空")
	}
	if d.Assets == nil {
		return nil, fmt.Errorf("asset: TransferorDeps.Assets 不能为空")
	}
	t := &transferor{
		blobs:   d.Blobs,
		assets:  d.Assets,
		uploads: d.Uploads,
		tasks:   d.Tasks,
		drivers: d.Drivers,
		quota:   d.Quota,
		client:  d.HTTPClient,
		log:     d.Logger,
		now:     d.Now,
	}
	if t.client == nil {
		t.client = &http.Client{Timeout: defaultTransferTimeout}
	}
	if t.log == nil {
		t.log = slog.Default()
	}
	if t.now == nil {
		t.now = time.Now
	}
	return t, nil
}

// TransferAll 转存一次任务的全部产物，返回值按 ArtifactRef.Index 升序，
// 该顺序即前端的展示顺序。
//
// 任何一件失败就整体失败，并把本次已经落地的资产回滚掉：留下"4 张里成了 3 张"
// 的任务会让用户以为产物少了一张是模型的问题，而实际上这条任务马上要被判失败
// 并退款——两边对不上比干脆没有更糟。
func (t *transferor) TransferAll(ctx context.Context, userID, taskID string, refs []adapter.ArtifactRef, req adapter.PollRequest) ([]domain.Asset, error) {
	if len(refs) == 0 {
		return nil, fmt.Errorf("asset: 任务 %s 没有可转存的产物", taskID)
	}

	ordered := make([]adapter.ArtifactRef, len(refs))
	copy(ordered, refs)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })

	out := make([]domain.Asset, 0, len(ordered))
	for _, ref := range ordered {
		a, err := t.Transfer(ctx, userID, taskID, ref, req)
		if err != nil {
			t.rollback(out)
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// Transfer 把一件上游产物下载并重新托管到本平台存储，随后落 assets 行。
//
// 顺序是「先写存储，再写库，写库失败就删掉刚写的对象」：反过来（先写库再写存储）
// 会在存储写失败时留下一条指向空对象的 asset 记录，那是用户点开就 404 的卡片；
// 而写库失败留下的无主文件只是占点磁盘，由 admin 的 orphan_files 清理兜底。
// 两种残留里，选那个不会被用户看见的。
func (t *transferor) Transfer(ctx context.Context, userID, taskID string, ref adapter.ArtifactRef, req adapter.PollRequest) (domain.Asset, error) {
	if userID == "" {
		return domain.Asset{}, fmt.Errorf("asset: 转存缺少 user id")
	}

	stream, err := t.open(ctx, ref, req)
	if err != nil {
		return domain.Asset{}, err
	}
	defer func() { _ = stream.Body.Close() }()

	mime := firstNonEmpty(normalizeMIME(ref.MIME), normalizeMIME(stream.MIME), defaultMIME(ref.Type))

	reserved := ref.Bytes
	if reserved <= 0 {
		reserved = stream.Bytes
	}
	if reserved <= 0 {
		reserved = estimatedArtifactBytes
	}
	if t.quota != nil {
		if err := t.quota.Reserve(ctx, userID, reserved); err != nil {
			return domain.Asset{}, err
		}
	}

	assetID := uid.New()
	createdAt := t.now().UTC()
	key := BuildAssetKey(userID, assetID, VariantOriginal, mime, createdAt)

	info, err := t.blobs.Put(ctx, key, stream.Body, mime)
	if err != nil {
		t.release(ctx, userID, reserved)
		return domain.Asset{}, fmt.Errorf("asset: 转存任务 %s 的产物 #%d: %w", taskID, ref.Index, err)
	}

	a := domain.Asset{
		ID:         assetID,
		UserID:     userID,
		Type:       assetTypeOf(ref, mime),
		StorageKey: info.Key,
		MIME:       mime,
		Bytes:      info.Bytes,
		Width:      ref.Width,
		Height:     ref.Height,
		DurationMS: ref.DurationMS,
		Source:     t.sourceOf(ctx, taskID, req),
		CreatedAt:  createdAt,
	}
	if taskID != "" {
		id := taskID
		a.TaskID = &id
	}
	fillImageSize(ctx, t.blobs, t.log, &a)

	saved, err := t.assets.Create(ctx, a)
	if err != nil {
		// 库没写成，刚落地的字节就是无主的，立刻删掉而不是留给兜底清理。
		if delErr := t.blobs.Delete(context.WithoutCancel(ctx), info.Key); delErr != nil {
			t.log.Error("转存回滚失败，留下无主对象", "key", info.Key, "err", delErr)
		}
		t.release(ctx, userID, reserved)
		return domain.Asset{}, fmt.Errorf("asset: 落库任务 %s 的产物 #%d: %w", taskID, ref.Index, err)
	}

	if t.quota != nil {
		if err := t.quota.Commit(ctx, userID, reserved); err != nil {
			// 配额是缓存，Recompute 能修回来；已经安全落地的产物不该为它作废。
			// 这里撤的只是预占，实际字节已经由上面的 Create 记进用量了。
			t.log.Error("配额结算失败", "user_id", userID, "asset_id", saved.ID, "err", err)
		}
	}
	return saved, nil
}

// fillImageSize 在上游没报宽高时，从刚落地的字节里把它读出来。
//
// 资产库的瀑布流按宽高比排版，尺寸缺失时只能退回统一行高，一屏图会全被裁成
// 同一个形状。上游报不报尺寸是它的自由（GPUGeek 的 predictions 就不报），
// 但字节已经在我们手上了，没道理不认。
//
// 这里读的是整个对象而不是固定长度的文件头：GPUGeek 回的 JPEG 前面挂着
// 120KB 上下的 ICC/EXIF，SOF 段排在它后面，任何"取前 N 字节"的窗口都是在赌。
// DecodeConfig 是流式的，读到尺寸就停，不会真把整张图拉进内存。
//
// 读不出来就留空：这是排版的锦上添花，不值得让一次成功的转存失败。
// WebP 不在标准库的解码器里，会走到这条路径上——多一个依赖不如少一点排版精度。
//
// 写成自由函数而不是方法：转存时（还没落库）与回填时（已经落库很久）都要用它，
// 而后者的调用方是 Deriver，两边共用一份实现才不会出现"新图有尺寸、补的没有"。
// 返回是否补上了新东西，供回填方决定要不要写库。
func fillImageSize(ctx context.Context, blobs Store, log *slog.Logger, a *domain.Asset) bool {
	if a.Type != domain.AssetTypeImage || (a.Width != nil && a.Height != nil) {
		return false
	}
	rc, _, err := blobs.Get(ctx, a.StorageKey)
	if err != nil {
		log.Warn("读产物字节失败，宽高留空", "asset_id", a.ID, "err", err)
		return false
	}
	defer func() { _ = rc.Close() }()

	cfg, format, err := image.DecodeConfig(rc)
	if err != nil {
		log.Warn("产物解不出宽高，留空", "asset_id", a.ID, "mime", a.MIME, "err", err)
		return false
	}
	a.Width, a.Height = &cfg.Width, &cfg.Height
	log.Debug("从字节里补出产物宽高", "asset_id", a.ID, "format", format, "w", cfg.Width, "h", cfg.Height)
	return true
}

// Promote 把一个临时 upload 提升为正式 asset。
//
// 实现上是**复制**而不是把 upload 的 key 直接挂到 asset 上：两者的生命周期
// 完全不同（upload 24h 回收、asset 长期保存），共用一个对象意味着回收扫描
// 必须时刻记得"这个还被引用着"，而任何一次疏忽都会删掉别人正在用的输入素材。
// 复制一次的代价换来的是两条生命周期彻底解耦。
//
// 重复调用返回同一件资产：一次任务的多个输入槽可能引用同一个 upload，
// 提升必须幂等，否则同一份素材会被复制成两条 asset，血缘也会跟着分叉。
//
// **这里是上传路径的配额闸**。闸放在提升而不是 POST /api/uploads：
// storage_used_bytes 的定义是"未软删资产的字节之和"（Recompute 正是照这个
// 定义把它重写回去），而 upload 是 24h 后回收的临时对象、没有 assets 行，
// 把它的字节占进那个列会在下一次 Recompute 时被抹掉，闸就成了摆设。
// 提升是这条路径上唯一产生 assets 行的地方，也是唯一的收口——任务提交、
// 画布、重跑都经由 executor 的 resolveOne 走到这里，闸放在更外层就得在每个
// 入口重复一遍，漏一个就是一个绕过口。
func (t *transferor) Promote(ctx context.Context, uploadID string) (domain.Asset, error) {
	if t.uploads == nil {
		return domain.Asset{}, fmt.Errorf("asset: 未配置 UploadRepo，无法提升 upload")
	}
	up, err := t.uploads.Get(ctx, uploadID)
	if err != nil {
		return domain.Asset{}, err
	}
	if up.AssetID != nil && *up.AssetID != "" {
		return t.assets.Get(ctx, *up.AssetID)
	}

	// 预占必须在幂等分支**之后**：重复提升返回的是同一件已落库的资产，它的字节
	// 早已由那次 Create 记进用量，再占一次会让引用同一素材的多个输入槽被重复
	// 计费，然后在配额边缘上误拒一个本该通过的任务。
	//
	// 占的是 upload 自报的字节而不是 Transfer 那样的估值：对象已经在本平台
	// 存储里，大小是确定的，副本与它等大。预占同样在落库后由 Commit 原样撤掉，
	// 实际字节归 AssetRepo.Create——分工与 Transfer 一致。
	reserved := up.Bytes
	if t.quota != nil {
		if err := t.quota.Reserve(ctx, up.UserID, reserved); err != nil {
			return domain.Asset{}, err
		}
	}

	src, _, err := t.blobs.Get(ctx, up.StorageKey)
	if err != nil {
		t.release(ctx, up.UserID, reserved)
		return domain.Asset{}, fmt.Errorf("asset: 读取 upload %s 的对象: %w", uploadID, err)
	}
	defer func() { _ = src.Close() }()

	mime := firstNonEmpty(normalizeMIME(up.MIME), mimeForExt(up.StorageKey))
	assetID := uid.New()
	createdAt := t.now().UTC()
	key := BuildAssetKey(up.UserID, assetID, VariantOriginal, mime, createdAt)

	info, err := t.blobs.Put(ctx, key, src, mime)
	if err != nil {
		t.release(ctx, up.UserID, reserved)
		return domain.Asset{}, fmt.Errorf("asset: 提升 upload %s: %w", uploadID, err)
	}

	a := domain.Asset{
		ID:         assetID,
		UserID:     up.UserID,
		Type:       assetTypeForMIME(mime),
		StorageKey: info.Key,
		MIME:       mime,
		Bytes:      info.Bytes,
		Width:      up.Width,
		Height:     up.Height,
		CreatedAt:  createdAt,
	}
	saved, err := t.assets.Create(ctx, a)
	if err != nil {
		if delErr := t.blobs.Delete(context.WithoutCancel(ctx), info.Key); delErr != nil {
			t.log.Error("提升回滚失败，留下无主对象", "key", info.Key, "err", delErr)
		}
		t.release(ctx, up.UserID, reserved)
		return domain.Asset{}, fmt.Errorf("asset: 落库 upload %s 的提升结果: %w", uploadID, err)
	}
	if t.quota != nil {
		if err := t.quota.Commit(ctx, up.UserID, reserved); err != nil {
			// 与 Transfer 同理：撤的只是预占，实际字节已由 Create 记进用量，
			// 结算失败不该作废一件已经安全落地的资产，漂移由 Recompute 修。
			t.log.Error("配额结算失败", "user_id", up.UserID, "asset_id", saved.ID, "err", err)
		}
	}
	if err := t.uploads.MarkPromoted(ctx, uploadID, saved.ID); err != nil {
		// 标记失败只会让回收扫描在 24h 后误判这份 upload 可删，
		// 而 asset 侧的副本是独立的，素材本身不会丢。
		t.log.Error("标记 upload 已提升失败", "upload_id", uploadID, "asset_id", saved.ID, "err", err)
	}
	return saved, nil
}

// open 把一个 ArtifactRef 变成字节流。
//
// 优先走 driver：KindBinary 的流式端点要带上游凭证、KindGCSURI 要 Google 凭证，
// 这些只有 driver 知道怎么给。同步族（chat / images）没有 AsyncDriver，
// 它们的 Inline 产物是 URL 或 base64，本包能直接处理。
func (t *transferor) open(ctx context.Context, ref adapter.ArtifactRef, req adapter.PollRequest) (adapter.ArtifactStream, error) {
	if drv, ok := t.asyncDriver(req.Model); ok {
		s, err := drv.FetchArtifact(ctx, ref, req)
		if err != nil {
			return adapter.ArtifactStream{}, fmt.Errorf("asset: 取产物 #%d: %w", ref.Index, err)
		}
		if s.Body == nil {
			return adapter.ArtifactStream{}, fmt.Errorf("asset: 驱动返回了空的产物流 (#%d)", ref.Index)
		}
		return s, nil
	}

	switch ref.Kind {
	case adapter.KindURL:
		return t.fetchURL(ctx, ref)
	case adapter.KindBase64:
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ref.Base64))
		if err != nil {
			return adapter.ArtifactStream{}, fmt.Errorf("asset: 解码 base64 产物 #%d: %w", ref.Index, err)
		}
		return adapter.ArtifactStream{
			Body:  io.NopCloser(strings.NewReader(string(raw))),
			MIME:  ref.MIME,
			Bytes: int64(len(raw)),
		}, nil
	default:
		// KindBinary / KindGCSURI 拿不到凭证就取不了，缺驱动是配置问题不是运行时意外。
		return adapter.ArtifactStream{}, fmt.Errorf("asset: 产物形态 %q 需要驱动 %q，但它未注册", ref.Kind, driverName(req.Model))
	}
}

// fetchURL 直取一个有时效的产物直链。
func (t *transferor) fetchURL(ctx context.Context, ref adapter.ArtifactRef) (adapter.ArtifactStream, error) {
	if strings.TrimSpace(ref.URL) == "" {
		return adapter.ArtifactStream{}, fmt.Errorf("asset: 产物 #%d 的 URL 为空", ref.Index)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.URL, nil)
	if err != nil {
		return adapter.ArtifactStream{}, fmt.Errorf("asset: 构造产物请求 #%d: %w", ref.Index, err)
	}
	resp, err := t.client.Do(httpReq)
	if err != nil {
		return adapter.ArtifactStream{}, fmt.Errorf("asset: 拉取产物 #%d: %w", ref.Index, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = resp.Body.Close()
		return adapter.ArtifactStream{}, fmt.Errorf("asset: 拉取产物 #%d 得到 HTTP %d", ref.Index, resp.StatusCode)
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = ref.MIME
	}
	return adapter.ArtifactStream{Body: resp.Body, MIME: mime, Bytes: resp.ContentLength}, nil
}

// asyncDriver 按模型协议查驱动。这里是本包唯一一处碰协议名的地方。
//
// 走 adapter.IsAsync 而不是直接 Async(name)：同一个查找键下可能同时挂着
// 同步与异步两个驱动（mock 即如此），认错了会把同步族的内联产物送去问
// 一个产视频的驱动要字节。
func (t *transferor) asyncDriver(m domain.ModelConfig) (adapter.AsyncDriver, bool) {
	if t.drivers == nil {
		return nil, false
	}
	name := driverName(m)
	if name == "" || !adapter.IsAsync(t.drivers, m) {
		return nil, false
	}
	return t.drivers.Async(name)
}

// sourceOf 拼出资产的「怎么来的」那一半血缘。
//
// 任务读不到时返回只带 model id 的降级版本而不是报错：产物已经在手上，
// 为了一份快照把它扔掉是本末倒置；缺快照只会让「做同款」少回填几个参数。
func (t *transferor) sourceOf(ctx context.Context, taskID string, req adapter.PollRequest) *domain.AssetSource {
	src := &domain.AssetSource{ModelID: req.Model.ID}
	if t.tasks == nil || taskID == "" {
		return src
	}
	task, err := t.tasks.Get(ctx, taskID)
	if err != nil {
		t.log.Warn("回读任务失败，资产 source 降级", "task_id", taskID, "err", err)
		return src
	}
	if task.ModelID != "" {
		src.ModelID = task.ModelID
	}
	src.Prompt = task.Prompt
	src.Params = task.Params
	src.InputAssetIDs = flattenInputs(task.Inputs)
	return src
}

// rollback 撤掉本次调用已经落地的资产。软删而非硬删：血缘边指向它们，
// 硬删会让边断掉，而这批资产的字节由 admin 的 soft_deleted_assets 清理回收。
func (t *transferor) rollback(created []domain.Asset) {
	for _, a := range created {
		ctx := context.WithoutCancel(context.Background())
		if err := t.assets.Delete(ctx, a.ID); err != nil {
			t.log.Error("回滚资产失败", "asset_id", a.ID, "err", err)
		}
	}
}

// release 释放预占的配额。失败只记日志：用量是缓存，Recompute 是它的真相来源。
func (t *transferor) release(ctx context.Context, userID string, bytes int64) {
	if t.quota == nil {
		return
	}
	if err := t.quota.Release(context.WithoutCancel(ctx), userID, bytes); err != nil {
		t.log.Error("释放配额预占失败", "user_id", userID, "bytes", bytes, "err", err)
	}
}

// driverName 由模型配置推出驱动名。video 族看 VideoProtocol，其余看 Family。
func driverName(m domain.ModelConfig) string {
	if m.Family == domain.FamilyVideo {
		if m.VideoProtocol == nil {
			return ""
		}
		return string(*m.VideoProtocol)
	}
	return string(m.Family)
}

// flattenInputs 把按槽分组的输入 id 拍平成一个有序列表。
//
// 槽名不进 AssetSource：血缘关心的是"用了哪些素材"，哪个槽是提交时的形态，
// 重跑时按当时的 capability 重新分配。
func flattenInputs(inputs map[string][]string) []string {
	if len(inputs) == 0 {
		return nil
	}
	slots := make([]string, 0, len(inputs))
	for k := range inputs {
		slots = append(slots, k)
	}
	sort.Strings(slots)

	seen := make(map[string]struct{}, len(inputs))
	out := make([]string, 0, len(inputs))
	for _, s := range slots {
		for _, id := range inputs[s] {
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// assetTypeOf 决定资产的媒体种类。driver 给了就用它的，没给就按 MIME 推。
func assetTypeOf(ref adapter.ArtifactRef, mime string) domain.AssetType {
	if ref.Type != "" {
		return ref.Type
	}
	return assetTypeForMIME(mime)
}

// assetTypeForMIME 按 MIME 主类型推媒体种类，认不出来的按 text 兜底。
func assetTypeForMIME(mime string) domain.AssetType {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return domain.AssetTypeImage
	case strings.HasPrefix(mime, "video/"):
		return domain.AssetTypeVideo
	case strings.HasPrefix(mime, "audio/"):
		return domain.AssetTypeAudio
	default:
		return domain.AssetTypeText
	}
}

// defaultMIME 是上游既没给 Content-Type 也没在 ref 上标 MIME 时的兜底。
func defaultMIME(t domain.AssetType) string {
	switch t {
	case domain.AssetTypeImage:
		return "image/png"
	case domain.AssetTypeVideo:
		return "video/mp4"
	case domain.AssetTypeAudio:
		return "audio/mpeg"
	case domain.AssetTypeText:
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

// firstNonEmpty 返回第一个非空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// randomSuffix 给临时文件名一个不会撞的后缀。
func randomSuffix() string { return uid.Token(8) }

// ErrUnsupported 供派生等尽力而为的步骤标注"这条路本来就走不通"，
// 与真实故障区分开，免得日志里全是可预期的噪音。
var ErrUnsupported = errors.New("asset: 不支持的操作")
