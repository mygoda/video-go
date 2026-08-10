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
// **本接口只管"还没有 assets 行的字节"。** 已落库资产的字节由 AssetRepo
// 在写 assets 行的同一个事务里记账，两边都记实际字节就会让一次转存的用量
// 翻倍（DEM-98）。预占是给尚未落地的那一件占位用的，行一落库就该撤掉。
//
// Reserve 预占额度，超限返回 domain.CodeQuotaExceeded。
// Commit 在资产行落库后撤掉预占（实际字节已由 AssetRepo 记过）。
// Release 在转存失败时释放预占，避免失败任务把配额慢慢吃光。
// Recompute 从 assets 表重算真实用量，修正因崩溃等原因漂移的缓存值——
// 与积分同理，累加出来的用量是缓存，逐条求和才是真相。
type QuotaGuard interface {
	Reserve(ctx context.Context, userID string, bytes int64) error
	Commit(ctx context.Context, userID string, reserved int64) error
	Release(ctx context.Context, userID string, bytes int64) error
	Usage(ctx context.Context, userID string) (Usage, error)
	Recompute(ctx context.Context, userID string) (Usage, error)
}
