// Package aigcpool exposes the repository's SQL migrations as an embedded
// filesystem.
//
// 迁移文件必须随二进制一起走：部署机上不会有这个仓库，`aigcd migrate up`
// 却要能在那里把库建出来。放在仓库根是因为 go:embed 只能向下取文件，
// 而 migrations/ 目录本身在根上——为了它把目录挪进 internal/ 反而更别扭。
package aigcpool

import "embed"

// MigrationsFS 是 migrations/ 目录，配合 internal/migrate 使用。
//
//go:embed migrations/*.sql
var MigrationsFS embed.FS

// MigrationsDir 是 MigrationsFS 里迁移文件所在的目录名。
const MigrationsDir = "migrations"
