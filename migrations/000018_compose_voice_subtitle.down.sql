-- =============================================================================
-- 000018_compose_voice_subtitle.down.sql
--
-- 把 capability 退回 000012 的取值：params 空数组、描述里不提两个开关。
--
-- 只回滚声明，不回滚代码。driver 读 params 是「有就用、没有就是原来那条路」，
-- 所以先执行这条 down、代码还没退版的窗口里，合成照样跑得通——只是 Validator
-- 会把带 mute / subtitle_srt 的提交拒掉（未知 params key 一律拒绝），前端那两个
-- 勾选框会开始报参数校验失败。要完整回退，代码退到 000018 之前的版本一起退。
--
-- 已经带字幕轨出好的成片不受影响：轨道在文件里，不在这张表里。
--
-- 幂等：JSON_SET 覆盖同一条路径，可重复执行。
-- =============================================================================

UPDATE models
   SET capability = JSON_SET(
         capability,
         '$.description', '把选中的片段按顺序拼接成一条 MP4。各段编码参数一致时零转码，不一致时自动重编码。',
         '$.params', JSON_ARRAY()
       )
 WHERE id = 'local-compose-1';

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'rollback: compose without mute and subtitle params');
