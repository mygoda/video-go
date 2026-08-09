package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/asset"
	"github.com/aigc-pool/aigc-pool/internal/config"
	mysqlstore "github.com/aigc-pool/aigc-pool/internal/store/mysql"
)

// backfillBatch 是一轮取多少条。取完一批就再查一次，而不是一次性把全表拉进内存：
// 补写会把这批行从查询条件里摘掉，因此下一轮自然拿到的是还没补的那些。
const backfillBatch = 200

// runBackfillMedia 实现 `aigcd backfill-media`：给历史资产补上宽高与时长。
//
// 为什么需要它：这两项过去只认上游报的值，而 GPUGeek 两条真实模型都不报，
// 于是库里的视频全是 width/height/duration_ms = NULL——资产库每条视频显示
// 「▶ 0s」，瀑布流也算不出高度。现在 Deriver 会在抽封面帧时顺手探测，
// 但那只对**此后**生成的产物生效，已经躺在库里的补不回来。
//
// 做成命令而不是迁移：探测要读二进制、要跑 ffprobe，那是 SQL 干不了的事。
// 它是幂等的——补上的行不再满足"缺失"的查询条件，重跑只会扫到剩下补不出来的那些。
func runBackfillMedia(_ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := mysqlstore.Open(cfg.MySQLDSN)
	if err != nil {
		return err
	}
	defer db.Close()

	blobs, err := asset.NewFSStore(cfg.StorageRoot)
	if err != nil {
		return err
	}
	assets := mysqlstore.NewAssetRepo(db)
	deriver, err := asset.NewDeriver(asset.DeriverDeps{Blobs: blobs, Assets: assets})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	var filled, skipped int
	// 补不出来的行会一直留在查询结果里，因此按"这一轮有没有进展"决定是否继续，
	// 否则同一批 ffprobe 解不开的坏文件会让循环永远转下去。
	for {
		batch, err := assets.ListMissingMedia(ctx, backfillBatch)
		if err != nil {
			return fmt.Errorf("backfill: 列出缺元信息的资产: %w", err)
		}
		if len(batch) == 0 {
			break
		}

		progress := 0
		for i := range batch {
			a := batch[i]
			ok, err := deriver.FillMeta(ctx, &a)
			switch {
			case errors.Is(err, asset.ErrUnsupported):
				skipped++
			case err != nil:
				slog.Warn("补写失败，跳过", "asset_id", a.ID, "type", a.Type, "err", err)
				skipped++
			case ok:
				filled++
				progress++
				slog.Info("补写完成", "asset_id", a.ID, "type", a.Type,
					"width", derefInt(a.Width), "height", derefInt(a.Height),
					"duration_ms", derefInt(a.DurationMS))
			default:
				// 探测跑通了但一项也没补出来（坏文件、不支持的编码）。
				skipped++
			}
		}
		if progress == 0 {
			break
		}
	}

	slog.Info("backfill-media 结束", "filled", filled, "skipped", skipped)
	return nil
}

// derefInt 把可空的整型摊平进日志，nil 记成 0。
func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
