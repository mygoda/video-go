-- =============================================================================
-- 000006_compose_local_model.down.sql
--
-- 回滚顺序与 up 相反：先删掉引用了 compose 族的模型行，再把枚举缩回去，
-- 反过来做会因为存量行的取值不在新枚举里而被拒。
--
-- 已经产出的「分镜序列」资产不动：它们是用户真实的产物，删列可以，
-- 删产物不行。
-- =============================================================================

DELETE FROM models WHERE id = 'local-compose-1';

DELETE FROM providers WHERE id = '00000000-0000-4000-8000-000000000003';

ALTER TABLE models
  MODIFY COLUMN protocol_family ENUM('chat','images','video','mock') NOT NULL;

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'rollback: local compose provider + local-compose-1 model');
