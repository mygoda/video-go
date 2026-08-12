package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/asset"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

func (s *server) handleListAssets(w http.ResponseWriter, r *http.Request) {
	id, err := identity(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	cursor, limit := pagination(r)
	q := r.URL.Query()

	var typ domain.AssetType
	var composed, standalone *bool
	switch v := q.Get("type"); v {
	case "", "all":
	case "short":
		// 短剧 = 成品：合成产出的视频（compose 家族模型）
		typ = domain.AssetTypeVideo
		yes := true
		composed = &yes
	case string(domain.AssetTypeVideo):
		// 视频 = 生成器直接产出的独立视频，排除短剧创作过程物（画布单镜成片等）
		typ = domain.AssetTypeVideo
		only := true
		standalone = &only
	case string(domain.AssetTypeImage):
		// 图片 = 生成器直接产出的独立图片，排除画布里的首帧 / 定妆图等过程物
		typ = domain.AssetTypeImage
		only := true
		standalone = &only
	case string(domain.AssetTypeAudio), string(domain.AssetTypeText):
		typ = domain.AssetType(v)
	default:
		writeError(w, r, errInvalid("type 取值非法: %s", v))
		return
	}

	page, err := s.deps.Assets.List(r.Context(), id.UserID, typ, composed, standalone, q.Get("task_id"), cursor, limit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pageOf(s.decorateAssets(page.Items), page.NextCursor))
}

func (s *server) handleGetAsset(w http.ResponseWriter, r *http.Request) {
	a, err := s.ownedAsset(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.decorateAssets([]domain.Asset{a})[0])
}

func (s *server) handleDeleteAsset(w http.ResponseWriter, r *http.Request) {
	a, err := s.ownedAsset(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// 软删。字节留给 admin 的清理任务回收——硬删会立刻切断指向它的血缘边，
	// 而血缘正是「做同款」与「单卡重跑」的依据。
	if err := s.deps.Assets.Delete(r.Context(), a.ID); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleAssetLineage(w http.ResponseWriter, r *http.Request) {
	a, err := s.ownedAsset(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	dir := domain.LineageBoth
	switch v := r.URL.Query().Get("direction"); v {
	case "", string(domain.LineageBoth):
	case string(domain.LineageAncestors):
		dir = domain.LineageAncestors
	case string(domain.LineageDescendants):
		dir = domain.LineageDescendants
	default:
		writeError(w, r, errInvalid("direction 取值非法: %s", v))
		return
	}
	depth := queryInt(r, "depth", 3)

	graph, err := s.deps.Lineage.Graph(r.Context(), a.ID, dir, depth)
	if err != nil {
		writeError(w, r, err)
		return
	}
	graph.Nodes = s.decorateAssets(graph.Nodes)
	if graph.Edges == nil {
		graph.Edges = []domain.LineageEdge{}
	}
	writeJSON(w, http.StatusOK, graph)
}

// handleAssetContent 回源资产字节。
//
// **这个端点不走 Bearer 鉴权**：<img src> / <video src> 带不上 Authorization 头，
// 要它走鉴权就只能把 token 塞进 query，而 query 会进访问日志、Referer 与浏览器
// 历史。资产 id 是不可猜的 UUID，配合 Cache-Control: private 已经够用。
func (s *server) handleAssetContent(w http.ResponseWriter, r *http.Request) {
	assetID, err := pathID(r, "assetId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	a, err := s.deps.Assets.Get(r.Context(), assetID)
	if err != nil {
		writeError(w, r, err)
		return
	}

	key := a.StorageKey
	switch asset.Variant(r.URL.Query().Get("variant")) {
	case asset.VariantThumb512:
		if a.ThumbKey != nil {
			key = *a.ThumbKey
		}
	case asset.VariantPoster:
		if a.PosterKey != nil {
			key = *a.PosterKey
		}
	}
	// 派生规格缺席时回落到原图而不是 404：缩略图允许生成失败（见 asset.Deriver），
	// 让前端因此显示一个破图是把一个可降级的问题变成了故障。
	s.streamBlob(w, r, key, a.MIME)
}

// streamBlob 把一个存储对象写进响应，支持单段 Range。
//
// 支持 Range 不是锦上添花：不支持时浏览器每拖一次视频进度条都要从头下载整个文件。
func (s *server) streamBlob(w http.ResponseWriter, r *http.Request, key, mime string) {
	ctx := r.Context()
	info, err := s.deps.Blobs.Stat(ctx, key)
	if err != nil {
		writeError(w, r, errNotFound("资产内容"))
		return
	}
	if mime == "" {
		mime = info.MIME
	}

	h := w.Header()
	h.Set("Content-Type", mime)
	// 资产内容不可变（同一个 id + variant 永远是同一份字节），可以长缓存。
	// private 是因为它属于某个用户，中间层不该替所有人存一份。
	h.Set("Cache-Control", "private, max-age=31536000, immutable")

	// 配了 X-Accel 前缀就把字节下发交给 nginx：Go 只做鉴权 + 解析 key（上面的
	// Stat 已确认文件在），文件本身由 nginx 从磁盘 sendfile 直发，range、
	// Content-Length、Content-Range 都由它按请求补，后端不再 io.Copy 整段视频。
	if prefix := s.deps.Config.AssetXAccelPrefix; prefix != "" {
		h.Set("X-Accel-Redirect", prefix+"/"+key)
		return
	}

	h.Set("Accept-Ranges", "bytes")

	offset, length, ok := parseRange(r.Header.Get("Range"), info.Bytes)
	if !ok {
		h.Set("Content-Length", strconv.FormatInt(info.Bytes, 10))
		rc, _, err := s.deps.Blobs.Get(ctx, key)
		if err != nil {
			writeError(w, r, errInternal(err))
			return
		}
		defer rc.Close()
		w.WriteHeader(http.StatusOK)
		copyBlob(w, rc)
		return
	}

	rc, _, err := s.deps.Blobs.GetRange(ctx, key, offset, length)
	if err != nil {
		writeError(w, r, errInternal(err))
		return
	}
	defer rc.Close()
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	h.Set("Content-Range", "bytes "+strconv.FormatInt(offset, 10)+"-"+
		strconv.FormatInt(offset+length-1, 10)+"/"+strconv.FormatInt(info.Bytes, 10))
	w.WriteHeader(http.StatusPartialContent)
	copyBlob(w, rc)
}

func copyBlob(w http.ResponseWriter, rc io.Reader) {
	if _, err := io.Copy(w, rc); err != nil {
		// 头早发出去了，改不了状态码。客户端中途断开是常态（拖进度条、
		// 关标签页），记 debug 而不是 error。
		slog.Debug("写资产内容中断", "err", err)
	}
}

// parseRange 解析单段 bytes=start-end。多段 Range 不支持——
// 浏览器播视频只发单段，为多段写一套 multipart 响应是纯粹的成本。
func parseRange(header string, size int64) (offset, length int64, ok bool) {
	if header == "" || size <= 0 || !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	startRaw, endRaw := spec[:dash], spec[dash+1:]

	switch {
	case startRaw == "":
		// bytes=-N：最后 N 字节。
		n, err := strconv.ParseInt(endRaw, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, n, true
	default:
		start, err := strconv.ParseInt(startRaw, 10, 64)
		if err != nil || start < 0 || start >= size {
			return 0, 0, false
		}
		end := size - 1
		if endRaw != "" {
			if end, err = strconv.ParseInt(endRaw, 10, 64); err != nil {
				return 0, 0, false
			}
		}
		if end >= size {
			end = size - 1
		}
		if end < start {
			return 0, 0, false
		}
		return start, end - start + 1, true
	}
}

// ownedAsset 取出路径上的资产并确认它属于调用方。
func (s *server) ownedAsset(r *http.Request) (domain.Asset, error) {
	id, err := identity(r)
	if err != nil {
		return domain.Asset{}, err
	}
	assetID, err := pathID(r, "assetId")
	if err != nil {
		return domain.Asset{}, err
	}
	a, err := s.deps.Assets.Get(r.Context(), assetID)
	if err != nil {
		return domain.Asset{}, err
	}
	if err := requireOwner(id, a.UserID, "资产"); err != nil {
		return domain.Asset{}, err
	}
	return a, nil
}
