package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

// 连接池参数。上限刻意压在数据库 max_connections 之下留出余量：
// 多实例部署时每个实例都会开满这个池，把单实例的上限设成"能开多少开多少"
// 会让第三个实例连不上库。
const (
	maxOpenConns    = 25
	maxIdleConns    = 25
	connMaxLifetime = 5 * time.Minute
	connMaxIdleTime = 2 * time.Minute
	pingTimeout     = 5 * time.Second
)

// Open 打开并校验一个 MySQL 连接池。
//
// DSN 里缺失 parseTime / loc / charset 时**补上而不是报错**：少了 parseTime
// 的症状是 DATETIME 列扫描成 []byte 失败，报错点离病根隔着十几层调用栈，
// 排查成本远高于在这里直接补全。
//
// 无论调用方写了什么，multiStatements 一律关掉：那个开关是给迁移用的，
// 而 internal/migrate 自己切分语句，业务连接开着它等于给每一条 SQL
// 都接了一根注入放大器。
func Open(dsn string) (*sql.DB, error) {
	normalized, err := normalizeDSN(dsn)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("mysql", normalized)
	if err != nil {
		return nil, fmt.Errorf("mysql: open: %w", err)
	}

	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxLifetime(connMaxLifetime)
	db.SetConnMaxIdleTime(connMaxIdleTime)

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mysql: ping: %w", err)
	}
	return db, nil
}

// normalizeDSN 补齐本包依赖的那几个连接参数。
//
// time_zone 显式钉成 '+00:00' 而不是只靠 loc=UTC：loc 只影响驱动在 Go 侧的
// 时区换算，NOW(3) / CURRENT_TIMESTAMP(3) 这些**服务端**取的时间跟着会话时区走。
// 库里全是不带时区的 DATETIME(3)，两边不一致时写进去的毫秒会整体偏几个小时，
// 而且只在部署到非 UTC 的机器上才暴露。
//
// loc 走 Params 显式写一遍，而不是只设 cfg.Loc：FormatDSN 在 Loc 等于驱动
// 默认值（UTC）时会把这个参数整个省掉，于是归一化出来的 DSN 里看不见 loc。
// 连接行为是对的，但这串 DSN 会被打日志、被复制到别处复用，那时它就不再
// 自带"按 UTC 解释时间"这条约束了。写进 Params 让它显式留在串里。
func normalizeDSN(dsn string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("mysql: parse dsn: %w", err)
	}

	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.MultiStatements = false

	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	if cfg.Params["charset"] == "" {
		cfg.Params["charset"] = "utf8mb4"
	}
	cfg.Params["loc"] = "UTC"
	cfg.Params["time_zone"] = "'+00:00'"

	return cfg.FormatDSN(), nil
}

// withTx 在一个事务里跑 fn，fn 返回错误或 panic 时回滚。
//
// 本包所有"流水与余额必须同进退""校验 revision 与落库必须同进退"之类的
// 约束都靠它兑现，因此它是唯一的事务入口，不在各方法里手写 Begin/Rollback。
func withTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return wrap("begin transaction", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return wrap("commit transaction", err)
	}
	committed = true
	return nil
}

// bumpConfigVersion 推一个配置版本号。
//
// providers / models / skills 任一变更都要调它，且必须与那次变更同事务：
// 版本号是在跑的实例刷新模型缓存与 GET /api/models 换 ETag 的唯一触发器，
// 变更提交了而版本号没推，等于配置改了但谁也看不见。
func bumpConfigVersion(ctx context.Context, tx *sql.Tx, note string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO config_versions (changed_by, note) VALUES (NULL, ?)`, note)
	if err != nil {
		return wrap("bump config version", err)
	}
	return nil
}

// rowScanner 抹平 *sql.Row 与 *sql.Rows，让单行读与列表读共用一个扫描函数。
type rowScanner interface {
	Scan(dest ...any) error
}

// querier 抹平 *sql.DB 与 *sql.Tx，让同一段读逻辑既能独立跑也能在事务里跑。
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
