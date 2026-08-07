package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	aigcpool "github.com/aigc-pool/aigc-pool"
	"github.com/aigc-pool/aigc-pool/internal/config"
	"github.com/aigc-pool/aigc-pool/internal/migrate"
	mysqlstore "github.com/aigc-pool/aigc-pool/internal/store/mysql"
)

// runMigrate 实现 `aigcd migrate up|down [n]`。
func runMigrate(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("migrate requires a direction: up or down")
	}
	direction, rest := args[0], args[1:]

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := mysqlstore.Open(cfg.MySQLDSN)
	if err != nil {
		return err
	}
	defer db.Close()

	migrations, err := migrate.Load(aigcpool.MigrationsFS, aigcpool.MigrationsDir)
	if err != nil {
		return err
	}

	// 迁移可能很慢（建索引），但也不能永远挂着——超时给足 5 分钟。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	switch direction {
	case "up":
		applied, err := migrate.Up(ctx, db, migrations)
		if err != nil {
			return err
		}
		if len(applied) == 0 {
			slog.Info("migrations already up to date")
			return nil
		}
		slog.Info("migrations applied", "versions", applied)
		return nil
	case "down":
		n, err := mustAtoi(rest, 1)
		if err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
		rolled, err := migrate.Down(ctx, db, migrations, n)
		if err != nil {
			return err
		}
		slog.Info("migrations rolled back", "versions", rolled)
		return nil
	default:
		return fmt.Errorf("unknown migrate direction %q (want up or down)", direction)
	}
}

// currentVersion 供 serve 启动时提示 schema 版本，用不着就删。
func currentVersion(ctx context.Context, db *sql.DB) int {
	v, err := migrate.Current(ctx, db)
	if err != nil {
		return -1
	}
	return v
}
