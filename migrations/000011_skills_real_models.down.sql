-- =============================================================================
-- 000011_skills_real_models.down.sql
--
-- 回滚让三个 Skill 重新指回 mock 假驱动。这在生产上没有意义，只在本地
-- 想复现「Skill 走 mock」那个旧行为时用得上——所以取值逐字抄回 000002。
-- =============================================================================

UPDATE skills
   SET default_model_id = 'mock-video-1',
       default_params   = '{ "resolution": "768p", "mode": "standard", "duration": 5 }'
 WHERE id = '00000000-0000-4000-8000-000000000101';

UPDATE skills
   SET default_model_id = 'mock-video-1',
       default_params   = '{ "resolution": "1080p", "mode": "pro", "duration": 5 }'
 WHERE id = '00000000-0000-4000-8000-000000000102';

UPDATE skills
   SET default_model_id = 'mock-image-1',
       default_params   = '{ "aspect": "1:1", "count": 2, "style_model": "none" }'
 WHERE id = '00000000-0000-4000-8000-000000000103';

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'rollback: skills default_model_id back to mock-*');
