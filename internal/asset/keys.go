package asset

import (
	"path"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// maxSegment 是单个 key 片段的字符上限。
//
// 片段来源全是平台自己生成的 id（UUID 36 字符），给到 64 是留给未来
// 可能变长的 id 形态。截断本身也是一道防线：上游若返回一个畸形长的
// 文件名，它不会变成一个几 KB 长的路径把文件系统撑到极限。
const maxSegment = 64

// BuildAssetKey 返回一件资产某个规格的存储键。
//
// 形态为 users/{user_id}/{yyyy}/{mm}/{asset_id}/{variant}{ext}。
// 按年月分层不是为了好看：单目录塞进几十万个 asset 目录会让 FS 的
// 目录项查找退化，而这个平台的资产是只增不减地攒着的。
//
// **key 的每一段都只由平台生成的 id 与固定枚举拼出，永远不含用户输入的文本。**
// 提示词、原始文件名这类东西一旦进入 key，路径穿越与编码问题就全来了；
// 想避开它们只能靠一层层过滤，而不产生它们才是结构上不可能出错的做法。
func BuildAssetKey(userID, assetID string, v Variant, mime string, at time.Time) string {
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()
	return path.Join(
		"users",
		sanitizeSegment(userID),
		at.Format("2006"),
		at.Format("01"),
		sanitizeSegment(assetID),
		sanitizeSegment(string(v))+extForMIME(mime),
	)
}

// BuildUploadKey 返回一个临时上传对象的存储键。
//
// upload 与 asset 分处两棵子树，是为了让「24h 回收临时对象」这件事
// 可以按前缀扫描，不必逐条回库判断某个 key 到底属于谁。
func BuildUploadKey(userID, uploadID, mime string, at time.Time) string {
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()
	return path.Join(
		"uploads",
		sanitizeSegment(userID),
		at.Format("2006"),
		at.Format("01"),
		sanitizeSegment(uploadID)+extForMIME(mime),
	)
}

// ContentURL 把一件资产的某个规格投影成对外 URL。
//
// URL 形态属于传输层，因此它不入库（domain.Asset 只存 StorageKey / ThumbKey /
// PosterKey）。但需要拼这个 URL 的不止 httpapi 一处——L2 推 SSE 事件、
// 给上游回源拉参考图时都要——所以格式集中在这里一份，避免几处各拼各的，
// 换域名或改路由时漏掉某一处。
func ContentURL(baseURL, assetID string, v Variant) string {
	if assetID == "" {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + "/api/assets/" + assetID + "/content?variant=" + string(v)
}

// DecorateURLs 把一批资产的存储键投影成可直接放进 <img src> 的内容 URL。
//
// 存储键（users/xx/asset/yy.png）是内部布局，一旦下发给前端就等于把它焊死：
// 换一个存储后端所有历史资产的地址全变。内容 URL 只暴露 asset id。
//
// 它住在这里而不是 httpapi，是因为投影的调用方不止一处：REST 响应要投，
// L2 推 task.succeeded 事件时也要投。两处各写一份的结果就是同一件资产
// 在 SSE 里没有 URL、在 REST 里有——前端拿到成功事件先渲染出一张裂图。
func DecorateURLs(baseURL string, items []domain.Asset) []domain.Asset {
	out := make([]domain.Asset, 0, len(items))
	for _, a := range items {
		a.Original = ContentURL(baseURL, a.ID, VariantOriginal)
		if a.ThumbKey != nil {
			u := ContentURL(baseURL, a.ID, VariantThumb512)
			a.Thumb512 = &u
		} else {
			a.Thumb512 = nil
		}
		if a.PosterKey != nil {
			u := ContentURL(baseURL, a.ID, VariantPoster)
			a.Poster = &u
		} else {
			a.Poster = nil
		}
		out = append(out, a)
	}
	return out
}

// sanitizeSegment 把一个 id 压成只含 [0-9a-zA-Z_-] 的片段。
//
// 不是「过滤掉危险字符」而是「只保留安全字符」：黑名单总有漏网的编码形式，
// 白名单没有。'.' 也在被替换之列，因此 ".." 这种片段根本构造不出来。
func sanitizeSegment(s string) string {
	if s == "" {
		return "unknown"
	}
	if len(s) > maxSegment {
		s = s[:maxSegment]
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if strings.Trim(out, "_") == "" {
		return "unknown"
	}
	return out
}

// mimeExt 是本平台会落盘的媒体类型与扩展名的对照表。
//
// 不用 mime.TypeByExtension 反查是因为它的结果依赖系统的 /etc/mime.types，
// 同一份 key 在开发机与容器里可能被解释成不同的类型；存储层的行为不该
// 随宿主机配置漂移。
var mimeExt = []struct {
	mime string
	ext  string
}{
	{"image/jpeg", ".jpg"},
	{"image/png", ".png"},
	{"image/webp", ".webp"},
	{"image/gif", ".gif"},
	{"image/avif", ".avif"},
	{"video/mp4", ".mp4"},
	{"video/webm", ".webm"},
	{"video/quicktime", ".mov"},
	{"audio/mpeg", ".mp3"},
	{"audio/wav", ".wav"},
	{"audio/mp4", ".m4a"},
	{"text/plain", ".txt"},
	{"application/json", ".json"},
}

// extForMIME 返回该媒体类型的扩展名，未知类型一律 .bin。
func extForMIME(mime string) string {
	m := normalizeMIME(mime)
	for _, e := range mimeExt {
		if e.mime == m {
			return e.ext
		}
	}
	return ".bin"
}

// mimeForExt 由扩展名反查媒体类型，未知时返回 application/octet-stream。
//
// 它服务于 Store.Stat：文件系统没有地方挂 Content-Type，而给对象存储
// 实现留的 ObjectInfo.MIME 字段又必须有值。资产的权威 MIME 记在
// assets 表里，这里给出的是尽力而为的近似值。
func mimeForExt(key string) string {
	ext := strings.ToLower(path.Ext(key))
	for _, e := range mimeExt {
		if e.ext == ext {
			return e.mime
		}
	}
	return "application/octet-stream"
}

// normalizeMIME 去掉参数部分并转小写，"image/JPEG; charset=x" → "image/jpeg"。
func normalizeMIME(mime string) string {
	m := strings.TrimSpace(mime)
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = m[:i]
	}
	return strings.ToLower(strings.TrimSpace(m))
}
