-- =============================================================================
-- 000002_seed_reference_data.up.sql
--
-- 只播种「安全且不含任何密钥」的参考数据：
--   1. 一个 mock 供应商 + 两个 mock 模型（image / video 各一）。
--      本环境没有任何真实上游 API key，mock 通道是整条链路（提交 → 状态机 →
--      轮询 → 转存 → 记账 → SSE）唯一能被端到端跑通的路径，因此它必须随 schema 一起存在。
--      capability JSON 逐字取自 docs/contracts/model-schema.examples.json，
--      只把 id / name / vendor 换成 mock 自己的取值（capability.id 必须与 models.id 一致，
--      因为它会原样下发前端、再原样回传到 POST /api/tasks）。
--   2. 三个 Skill：短剧 / MV / 产品广告。
--
-- 关于管理员账号：**本迁移不播种任何用户，也不写死任何口令哈希。**
-- 首个管理员由实现阶段的 CLI 子命令创建（形如 `aigc-pool admin create --username <u>`，
-- 口令从 stdin 读、bcrypt 后入库）。迁移文件会进版本库、会被复制、会进日志，
-- 把默认口令写在这里等于把后门写进仓库。
--
-- 幂等：全部使用 INSERT ... ON DUPLICATE KEY UPDATE，可从零重复执行。
-- =============================================================================

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L0 适配器配置 —— mock 供应商
-- credential_ref 是**环境变量名**，不是密钥。mock driver 根本不读它，
-- 保留该字段只为让 mock 与真实供应商走完全相同的配置形态。
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO providers
  (id, name, base_url, credential_ref, auth_style, auth_header_name, enabled, timeout_ms, max_concurrency)
VALUES
  ('00000000-0000-4000-8000-000000000001',
   'mock',
   'http://localhost:8080/__mock',
   'AIGC_PROVIDER_MOCK_KEY',
   'bearer',
   'Authorization',
   1,
   30000,
   16)
ON DUPLICATE KEY UPDATE
  base_url       = VALUES(base_url),
  credential_ref = VALUES(credential_ref),
  enabled        = VALUES(enabled);

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L1 能力声明 —— mock 图像模型
-- 覆盖 select / compound / aspect_select / stepper / slider / textarea / seed
-- 七种控件与 has_input 隐式分流，前端零改动即可渲染出完整表单
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO models
  (id, provider_id, upstream_model, protocol_family, video_protocol, modality,
   enabled, display_order, name, vendor, description, preview_url,
   capability, request_mapping, poll_interval_seconds)
