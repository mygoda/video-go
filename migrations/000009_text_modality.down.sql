-- =============================================================================
-- 000009_text_modality.down.sql
--
-- 回滚顺序与 up 相反：先把 text 的行改回 image，再把枚举缩回去，
-- 反过来做会因为存量行的取值不在新枚举里而被拒。
--
-- 回滚会重新把六条对话模型标成图片模型——那正是本迁移要修的病。
-- 这里如实还原，是因为回滚的语义是"退回上一版的状态"，不是"退回一个更好的状态"。
-- =============================================================================

UPDATE models
   SET modality   = 'image',
       capability = JSON_SET(capability, '$.modality', 'image'),
       visibility = 'public'
 WHERE protocol_family = 'chat';

ALTER TABLE models
  MODIFY COLUMN modality ENUM('image','video') NOT NULL;

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'rollback: chat models back to modality=image');
