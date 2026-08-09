-- =============================================================================
-- 000011_skills_real_models.up.sql
--
-- 三个预置 Skill 的默认模型仍然指向 mock 假驱动，改成真实模型。
--
-- ── 为什么这条不是「顺手改个默认值」 ──────────────────────────────────────
-- Skill 是画布对话的入口：用户选「短剧」，拆完分镜之后每个镜头要用
-- default_model_id 去生成。指向 mock-video-1 意味着整条短剧链路的终点
-- 是一段占位视频——任务全绿、卡片全有图、合成也成功，产出的东西却是假的。
-- 这是绿灯下的坏产物，比直接报错难发现。000008 已经把 mock 模型移出用户
-- 目录（visibility='internal'），但 Skill 这条软引用绕过了目录，是最后一处
-- 还能走到 mock 的路。
--
-- ── default_params 必须跟着一起换 ────────────────────────────────────────
-- 老的默认参数是照 mock 模型的 capability 写的：resolution=768p、mode、
-- aspect、count、style_model 这些键在 seedance / seedream 的 params 里
-- 根本不存在，而校验器按 schema 逐键校验，只换 model_id 会让每一次生成
-- 都以 invalid_param 收场。两个字段是一对，不能只改一半。
--
-- ── 参数取值为什么这么保守 ────────────────────────────────────────────────
-- 短剧一次拆若干镜头，每个镜头一段真实视频调用，费用是逐段叠加的。
-- seedance-fast 的计价是 base 2 × duration，480p 不加价：4 秒一段 = 8 积分。
-- 取最短时长 + 最低清晰度，是为了让「试一次短剧」这件事的门槛停在几十积分，
-- 而不是几百。用户想要更长更清晰，在卡片上改参数重跑即可——默认值该服务于
-- 第一次尝试，不该服务于成片。
-- MV 只有它一个模型出全片、段数通常也少，给到 720p。
--
-- 幂等：INSERT ... ON DUPLICATE KEY UPDATE，行已存在时只覆盖这几列。
-- =============================================================================

INSERT INTO skills
  (id, name, description, system_prompt, default_model_id, default_params, enabled, display_order)
VALUES
  ('00000000-0000-4000-8000-000000000101',
   '短剧',
   '把一句剧情梗概拆成可拍摄的分镜脚本，逐镜生成画面',
   '你是短剧分镜导演。把用户给的剧情拆成若干镜头，每个镜头输出：景别、人物动作、环境、光线、镜头运动。人物外观在所有镜头间保持一致。每镜一段，不写解说词。',
   'gpugeek-seedance-2-0-fast',
   '{ "duration": 4, "resolution": "480p", "ratio": "16:9" }',
   1,
   10),
  ('00000000-0000-4000-8000-000000000102',
   'MV',
   '按歌词节奏产出视觉段落，风格统一、转场连贯',
   '你是音乐短片视觉导演。按用户给的歌词或情绪拆成若干视觉段落，每段描述主体、色调、质感、运镜与节奏点。全片保持同一视觉风格与色彩基调，不出现文字与字幕。',
   'gpugeek-seedance-2-0-fast',
   '{ "duration": 4, "resolution": "720p", "ratio": "16:9" }',
   1,
   20),
  ('00000000-0000-4000-8000-000000000103',
   '产品广告',
   '围绕单个产品产出商业级展示画面与卖点镜头',
   '你是商业广告创意总监。围绕用户给的产品输出画面描述：产品始终为画面主体且形态不变形，说明材质、背景、布光与拍摄角度，突出一个核心卖点。不生成任何品牌 logo 与文案文字。',
   'gpugeek-seedream-4-5',
   '{ "size": "2048x2048", "watermark": false }',
   1,
   30)
ON DUPLICATE KEY UPDATE
  system_prompt    = VALUES(system_prompt),
  default_model_id = VALUES(default_model_id),
  default_params   = VALUES(default_params);

-- 短剧的 system_prompt 原来写死「拆成 3-8 个镜头」。镜头数现在由服务端
-- 按成本收口（见 httpapi.storyboardMaxShots / storyboardDefaultShots），
-- 提示词里再写一个区间只会和它打架，因此上面那条已经把数字拿掉了。

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'skills: default_model_id from mock-* to real seedance/seedream');