VALUES
  ('mock-image-1',
   '00000000-0000-4000-8000-000000000001',
   'mock-image-1',
   'mock',
   NULL,
   'image',
   1,
   10,
   'Mock 图像模型',
   'mock',
   '本地开发用假模型，走与真实模型完全相同的状态机与记账路径',
   NULL,
   '{
     "id": "mock-image-1",
     "name": "Mock 图像模型",
     "vendor": "mock",
     "modality": "image",
     "enabled": true,
     "order": 10,
     "description": "通用高质量出图，支持参考图编辑（本地 mock）",
     "inputs": [
       {
         "key": "reference_images",
         "label": "参考图",
         "kind": "image",
         "required": false,
         "min_count": 0,
         "max_count": 3,
         "accept": ["image/png", "image/jpeg", "image/webp"],
         "max_bytes": 10485760,
         "max_pixels": { "width": 4096, "height": 4096 },
         "hint": "传图后将按提示词对图片进行编辑"
       }
     ],
     "params": [
       {
         "key": "style_model",
         "label": "风格模型",
         "control": "select",
         "group": "primary",
         "order": 10,
         "default": "none",
         "render_hint": "dropdown",
         "options": [
           { "value": "none", "label": "不使用" },
           { "value": "anime_v3", "label": "动漫 V3" },
           { "value": "film_a24", "label": "A24 电影感", "badge": "+2 积分" }
         ]
       },
       {
         "key": "size_group",
         "label": "尺寸与数量",
         "control": "compound",
         "group": "primary",
         "order": 20,
         "default": null,
         "display_template": "{aspect} · {count}张",
         "fields": [
           {
             "key": "aspect",
             "label": "比例",
             "control": "aspect_select",
             "group": "primary",
             "order": 10,
             "default": "1:1",
             "options": [
               { "value": "1:1", "label": "1:1", "ratio_w": 1, "ratio_h": 1, "pixels": { "width": 1024, "height": 1024 } },
               { "value": "3:4", "label": "3:4", "ratio_w": 3, "ratio_h": 4, "pixels": { "width": 896, "height": 1152 } },
               { "value": "4:3", "label": "4:3", "ratio_w": 4, "ratio_h": 3, "pixels": { "width": 1152, "height": 896 } },
               { "value": "9:16", "label": "9:16", "ratio_w": 9, "ratio_h": 16, "pixels": { "width": 768, "height": 1344 } },
               { "value": "16:9", "label": "16:9", "ratio_w": 16, "ratio_h": 9, "pixels": { "width": 1344, "height": 768 } }
             ]
           },
           {
             "key": "count",
             "label": "数量",
             "control": "stepper",
             "group": "primary",
             "order": 20,
             "default": 1,
             "min": 1,
             "max": 4,
             "step": 1,
             "unit": "张"
           }
         ]
       },
       {
         "key": "strength",
         "label": "重绘强度",
         "control": "slider",
         "group": "advanced",
         "order": 10,
         "default": 0.6,
         "min": 0.1,
         "max": 1,
         "step": 0.05,
         "help": "越低越贴近原图",
         "visible_when": { "op": "has_input", "slot": "reference_images" }
       },
       {
         "key": "negative_prompt",
         "label": "负向提示词",
         "control": "textarea",
         "group": "advanced",
         "order": 20,
         "default": "",
         "max_length": 500,
         "placeholder": "不希望出现的元素"
       },
       {
         "key": "seed",
         "label": "随机种子",
         "control": "seed",
         "group": "advanced",
         "order": 30,
         "default": null,
         "min": 0,
         "max": 2147483647,
         "allow_random": true
       }
     ],
     "pricing": {
       "currency": "credit",
       "base": 8,
       "modifiers": [
         { "when": { "op": "eq", "key": "style_model", "value": "film_a24" }, "op": "add", "value": 2, "label": "A24 风格模型" },
         { "when": { "op": "in", "key": "aspect", "value": ["9:16", "16:9"] }, "op": "mul", "value": 1.2, "label": "宽幅加价" }
       ],
       "multiplier_param": "count",
       "rounding": "ceil"
     },
     "eta": { "p50_seconds": 14, "p90_seconds": 35 },
     "limits": {
       "max_concurrent_per_user": 4,
       "prompt_max_length": 2000,
       "queue_position_available": true
     }
   }',
   '{
     "body_template": { "model": "mock-image-1" },
     "rules": [
       { "from": "prompt", "to": "prompt" },
       { "from": "params.aspect", "to": "size" },
       { "from": "params.count", "to": "n", "cast": "int" },
       { "from": "params.seed", "to": "seed", "cast": "int" },
       { "from": "params.negative_prompt", "to": "negative_prompt" },
       { "from": "inputs.reference_images", "to": "image[]", "wrap": "image_url_part" }
     ]
   }',
   2)
ON DUPLICATE KEY UPDATE
  capability      = VALUES(capability),
  request_mapping = VALUES(request_mapping),
  enabled         = VALUES(enabled);

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L1 能力声明 —— mock 视频模型
-- protocol_family='video' 时 video_protocol 必填（见 chk_models_video_protocol）
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO models
  (id, provider_id, upstream_model, protocol_family, video_protocol, modality,
   enabled, display_order, name, vendor, description, preview_url,
   capability, request_mapping, poll_interval_seconds)
