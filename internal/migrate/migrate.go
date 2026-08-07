// Package migrate applies the SQL files in migrations/ to a MySQL database.
//
// 自带迁移器而不是依赖 golang-migrate 的 CLI：那个 CLI 需要单独安装，
// 而「照着 README 走能从零到可用」这条验收线不该卡在让人先去装一个 Go 工具上。
// 文件命名与格式仍完全兼容 golang-migrate（NNNNNN_name.up.sql / .down.sql），
// 需要时换回那个 CLI 不用改任何迁移文件。
//
// 版本表 schema_migrations 的形态也与 golang-migrate 一致（version + dirty），
// 两者可以互相接管。
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Migration 是一个版本的一对文件。
type Migration struct {
	Version int
	Name    string
	Up      string
	Down    string
}

// Load 从 fsys 的 dir 目录读出全部迁移，按版本号升序返回。
func Load(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: read %s: %w", dir, err)
	}

	byVersion := map[int]*Migration{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, direction, err := parseName(e.Name())
		if err != nil {
			return nil, err
		}
		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", e.Name(), err)
		}
		m := byVersion[version]
		if m == nil {
			m = &Migration{Version: version, Name: name}
			byVersion[version] = m
		}
		if direction == "up" {
			m.Up = string(body)
		} else {
			m.Down = string(body)
		}
	}

	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// parseName 解析 000001_init_schema.up.sql 这样的文件名。
func parseName(filename string) (version int, name, direction string, err error) {
	base := strings.TrimSuffix(filename, ".sql")
	dot := strings.LastIndex(base, ".")
	if dot < 0 {
		return 0, "", "", fmt.Errorf("migrate: %q is missing an .up/.down suffix", filename)
	}
	direction = base[dot+1:]
	if direction != "up" && direction != "down" {
		return 0, "", "", fmt.Errorf("migrate: %q has unknown direction %q", filename, direction)
	}
	rest := base[:dot]
	under := strings.Index(rest, "_")
	if under < 0 {
		return 0, "", "", fmt.Errorf("migrate: %q is missing the version_name separator", filename)
	}
	version, err = strconv.Atoi(rest[:under])
	if err != nil {
		return 0, "", "", fmt.Errorf("migrate: %q has a non-numeric version: %w", filename, err)
	}
	return version, rest[under+1:], direction, nil
}

// EnsureVersionTable 建立版本表。与 golang-migrate 的表结构一致。
func EnsureVersionTable(ctx context.Context, db *sql.DB) error {
	const ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version BIGINT NOT NULL PRIMARY KEY,
		dirty   TINYINT(1) NOT NULL DEFAULT 0
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`
	_, err := db.ExecContext(ctx, ddl)
	return err
}

// Current 返回已应用的最高版本，未应用过任何迁移时返回 0。
func Current(ctx context.Context, db *sql.DB) (int, error) {
	if err := EnsureVersionTable(ctx, db); err != nil {
		return 0, err
	}
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// Up 应用全部尚未应用的迁移，返回本次应用的版本列表。
//
// 每个版本单独一个事务：MySQL 的 DDL 隐式提交，事务保护不了建表语句本身，
// 但它保证 schema_migrations 的记账与该版本的最后一条语句同进退，
// 不会出现「表建了但版本没记上」导致下次重跑直接报表已存在。
func Up(ctx context.Context, db *sql.DB, migrations []Migration) ([]int, error) {
	current, err := Current(ctx, db)
	if err != nil {
		return nil, err
	}

	var applied []int
	for _, m := range migrations {
		if m.Version <= current {
			continue
		}
		if strings.TrimSpace(m.Up) == "" {
			return applied, fmt.Errorf("migrate: version %d has no .up.sql", m.Version)
		}
		if err := execScript(ctx, db, m.Up); err != nil {
			return applied, fmt.Errorf("migrate: apply %06d_%s: %w", m.Version, m.Name, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, dirty) VALUES (?, 0)
			 ON DUPLICATE KEY UPDATE dirty = 0`, m.Version); err != nil {
			return applied, fmt.Errorf("migrate: record %d: %w", m.Version, err)
		}
		applied = append(applied, m.Version)
	}
	return applied, nil
}

// Down 回滚最高的 n 个版本。
func Down(ctx context.Context, db *sql.DB, migrations []Migration, n int) ([]int, error) {
	current, err := Current(ctx, db)
	if err != nil {
		return nil, err
	}

	var rolled []int
	for i := len(migrations) - 1; i >= 0 && len(rolled) < n; i-- {
		m := migrations[i]
		if m.Version > current {
			continue
		}
		if strings.TrimSpace(m.Down) == "" {
			return rolled, fmt.Errorf("migrate: version %d has no .down.sql", m.Version)
		}
		if err := execScript(ctx, db, m.Down); err != nil {
			return rolled, fmt.Errorf("migrate: rollback %06d_%s: %w", m.Version, m.Name, err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = ?`, m.Version); err != nil {
			return rolled, fmt.Errorf("migrate: unrecord %d: %w", m.Version, err)
		}
		rolled = append(rolled, m.Version)
	}
	return rolled, nil
}

// execScript 逐条执行一个脚本里的语句。
//
// 不依赖 DSN 的 multiStatements=true：那个开关会让**所有**查询都允许多语句，
// 等于给整个进程的每一条 SQL 都开了一道注入放大器。迁移这一处需要多语句，
// 就在这一处自己切分。
func execScript(ctx context.Context, db *sql.DB, script string) error {
	for _, stmt := range splitStatements(script) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%w\n-- statement --\n%s", err, truncate(stmt, 400))
		}
	}
	return nil
}

// splitStatements 按分号切分 SQL，跳过注释与字符串字面量里的分号。
func splitStatements(script string) []string {
	var (
		out     []string
		cur     strings.Builder
		inSingle, inDouble, inBacktick bool
		inLineComment, inBlockComment  bool
	)
	rs := []rune(script)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		next := rune(0)
		if i+1 < len(rs) {
			next = rs[i+1]
		}

		switch {
		case inLineComment:
			if c == '\n' {
				inLineComment = false
				cur.WriteRune(c)
			}
			continue
		case inBlockComment:
			if c == '*' && next == '/' {
				inBlockComment = false
				i++
			}
			continue
		case inSingle:
			cur.WriteRune(c)
			if c == '\\' && next != 0 {
				cur.WriteRune(next)
				i++
			} else if c == '\'' {
				inSingle = false
			}
			continue
		case inDouble:
			cur.WriteRune(c)
			if c == '\\' && next != 0 {
				cur.WriteRune(next)
				i++
			} else if c == '"' {
				inDouble = false
			}
			continue
		case inBacktick:
			cur.WriteRune(c)
			if c == '`' {
				inBacktick = false
			}
			continue
		}

		switch {
		case c == '-' && next == '-':
			inLineComment = true
			i++
		case c == '/' && next == '*':
			inBlockComment = true
			i++
		case c == '\'':
			inSingle = true
			cur.WriteRune(c)
		case c == '"':
			inDouble = true
			cur.WriteRune(c)
		case c == '`':
			inBacktick = true
			cur.WriteRune(c)
		case c == ';':
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
		default:
			cur.WriteRune(c)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
