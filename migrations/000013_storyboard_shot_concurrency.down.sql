-- =============================================================================
-- 000013_storyboard_shot_concurrency.down.sql
--
-- 回到 000007 的取值。回滚后分镜的第 3 个镜头会被 rate_limited 顶回来，
-- 因此这条 down 只在整体回退到 000012 之前的代码时才有意义。
-- =============================================================================

UPDATE models
   SET capability = JSON_SET(capability, '$.limits.max_concurrent_per_user', 2)
 WHERE id = 'gpugeek-seedance-2-0-fast';

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'rollback: seedance-fast per-user concurrency back to 2');