VALUES
  ('mock-video-1',
   '00000000-0000-4000-8000-000000000001',
   'mock-video-1',
   'video',
   'mock',
   'video',
   1,
   10,
   'Mock 视频模型',
   'mock',
   '本地开发用假模型，异步提交 + 轮询，与真实视频驱动同一条路径',
   NULL,
   '{
     "id": "mock-video-1",
     "name": "Mock 视频模型",
     "vendor": "mock",
     "modality": "video",
     "enabled": true,
     "order": 10,
     "description": "通用视频生成，支持首帧控制（本地 mock）",
     "inputs": [
       {
         "key": "first_frame",
         "label": "首帧",
         "kind": "image",
         "required": false,
         "min_count": 0,
         "max_count": 1,
         "accept": ["image/png", "image/jpeg"],
         "max_bytes": 10485760,
         "min_pixels": { "width": 300, "height": 300 },
         "hint": "传图后从该画面开始生成"
       }
     ],
     "params": [
       {
         "key": "resolution",
         "label": "分辨率",
         "control": "select",
         "group": "primary",
         "order": 10,
         "default": "768p",
         "render_hint": "segmented",
         "options": [
           { "value": "512p", "label": "512P" },
           { "value": "768p", "label": "768P" },
           { "value": "1080p", "label": "1080P", "badge": "×2 积分" }
         ]
       },
       {
         "key": "mode",
         "label": "生成模式",
         "control": "select",
         "group": "primary",
         "order": 20,
         "default": "standard",
         "render_hint": "segmented",
         "options": [
           { "value": "standard", "label": "普通模式" },
           { "value": "pro", "label": "专业模式", "badge": "慢" }
         ]
       },
       {
         "key": "duration",
         "label": "时长",
         "control": "select",
         "group": "primary",
         "order": 30,
         "default": 5,
         "render_hint": "segmented",
         "options": [
           { "value": 5, "label": "5s" },
           { "value": 10, "label": "10s", "disabled": false }
         ]
       },
       {
         "key": "camera_motion",
         "label": "运镜",
         "control": "select",
         "group": "advanced",
         "order": 10,
         "default": "auto",
         "options": [
           { "value": "auto", "label": "自动" },
           { "value": "push_in", "label": "推近" },
           { "value": "pan_left", "label": "左摇" },
           { "value": "static", "label": "固定机位" }
         ],
         "enabled_when": { "op": "eq", "key": "mode", "value": "pro" },
         "disabled_hint": "仅专业模式支持指定运镜"
       },
       {
         "key": "seed",
         "label": "随机种子",
         "control": "seed",
         "group": "advanced",
         "order": 20,
         "default": null,
         "min": 0,
         "max": 2147483647,
         "allow_random": true
       }
     ],
     "pricing": {
       "currency": "credit",
       "base": 4,
       "modifiers": [
         { "when": { "op": "eq", "key": "resolution", "value": "1080p" }, "op": "mul", "value": 2, "label": "1080P" },
         { "when": { "op": "eq", "key": "mode", "value": "pro" }, "op": "mul", "value": 1.5, "label": "专业模式" }
       ],
       "multiplier_param": "duration",
       "rounding": "ceil"
     },
     "eta": { "p50_seconds": 120, "p90_seconds": 300, "scales_with": "duration" },
     "limits": {
       "max_concurrent_per_user": 2,
       "prompt_max_length": 1500,
       "queue_position_available": true
     }
   }',
   '{
     "body_template": { "model": "mock-video-1" },
     "rules": [
       { "from": "prompt", "to": "content[]", "wrap": "text_part" },
       { "from": "inputs.first_frame", "to": "content[]", "wrap": "image_url_part" },
       { "from": "params.resolution", "to": "parameters.resolution", "value_map": { "768p": "720p" } },
       { "from": "params.duration", "to": "parameters.duration", "cast": "int" },
       { "from": "params.mode", "to": "parameters.mode" },
       { "from": "params.camera_motion", "to": "parameters.camera_motion", "when": { "op": "eq", "key": "mode", "value": "pro" } },
       { "from": "params.seed", "to": "parameters.seed", "cast": "int" }
     ]
   }',
   3)
ON DUPLICATE KEY UPDATE
  capability      = VALUES(capability),
  request_mapping = VALUES(request_mapping),
  enabled         = VALUES(enabled);

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：skills —— 三个预置创作 Skill
-- system_prompt 只在服务端使用，GET /api/skills 不下发原文
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO skills
  (id, name, description, system_prompt, default_model_id, default_params, enabled, display_order)
VALUES
  ('00000000-0000-4000-8000-000000000101',
   '短剧',
   '把一句剧情梗概拆成可拍摄的分镜脚本，逐镜生成画面',
   '你是短剧分镜导演。把用户给的剧情拆成 3-8 个镜头，每个镜头输出：景别、人物动作、环境、光线、镜头运动。人物外观在所有镜头间保持一致。每镜一段，不写解说词。',
   'mock-video-1',
   '{ "resolution": "768p", "mode": "standard", "duration": 5 }',
   1,
   10),
  ('00000000-0000-4000-8000-000000000102',
   'MV',
   '按歌词节奏产出视觉段落，风格统一、转场连贯',
   '你是音乐短片视觉导演。按用户给的歌词或情绪拆成若干视觉段落，每段描述主体、色调、质感、运镜与节奏点。全片保持同一视觉风格与色彩基调，不出现文字与字幕。',
   'mock-video-1',
   '{ "resolution": "1080p", "mode": "pro", "duration": 5 }',
   1,
   20),
  ('00000000-0000-4000-8000-000000000103',
   '产品广告',
   '围绕单个产品产出商业级展示画面与卖点镜头',
   '你是商业广告创意总监。围绕用户给的产品输出画面描述：产品始终为画面主体且形态不变形，说明材质、背景、布光与拍摄角度，突出一个核心卖点。不生成任何品牌 logo 与文案文字。',
   'mock-image-1',
   '{ "aspect": "1:1", "count": 2, "style_model": "none" }',
   1,
   30)
ON DUPLICATE KEY UPDATE
  description    = VALUES(description),
  system_prompt  = VALUES(system_prompt),
  default_params = VALUES(default_params),
  enabled        = VALUES(enabled);

-- 播种也是一次配置变更：推一个版本号，让在跑的实例刷新模型缓存并换掉 ETag
INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'seed: mock provider + 2 mock models + 3 skills');
