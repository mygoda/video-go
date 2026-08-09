-- =============================================================================
-- 000012_compose_real_concat.down.sql
--
-- 回滚到 000006 的取值。注意这只回滚配置，不回滚 driver——代码里是 ffmpeg
-- 拼接的话，30 秒超时会让重编码那条路必然失败。这条 down 只在整体回退到
-- 000011 之前的代码时才有意义。
-- =============================================================================

UPDATE providers
   SET timeout_ms = 30000
 WHERE id = '00000000-0000-4000-8000-000000000003';

UPDATE models
   SET description = '把画布上选中的卡片按顺序编成一份分镜序列；不做转码拼接',
       capability  = JSON_SET(
         capability,
         '$.description', '把选中的卡片按顺序编成一份分镜序列清单。它是降级形态：只编排顺序，不做转码拼接。',
         '$.eta.p50_seconds', 1,
         '$.eta.p90_seconds', 3
       )
 WHERE id = 'local-compose-1';

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'rollback: compose back to manifest-only timing');
