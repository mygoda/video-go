-- =============================================================================
-- 000003_seed_gpugeek_provider.down.sql
--
-- 完全反转 000003_seed_gpugeek_provider.up.sql：按 FK 逆序删除
-- （models 引用 providers，故先删 models 再删 provider）。
-- 只按播种时写死的主键删，绝不按 provider_id 批删 —— 运维后来在同一供应商下
-- 加的模型不该被一次回滚带走。
-- 若这些模型已被任务引用，fk_tasks_model 是 RESTRICT，删除会失败并报错，
-- 这是刻意的：宁可回滚失败，也不静默丢掉任务与模型配置的关联。
--
-- 环境变量 AIGC_PROVIDER_GPUGEEK_KEY 不在本迁移的管辖范围内：密钥从来没有
-- 进过数据库，回滚自然也没有什么可清的。
-- =============================================================================

DELETE FROM models
 WHERE id IN ('gpugeek-qwen-flash', 'gpugeek-qwen3-max', 'gpugeek-gpt-5-2');

DELETE FROM providers
 WHERE id = '00000000-0000-4000-8000-000000000002';

-- 配置回滚同样要推版本号，否则在跑的实例会继续用缓存里已被删掉的模型
INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'rollback seed: removed gpugeek provider and its chat models');
