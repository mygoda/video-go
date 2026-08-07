package asset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// fsStore 是 Store 的本地文件系统实现。
//
// 用 os.Root 而不是自己拼绝对路径再检查前缀：Root 把「不许逃出这棵树」
// 交给内核层面的相对打开来保证，符号链接、竞态改名这些自己写检查
// 一定会漏的情况它都覆盖。key 侧的 sanitize 是第一道防线，Root 是第二道，
// 两道都在的理由是任何一道单独失效时另一道仍然成立。
type fsStore struct {
	root *os.Root
	dir  string
}

// NewFSStore 打开（必要时创建）root 目录并返回一个文件系统存储。
//
// 目录不存在时创建而不是报错：本地开发第一次跑起来不该先手动 mkdir 一层
// ./data/storage，那是纯粹的摩擦。
func NewFSStore(root string) (Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("asset: 存储根目录不能为空")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("asset: 创建存储根目录 %q: %w", root, err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("asset: 打开存储根目录 %q: %w", root, err)
	}
	return &fsStore{root: r, dir: root}, nil
}

// Close 释放根目录句柄。进程退出时调用，测试里也用它避免 fd 泄漏。
func (s *fsStore) Close() error { return s.root.Close() }

// Put 流式写入一个对象。
//
// 先写同目录下的临时文件再 rename，而不是直接往目标名上写：直接写时进程
// 在中途挂掉会留下一个长度不对的文件，而它在 Stat 看来是完全正常的——
// 一个"存在但内容截断"的产物比一个不存在的产物危险得多，后者至少能被
// 转存失败的路径捕获。rename 在同一文件系统内是原子的，因此对象要么
// 完整存在要么不存在。
func (s *fsStore) Put(ctx context.Context, key string, r io.Reader, mime string) (ObjectInfo, error) {
	name, err := cleanKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if r == nil {
		return ObjectInfo{}, fmt.Errorf("asset: Put %q 的数据源为空", key)
	}
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}

	if dir := path.Dir(name); dir != "." {
		if err := s.root.MkdirAll(dir, 0o755); err != nil {
			return ObjectInfo{}, fmt.Errorf("asset: 创建目录 %q: %w", dir, err)
		}
	}

	tmp := name + ".tmp-" + randomSuffix()
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("asset: 创建临时对象 %q: %w", tmp, err)
	}

	n, copyErr := io.Copy(f, &ctxReader{ctx: ctx, r: r})
	closeErr := f.Close()
	if copyErr != nil {
		_ = s.root.Remove(tmp)
		return ObjectInfo{}, fmt.Errorf("asset: 写入对象 %q: %w", key, copyErr)
	}
	if closeErr != nil {
		_ = s.root.Remove(tmp)
		return ObjectInfo{}, fmt.Errorf("asset: 关闭对象 %q: %w", key, closeErr)
	}

	if err := s.root.Rename(tmp, name); err != nil {
		_ = s.root.Remove(tmp)
		return ObjectInfo{}, fmt.Errorf("asset: 提交对象 %q: %w", key, err)
	}

	info, err := s.root.Stat(name)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("asset: 读取对象元信息 %q: %w", key, err)
	}
	m := normalizeMIME(mime)
	if m == "" {
		m = mimeForExt(name)
	}
	return ObjectInfo{Key: key, Bytes: n, MIME: m, UpdatedAt: info.ModTime().UTC()}, nil
}

// Get 打开一个对象读取。返回的 ReadCloser 由调用方关闭。
func (s *fsStore) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	name, err := cleanKey(key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, ObjectInfo{}, err
	}
	f, err := s.root.Open(name)
	if err != nil {
		return nil, ObjectInfo{}, wrapStatErr(key, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, ObjectInfo{}, fmt.Errorf("asset: 读取对象元信息 %q: %w", key, err)
	}
	return f, objectInfoOf(key, fi), nil
}

