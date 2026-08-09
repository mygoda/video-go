-- =============================================================================
-- 000012_compose_real_concat.up.sql
--
-- 一键合成从「编一份顺序清单」变成「用 ffmpeg 真拼接」，配置要跟着改。
--
-- ── 改的是三件事，都是同一个原因：拼接不再是零成本的 ────────────────────
-- 000006 那版本地合成只是把一串地址编成 JSON，毫秒级、不碰字节，所以
-- provider.timeout_ms 给了 30 秒、eta 给了 1 秒、描述里写着「不做转码拼接」。
-- 现在这条链路要把每段的字节落到磁盘、跑 ffmpeg、再把成片读回来：
--
--   · timeout_ms 30s → 10min。executor 用 provider.timeout_ms 作为一次
--     driver 调用的上下文超时，它比 driver 自己的 concatTimeout 更靠外，
--     不放宽的话零转码那条路（几百毫秒）没事，回退到重编码就必然被掐断。
--   · eta 1s/3s → 20s/120s。零转码是秒级，重编码十二段能到分钟级。
--     ETA 撑着前端的进度条与「还要多久」，报 1 秒会让每次合成都看起来卡死。
--   · 描述里的「不做转码拼接」现在是错的，用户在模型下拉里看得见它。
--
-- max_bytes 与 max_count 不动：单段 500MB、最多 12 段仍然是合理的上界，
-- 成片体积的兜底在 driver 里（compose.maxOutputBytes）。
--
-- 幂等：两条 UPDATE / INSERT ... ON DUPLICATE KEY UPDATE 都可重复执行。
-- =============================================================================

UPDATE providers
   SET timeout_ms = 600000
 WHERE id = '00000000-0000-4000-8000-000000000003';

UPDATE models
   SET description = '把画布上选中的卡片按顺序拼接成一条 MP4',
       capability  = JSON_SET(
         capability,
         '$.description', '把选中的片段按顺序拼接成一条 MP4。各段编码参数一致时零转码，不一致时自动重编码。',
         '$.eta.p50_seconds', 20,
         '$.eta.p90_seconds', 120
       )
 WHERE id = 'local-compose-1';

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'compose: real ffmpeg concat — longer timeout and honest eta');
