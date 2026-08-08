-- =============================================================================
-- 000004_seed_gpugeek_vision_models.down.sql
--
-- 完全反转 000004_seed_gpugeek_vision_models.up.sql。
--
-- 只删本迁移播下的三个模型，**不碰 providers**：gpugeek 供应商是 000003 播的，
-- 由 000003 的 down 负责。回滚一层迁移把下一层的东西带走，会让
-- 「回滚到 3」和「回滚到 2 再前滚到 3」得到不同的库。
--
-- 若这些模型已被任务引用，fk_tasks_model 是 RESTRICT，删除会失败并报错，
-- 这是刻意的：宁可回滚失败，也不静默丢掉任务与模型配置的关联。
-- =============================================================================

DELETE FROM models
 WHERE id IN ('gpugeek-qwen3-vl-plus', 'gpugeek-qwen3-vl-flash', 'gpugeek-qwen-vl-max');

-- 配置回滚同样要推版本号，否则在跑的实例会继续用缓存里已被删掉的模型
INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'rollback seed: removed gpugeek vision chat models');
