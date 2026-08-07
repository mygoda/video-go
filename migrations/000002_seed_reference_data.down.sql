-- =============================================================================
-- 000002_seed_reference_data.down.sql
--
-- 完全反转 000002_seed_reference_data.up.sql：按 FK 逆序删除本次播种的行
-- （models 引用 providers，故先删 models 再删 provider）。
-- 只按播种时写死的主键删，绝不 TRUNCATE —— 表里可能已经有运维后来加的真实配置。
-- 若这些 mock 模型已被任务引用，fk_tasks_model 是 RESTRICT，删除会失败并报错，
-- 这是刻意的：宁可回滚失败，也不静默丢掉任务与模型配置的关联。
-- =============================================================================

DELETE FROM skills
 WHERE id IN (
   '00000000-0000-4000-8000-000000000101',
   '00000000-0000-4000-8000-000000000102',
   '00000000-0000-4000-8000-000000000103'
 );

DELETE FROM models
 WHERE id IN ('mock-image-1', 'mock-video-1');

DELETE FROM providers
 WHERE id = '00000000-0000-4000-8000-000000000001';

-- 配置回滚同样要推版本号，否则在跑的实例会继续用缓存里已被删掉的模型
INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'rollback seed: removed mock provider, mock models and preset skills');
