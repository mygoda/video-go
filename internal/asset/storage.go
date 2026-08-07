// Package asset is the L3 asset layer: blob storage, upstream artifact
// transfer, derived variants, lineage and per-user storage quota.
//
// 本层的存在理由只有一句话：**上游产物是借来的，我们必须马上抄一份。**
// 见 Transferor 的注释。
package asset

import (
	"context"
	"io"
	"time"
)

// ObjectInfo 是一个存储对象的元信息。
type ObjectInfo struct {
	Key       string
	Bytes     int64
	MIME      string
	UpdatedAt time.Time
}

// Store 是键值化的二进制存储。
//
// 接口刻意保持得极窄（Put/Get/Delete/Stat + 范围读），目的是让**本地文件系统
// 与 S3 兼容对象存储都能实现它**，且上层代码在两者之间切换时一行都不用改。
// 本地开发用 FS 实现，落到有对象存储的环境换个实现即可。
// 任何"只有 FS 才有"的能力（比如返回绝对路径）都不该出现在这个接口上，
// 一旦出现，S3 实现就得造假，抽象就破了。
//
// Key 的形态由调用方决定（建议 users/{user_id}/{yyyy}/{mm}/{asset_id}/{variant}），
// Store 只把它当不透明字符串。
//
// Put 接收 io.Reader 而不是 []byte：视频动辄上百 MB，全读进内存再写
// 会让并发转存把进程撑爆。整条转存链路必须是流式的。
//
// GetRange 支持 HTTP Range 请求，视频拖动进度条依赖它——没有范围读，
// 用户每拖一次都要从头下载整个文件。
type Store interface {
	Put(ctx context.Context, key string, r io.Reader, mime string) (ObjectInfo, error)
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
	GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, ObjectInfo, error)
	Delete(ctx context.Context, key string) error
	Stat(ctx context.Context, key string) (ObjectInfo, error)
}
