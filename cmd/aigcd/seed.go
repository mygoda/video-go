package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/config"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/httpapi"
	mysqlstore "github.com/aigc-pool/aigc-pool/internal/store/mysql"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// 种子账号的默认口令。**只用于本地开发**，是明摆着的占位符：
// 任何非本机环境都必须用 AIGC_SEED_ADMIN_PASSWORD / AIGC_SEED_USER_PASSWORD
// 覆盖。真实口令不入库、不入仓库、不进日志，只从环境读。
const (
	defaultSeedAdminPassword = "admin-dev-only"
	defaultSeedUserPassword  = "user-dev-only"
	seedAdminUsername        = "admin"
	seedUserUsername         = "demo"
	// seedInitialCredits 给种子用户一笔起始额度，否则第一次提交任务就撞积分不足，
	// 而"跑通全链路"恰恰要先跑得起来。
	seedInitialCredits = 100000
)

// runSeed 实现 `aigcd seed`：建一个管理员、一个普通用户，各充一笔起始积分。
//
// 它刻意不做成 SQL 迁移：口令哈希是 PBKDF2，必须由 Go 算，
// 把哈希写死进迁移文件等于把一份固定口令提交进仓库。
func runSeed(_ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := mysqlstore.Open(cfg.MySQLDSN)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	hasher := httpapi.NewPasswordHasher()

	accounts := []struct {
		username string
		password string
		role     domain.Role
	}{
		{seedAdminUsername, envOr("AIGC_SEED_ADMIN_PASSWORD", defaultSeedAdminPassword), domain.RoleAdmin},
		{seedUserUsername, envOr("AIGC_SEED_USER_PASSWORD", defaultSeedUserPassword), domain.RoleUser},
	}

	for _, a := range accounts {
		hash, err := hasher.Hash(a.password)
		if err != nil {
			return fmt.Errorf("seed: hash password for %s: %w", a.username, err)
		}
		created, err := seedUser(ctx, db, a.username, hash, string(a.role))
		if err != nil {
			return err
		}
		if created {
			slog.Info("seed user created", "username", a.username, "role", a.role, "credits", seedInitialCredits)
		} else {
			slog.Info("seed user already exists, left untouched", "username", a.username)
		}
	}
	return nil
}

// seedUser 幂等地建一个账号并记一笔 topup 流水。已存在时原样返回，
// **不覆盖口令**——重跑 seed 不该把用户自己改过的密码打回去。
func seedUser(ctx context.Context, db *sql.DB, username, passwordHash, role string) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, username).Scan(&existing)
	if err == nil {
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, fmt.Errorf("seed: look up %s: %w", username, err)
	}

	userID := uid.New()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, role, status, credits) VALUES (?, ?, ?, ?, 'active', ?)`,
		userID, username, passwordHash, role, seedInitialCredits); err != nil {
		return false, fmt.Errorf("seed: insert %s: %w", username, err)
	}
	// 起始积分同样走流水：users.credits 只是物化缓存，绕开流水直接写它
	// 会让 LedgerRepo.Recompute 之后把这笔积分抹掉。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO credit_ledger (id, user_id, type, amount, balance_after, reason)
		 VALUES (?, ?, 'topup', ?, ?, 'seed initial credits')`,
		uid.New(), userID, seedInitialCredits, seedInitialCredits); err != nil {
		return false, fmt.Errorf("seed: ledger for %s: %w", username, err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