// GetRange 从 offset 起读 length 字节。length <= 0 表示读到结尾。
//
// 它是视频拖进度条的底座：没有范围读，用户每拖一次都要从头下整个文件。
func (s *fsStore) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, ObjectInfo, error) {
	name, err := cleanKey(key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	if offset < 0 {
		return nil, ObjectInfo{}, fmt.Errorf("asset: GetRange %q 的 offset 不能为负: %d", key, offset)
	}
	if err := ctx.Err(); err != nil {
		return nil, ObjectInfo{}, err
	}
	f, err := s.root.Open(name)
	if err != nil {
		return nil, ObjectInfo{}, wrapStatErr(key, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, ObjectInfo{}, fmt.Errorf("asset: 读取对象元信息 %q: %w", key, err)
	}
	if offset > fi.Size() {
		_ = f.Close()
		return nil, ObjectInfo{}, &domain.Error{
			Code:    domain.CodeInvalidParam,
			Message: "请求的范围超出对象长度",
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, ObjectInfo{}, fmt.Errorf("asset: 定位对象 %q 到 %d: %w", key, offset, err)
	}
	info := objectInfoOf(key, fi)
	if length <= 0 {
		return f, info, nil
	}
	if remain := fi.Size() - offset; length > remain {
		length = remain
	}
	return &sectionReadCloser{Reader: io.LimitReader(f, length), c: f}, info, nil
}

// Delete 删除一个对象。对象不存在时视为成功——清理是可重入的动作，
// 「已经不在了」与「刚被我删掉」对调用方是同一个结果。
func (s *fsStore) Delete(ctx context.Context, key string) error {
	name, err := cleanKey(key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("asset: 删除对象 %q: %w", key, err)
	}
	return nil
}

// Stat 返回对象元信息，不存在时返回 domain.CodeNotFound。
func (s *fsStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	name, err := cleanKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	fi, err := s.root.Stat(name)
	if err != nil {
		return ObjectInfo{}, wrapStatErr(key, err)
	}
	return objectInfoOf(key, fi), nil
}

// objectInfoOf 由文件元信息拼出 ObjectInfo。MIME 按扩展名近似，
// 权威值在 assets 表里。
func objectInfoOf(key string, fi fs.FileInfo) ObjectInfo {
	return ObjectInfo{
		Key:       key,
		Bytes:     fi.Size(),
		MIME:      mimeForExt(key),
		UpdatedAt: fi.ModTime().UTC(),
	}
}

// wrapStatErr 把 fs.ErrNotExist 翻译成平台的 not_found，其余原样包装。
//
// 翻译在这里做而不是在 handler 里判 os.IsNotExist：一旦换成 S3 实现，
// handler 那边的 os 判断就会静默失效，而接口约定的错误码不该随实现变。
func wrapStatErr(key string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return &domain.Error{
			Code:    domain.CodeNotFound,
			Message: "对象不存在",
			Err:     err,
		}
	}
	return fmt.Errorf("asset: 访问对象 %q: %w", key, err)
}

// cleanKey 校验并规整一个存储键。
//
// 它拒绝绝对路径、拒绝任何 ".." 片段、拒绝空键。这与 os.Root 的保护重复，
// 但重复是有意的：Root 保证「写不出这棵树」，cleanKey 保证「同一个 key
// 永远映射到同一个文件」——path.Clean 之后 a//b 与 a/b 是同一个对象，
// 否则同一件资产可能因为拼 key 时多了个斜杠而落两份。
func cleanKey(key string) (string, error) {
	k := strings.TrimSpace(key)
	if k == "" {
		return "", fmt.Errorf("asset: 存储键不能为空")
	}
	if strings.ContainsRune(k, 0) {
		return "", fmt.Errorf("asset: 存储键含非法字符")
	}
	if path.IsAbs(k) || strings.HasPrefix(k, `\`) {
		return "", fmt.Errorf("asset: 存储键必须是相对路径: %q", key)
	}
	cleaned := path.Clean(k)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("asset: 存储键越界: %q", key)
	}
	return cleaned, nil
}

// sectionReadCloser 把一个受限读取器与底层文件的关闭绑在一起，
// 免得调用方拿到 LimitReader 后无从关闭文件。
type sectionReadCloser struct {
	io.Reader
	c io.Closer
}

func (s *sectionReadCloser) Close() error { return s.c.Close() }

// ctxReader 让 io.Copy 在 ctx 取消时尽快停下。
//
// 没有它，一个被取消的转存会一直把几百 MB 拷完才发现没人要了——
// 视频转存动辄几十秒，那期间的带宽和磁盘都是白花的。
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
