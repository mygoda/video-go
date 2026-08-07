package asset

import "context"

// Usage 是某用户的存储用量。
type Usage struct {
	UsedBytes  int64
	QuotaBytes int64
	AssetCount int
}

// QuotaGuard 守住每用户的存储配额。
//
// 检查点在**转存之前**而不是之后：产物一旦下载落盘，字节就已经占掉了，
// 那时再发现超配额只能删掉重来，白白浪费一次上游生成的钱和一次带宽。
// 因此 Reserve 在 Transferor 动手前先按预估大小占位。
//
// Reserve 预占额度，超限返回 domain.CodeQuotaExceeded。
// Commit 用实际字节数落实预占（预估与实际总有偏差，多退少补）。
// Release 在转存失败时释放预占，避免失败任务把配额慢慢吃光。
// Recompute 从 assets 表重算真实用量，修正因崩溃等原因漂移的缓存值——
// 与积分同理，累加出来的用量是缓存，逐条求和才是真相。
type QuotaGuard interface {
	Reserve(ctx context.Context, userID string, bytes int64) error
	Commit(ctx context.Context, userID string, reserved, actual int64) error
	Release(ctx context.Context, userID string, bytes int64) error
	Usage(ctx context.Context, userID string) (Usage, error)
	Recompute(ctx context.Context, userID string) (Usage, error)
}
