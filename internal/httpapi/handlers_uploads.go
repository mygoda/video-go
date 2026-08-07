package httpapi

import (
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/asset"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

const (
	// maxUploadBytes 是单文件上限。真正的按槽上限（InputSlotSpec.MaxBytes）
	// 在提交任务时校验；这里只是一道防止把磁盘写满的粗筛。
	maxUploadBytes = 128 << 20 // 128 MiB
	// uploadTTL 与 PRD 的「24h 未被引用即回收」一致。
	uploadTTL = 24 * time.Hour
	// sniffLen 是 http.DetectContentType 需要的前缀长度。
	sniffLen = 512
)

// allowedUploadMIME 是上传白名单。
//
// 白名单而不是黑名单：黑名单永远差一个刚出现的类型，而这个平台需要的
// 输入类型是有限且明确的几种。
var allowedUploadMIME = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
	"video/mp4":  true,
}

func (s *server) handleCreateUpload(w http.ResponseWriter, r *http.Request) {
	id, err := identity(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, r, errInvalid("上传解析失败: %v", err))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, r, errFields(
			[]domain.FieldError{{Key: "file", Message: "缺少文件"}}, "参数校验未通过"))
		return
	}
	defer file.Close()

	// 先嗅探再落盘：MIME 以字节内容为准，不信 Content-Type 头也不信扩展名，
	// 两者都是客户端说了算的。
	head := make([]byte, sniffLen)
	n, _ := io.ReadFull(file, head)
	head = head[:n]
	mime := sniffMIME(head, header.Filename)
	if !allowedUploadMIME[mime] {
		writeError(w, r, errFields(
			[]domain.FieldError{{Key: "file", Message: "不支持的文件类型: " + mime}}, "参数校验未通过"))
		return
	}

	width, height := imageDimensions(file)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(w, r, errInternal(err))
		return
	}

	now := s.now()
	uploadID := uid.New()
	key := asset.BuildUploadKey(id.UserID, uploadID, mime, now)

	info, err := s.deps.Blobs.Put(r.Context(), key, file, mime)
	if err != nil {
		writeError(w, r, errInternal(err))
		return
	}

	u, err := s.deps.Uploads.Create(r.Context(), domain.Upload{
		ID:         uploadID,
		UserID:     id.UserID,
		StorageKey: key,
		MIME:       mime,
		Bytes:      info.Bytes,
		Width:      width,
		Height:     height,
		Slot:       r.FormValue("slot"),
		ExpiresAt:  now.Add(uploadTTL),
		CreatedAt:  now,
	})
	if err != nil {
		// 库里没落成，磁盘上那份就是垃圾，顺手清掉。
		_ = s.deps.Blobs.Delete(detachedContext(), key)
		writeError(w, r, err)
		return
	}

	u.PreviewURL = s.uploadContentURL(u.ID)
	writeJSON(w, http.StatusCreated, u)
}

// handleUploadContent 回源一个临时上传对象的字节。
//
// 它是 openapi.yaml 之外的增补端点：Upload.preview_url 必须指向某处，
// 而 upload 还不是 asset，走不了 /api/assets/{id}/content。
func (s *server) handleUploadContent(w http.ResponseWriter, r *http.Request) {
	uploadID, err := pathID(r, "uploadId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	u, err := s.deps.Uploads.Get(r.Context(), uploadID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	s.streamBlob(w, r, u.StorageKey, u.MIME)
}

func (s *server) uploadContentURL(uploadID string) string {
	return strings.TrimRight(s.deps.Config.PublicBaseURL, "/") + "/api/uploads/" + uploadID + "/content"
}

// sniffMIME 按字节内容判定类型，扩展名只在嗅探给不出结论时兜底。
func sniffMIME(head []byte, filename string) string {
	mime := http.DetectContentType(head)
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if mime != "application/octet-stream" {
		return mime
	}
	switch {
	case strings.HasSuffix(strings.ToLower(filename), ".webp"):
		return "image/webp"
	case strings.HasSuffix(strings.ToLower(filename), ".mp4"):
		return "video/mp4"
	}
	return mime
}

// imageDimensions 读出图片宽高。读不出（视频、webp）时返回两个 nil——
// 宽高是给前端摆位用的辅助信息，缺了不影响上传本身。
func imageDimensions(rest io.ReadSeeker) (*int, *int) {
	if _, err := rest.Seek(0, io.SeekStart); err != nil {
		return nil, nil
	}
	cfg, _, err := image.DecodeConfig(rest)
	if err != nil {
		return nil, nil
	}
	w, h := cfg.Width, cfg.Height
	return &w, &h
}
