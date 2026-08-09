-- =============================================================================
-- 000007_predictions_generation_models.down.sql
--
-- 回滚顺序与 up 相反：先删掉引用了 predictions 的模型行，再把两个枚举缩回去。
-- 反过来做会因为存量行的取值不在新枚举里而被拒。
--
-- 已经产出的图片与视频资产不动：它们是用户真实的产物，删配置可以，
-- 删产物不行。
-- =============================================================================

DELETE FROM models WHERE id IN (
  'gpugeek-seedream-4-5',
  'gpugeek-seedance-2-0',
  'gpugeek-seedance-2-0-fast'
);

ALTER TABLE models
  MODIFY COLUMN video_protocol ENUM('ark','openai_video','google_lro','mock') NULL;

ALTER TABLE models
  MODIFY COLUMN protocol_family ENUM('chat','images','video','mock','compose') NOT NULL;

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'rollback: gpugeek predictions generation models');
