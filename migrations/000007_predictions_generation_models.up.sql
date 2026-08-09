-- =============================================================================
-- 000007_predictions_generation_models.up.sql
--
-- 平台**第一次拿到真实的文生图 / 文生视频能力**：接入 GPUGeek 的模型市场
-- （Replicate 风格 POST /predictions），对应 internal/adapter/predictions。
--
-- ── 这条迁移订正的是一个结论，不只是补三行配置 ──────────────────────────
-- 000003 / 000004 的文件头曾经写着「本 key 能调到的模型里没有任何一个能输出
-- 图像或视频」。那个结论来自只探了 GPUGeek 的 **OpenAI 兼容层**
-- （/v1/models、/v1/chat/completions）。GPUGeek 实际上有**两套互不相通的
-- API 面**，生成类模型全在另一套上：
--
--   /v1/models、/v1/chat/completions   OpenAI 兼容层，只有 LLM 与视觉理解
--   POST /predictions                  模型市场，文生图 / 文生视频都在这里
--
-- /v1/images/generations 回 404 并不代表平台没有图像能力——生成类模型根本
-- 不走那条路。2026-08-09 用同一把 key 实测通过（详见各模型的注释）。
--
-- ── 一个协议，两个驱动实例，两个枚举取值 ────────────────────────────────
-- 同一个 POST /predictions 既出图也出视频，差别只在**耗时**：实测出图约 10 秒、
-- 出视频约 256 秒。10 秒的东西走提交-轮询会白白多绕一圈状态机与一次数据库
-- 往返，256 秒的东西走同步则必然把 HTTP 连接挂死。因此 driver 注册了两个实例，
-- 共用 "predictions" 这一个查找键，靠 Family() 与本表的 protocol_family 对齐
-- 选边（见 adapter.IsAsync）。落到配置上就是：
--
--   出图    protocol_family='predictions'                     → 同步实例
--   出视频  protocol_family='video' + video_protocol='predictions' → 异步实例
--
-- 所以两个枚举列都要加同一个取值 'predictions'，缺任何一个那一侧都配不出来。
--
-- ── 产物只有 24 小时 ────────────────────────────────────────────────────
-- 两种模态的产物都是带签名的对象存储直链（X-Tos-Expires=86400），与 Ark 一致。
-- 这正是 L3 必须在任务成功那一刻立刻转存的原因，不是可以以后再做的优化。
--
-- credential_ref 存的是**环境变量名**，沿用 000003 播下的
-- AIGC_PROVIDER_GPUGEEK_KEY；本迁移不碰 providers 表，密钥不进迁移、不进代码。
--
-- 幂等：MODIFY COLUMN 与 INSERT ... ON DUPLICATE KEY UPDATE 都可重复执行。
-- =============================================================================

-- ─────────────────────────────────────────────────────────────────────────────
-- 两个枚举各加一个取值。MySQL 的 ENUM 只能整列重写，因此老取值必须原样抄回来
-- —— 漏抄任何一个都会让已有的模型行在下次写入时被拒（同 000006 的做法）。
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE models
  MODIFY COLUMN protocol_family ENUM('chat','images','video','mock','compose','predictions') NOT NULL;

ALTER TABLE models
  MODIFY COLUMN video_protocol ENUM('ark','openai_video','google_lro','mock','predictions') NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L1 能力声明 —— 文生图主力（Volcengine/Doubao-Seedream-4.5）
--
-- 走同步实例：实测一次 POST 往返约 10.1~10.6 秒就直接回 succeeded 与产物直链，
-- 根本没有中间态可轮询。
--
-- ── size 为什么是 aspect_select 而不是 select ────────────────────────────
-- 上游只认一个 size 字段，取值是 'WIDTHxHEIGHT' 或 '1k'/'2k'/'4k'。实测
-- 该模型**拒绝 1k**（"the specified size is not supported"），并且要求
-- **总像素不低于 3686400**（低于就回 400）。所以档位串在这里没有可用的取值域，
-- 只能给显式宽高；而显式宽高天然就是「选比例」这件事，用 aspect_select
-- 才能让前端画出比例框。下面五个取值全部实测 ≥ 3686400 像素：
--
--   2048x2048 = 4194304   2560x1440 = 3686400（下限，实测通过）
--   2304x1728 = 3981312   1728x2304 / 1440x2560 同为其转置
--
-- watermark 收进高级面板：它是个二值开关，占主参数条不值当。默认 false ——
-- 平台自己的产物不该被上游打水印。
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO models
  (id, provider_id, upstream_model, protocol_family, video_protocol, modality,
   enabled, display_order, name, vendor, description, preview_url,
   capability, request_mapping, poll_interval_seconds)
VALUES
  ('gpugeek-seedream-4-5',
   '00000000-0000-4000-8000-000000000002',
   'Volcengine/Doubao-Seedream-4.5',
   'predictions',
   NULL,
   'image',
   1,
   5,
   'Doubao Seedream 4.5 · 文生图',
   'GPUGeek',
   '真实文生图模型，一次调用约 10 秒出一张 2K 级别的图',
   NULL,
   '{
     "id": "gpugeek-seedream-4-5",
     "name": "Doubao Seedream 4.5 · 文生图",
     "vendor": "GPUGeek",
     "modality": "image",
     "enabled": true,
     "order": 5,
     "description": "真实文生图模型，一次调用约 10 秒出一张 2K 级别的图",
     "inputs": [],
     "params": [
       {
         "key": "size",
         "label": "画幅",
         "control": "aspect_select",
         "group": "primary",
         "order": 10,
         "default": "2048x2048",
         "render_hint": "segmented",
         "help": "上游要求总像素不低于 3686400，因此各比例都在 2K 级别",
         "options": [
           { "value": "2048x2048", "label": "1:1", "ratio_w": 1, "ratio_h": 1, "pixels": { "width": 2048, "height": 2048 } },
           { "value": "2560x1440", "label": "16:9", "ratio_w": 16, "ratio_h": 9, "pixels": { "width": 2560, "height": 1440 } },
           { "value": "1440x2560", "label": "9:16", "ratio_w": 9, "ratio_h": 16, "pixels": { "width": 1440, "height": 2560 } },
           { "value": "2304x1728", "label": "4:3", "ratio_w": 4, "ratio_h": 3, "pixels": { "width": 2304, "height": 1728 } },
           { "value": "1728x2304", "label": "3:4", "ratio_w": 3, "ratio_h": 4, "pixels": { "width": 1728, "height": 2304 } }
         ]
       },
       {
         "key": "watermark",
         "label": "上游水印",
         "control": "toggle",
         "group": "advanced",
         "order": 10,
         "default": false,
         "help": "开启后由上游在产物右下角打水印"
       }
     ],
     "pricing": {
       "currency": "credit",
       "base": 10,
       "modifiers": [],
       "rounding": "ceil"
     },
     "eta": { "p50_seconds": 11, "p90_seconds": 25 },
     "limits": {
       "max_concurrent_per_user": 2,
       "prompt_max_length": 4000,
       "queue_position_available": false
     }
   }',
   '{
     "body_template": { "model": "", "input": {} },
     "rules": [
       { "from": "model.upstream_model", "to": "model" },
       { "from": "prompt", "to": "input.prompt" },
       { "from": "params.size", "to": "input.size" },
       { "from": "params.watermark", "to": "input.watermark", "cast": "bool" }
     ]
   }',
   NULL)
ON DUPLICATE KEY UPDATE
  upstream_model  = VALUES(upstream_model),
  protocol_family = VALUES(protocol_family),
  video_protocol  = VALUES(video_protocol),
  capability      = VALUES(capability),
  request_mapping = VALUES(request_mapping),
  enabled         = VALUES(enabled);

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L1 能力声明 —— 文生视频主力（Volcengine/Doubao-Seedance-2.0）
--
-- 走异步实例：提交回 {"id":..., "status":"processing"}，实测约 256 秒后
-- 轮询到 succeeded，产物是一个 mp4 直链。
--
-- ── 三个参数的取值域是从上游错误消息里问出来的，不是猜的 ─────────────────
-- 故意传非法值，上游会把合法集合原样列出来（2026-08-09 实测）：
--   duration    "duration must be -1 or in range [4, 15] seconds"
--   resolution  "resolution must be one of: 480p, 720p, 1080p"
--   ratio       "ratio must be one of: 21:9, 16:9, 9:16, 1:1, 4:3, 3:4, adaptive"
-- duration 的 -1 是「由上游自己定」，平台不暴露：它会让费用与 ETA 都算不出来。
-- ratio 的 adaptive 同理不暴露——它是给图生视频按参考图定比例用的，
-- 而本模型这一轮只配了文生视频，没有输入槽。
--
-- ── 计价与 ETA 都挂在 duration 上 ──────────────────────────────────────
-- multiplier_param 在 modifiers 之后整体相乘（见 PricingSpec 的算法注释），
-- 因此 4 秒 / 480p = 5 × 1 × 4 = 20 积分，4 秒 / 1080p = 5 × 4 × 4 = 80 积分。
-- eta.scales_with 同样指向 duration：256 秒是 4 秒片子的实测值，
-- 15 秒的片子按线性放大给用户一个不至于离谱的进度条。
--
-- ── 默认 480p 是刻意的 ─────────────────────────────────────────────────
-- p50=256 秒是在 4 秒 / 480p 下实测出来的。720p / 1080p 上游支持但**没有实测过**，
-- 把默认值放在没测过的档位上，等于让 ETA 从第一次使用起就是假的。
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO models
  (id, provider_id, upstream_model, protocol_family, video_protocol, modality,
   enabled, display_order, name, vendor, description, preview_url,
   capability, request_mapping, poll_interval_seconds)
VALUES
  ('gpugeek-seedance-2-0',
   '00000000-0000-4000-8000-000000000002',
   'Volcengine/Doubao-Seedance-2.0',
   'video',
   'predictions',
   'video',
   1,
   5,
   'Doubao Seedance 2.0 · 文生视频',
   'GPUGeek',
   '真实文生视频模型，4 秒片子约 4 分钟出片',
   NULL,
   '{
     "id": "gpugeek-seedance-2-0",
     "name": "Doubao Seedance 2.0 · 文生视频",
     "vendor": "GPUGeek",
     "modality": "video",
     "enabled": true,
     "order": 5,
     "description": "真实文生视频模型，4 秒片子约 4 分钟出片",
     "inputs": [],
     "params": [
       {
         "key": "duration",
         "label": "时长",
         "control": "stepper",
         "group": "primary",
         "order": 10,
         "default": 4,
         "min": 4,
         "max": 15,
         "step": 1,
         "unit": "秒",
         "help": "时长直接决定出片时间与费用"
       },
       {
         "key": "resolution",
         "label": "清晰度",
         "control": "select",
         "group": "primary",
         "order": 20,
         "default": "480p",
         "render_hint": "segmented",
         "options": [
           { "value": "480p", "label": "480P" },
           { "value": "720p", "label": "720P", "badge": "×2 积分" },
           { "value": "1080p", "label": "1080P", "badge": "×4 积分" }
         ]
       },
       {
         "key": "ratio",
         "label": "画幅",
         "control": "aspect_select",
         "group": "primary",
         "order": 30,
         "default": "16:9",
         "options": [
           { "value": "21:9", "label": "21:9", "ratio_w": 21, "ratio_h": 9 },
           { "value": "16:9", "label": "16:9", "ratio_w": 16, "ratio_h": 9 },
           { "value": "9:16", "label": "9:16", "ratio_w": 9, "ratio_h": 16 },
           { "value": "1:1", "label": "1:1", "ratio_w": 1, "ratio_h": 1 },
           { "value": "4:3", "label": "4:3", "ratio_w": 4, "ratio_h": 3 },
           { "value": "3:4", "label": "3:4", "ratio_w": 3, "ratio_h": 4 }
         ]
       }
     ],
     "pricing": {
       "currency": "credit",
       "base": 5,
       "modifiers": [
         { "when": { "op": "eq", "key": "resolution", "value": "720p" }, "op": "mul", "value": 2, "label": "720P 加价" },
         { "when": { "op": "eq", "key": "resolution", "value": "1080p" }, "op": "mul", "value": 4, "label": "1080P 加价" }
       ],
       "multiplier_param": "duration",
       "rounding": "ceil"
     },
     "eta": { "p50_seconds": 256, "p90_seconds": 420, "scales_with": "duration" },
     "limits": {
       "max_concurrent_per_user": 1,
       "prompt_max_length": 4000,
       "queue_position_available": false
     }
   }',
   '{
     "body_template": { "model": "", "input": {} },
     "rules": [
       { "from": "model.upstream_model", "to": "model" },
       { "from": "prompt", "to": "input.prompt" },
       { "from": "params.duration", "to": "input.duration", "cast": "int" },
       { "from": "params.resolution", "to": "input.resolution" },
       { "from": "params.ratio", "to": "input.ratio" }
     ]
   }',
   NULL)
ON DUPLICATE KEY UPDATE
  upstream_model  = VALUES(upstream_model),
  protocol_family = VALUES(protocol_family),
  video_protocol  = VALUES(video_protocol),
  capability      = VALUES(capability),
  request_mapping = VALUES(request_mapping),
  enabled         = VALUES(enabled);

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L1 能力声明 —— 文生视频轻量档（Volcengine/Doubao-Seedance-2.0-fast）
--
-- 与主力同一个协议、同一份参数取值域（实测三条错误消息逐字相同），
-- 差别只在快与便宜：同样 4 秒 / 480p / 16:9，实测 85 秒左右出片，
-- 是主力那条（256 秒）的三分之一。留着它的第二个理由与 000004 的备选档一样：
-- 主力那条出问题时，换 model_id 就能继续跑，不用改任何代码。
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO models
  (id, provider_id, upstream_model, protocol_family, video_protocol, modality,
   enabled, display_order, name, vendor, description, preview_url,
   capability, request_mapping, poll_interval_seconds)
VALUES
  ('gpugeek-seedance-2-0-fast',
   '00000000-0000-4000-8000-000000000002',
   'Volcengine/Doubao-Seedance-2.0-fast',
   'video',
   'predictions',
   'video',
   1,
   6,
   'Doubao Seedance 2.0 Fast · 文生视频（快）',
   'GPUGeek',
   '轻量文生视频模型，出片更快、更便宜',
   NULL,
   '{
     "id": "gpugeek-seedance-2-0-fast",
     "name": "Doubao Seedance 2.0 Fast · 文生视频（快）",
     "vendor": "GPUGeek",
     "modality": "video",
     "enabled": true,
     "order": 6,
     "description": "轻量文生视频模型，出片更快、更便宜",
     "inputs": [],
     "params": [
       {
         "key": "duration",
         "label": "时长",
         "control": "stepper",
         "group": "primary",
         "order": 10,
         "default": 4,
         "min": 4,
         "max": 15,
         "step": 1,
         "unit": "秒",
         "help": "时长直接决定出片时间与费用"
       },
       {
         "key": "resolution",
         "label": "清晰度",
         "control": "select",
         "group": "primary",
         "order": 20,
         "default": "480p",
         "render_hint": "segmented",
         "options": [
           { "value": "480p", "label": "480P" },
           { "value": "720p", "label": "720P", "badge": "×2 积分" },
           { "value": "1080p", "label": "1080P", "badge": "×4 积分" }
         ]
       },
       {
         "key": "ratio",
         "label": "画幅",
         "control": "aspect_select",
         "group": "primary",
         "order": 30,
         "default": "16:9",
         "options": [
           { "value": "21:9", "label": "21:9", "ratio_w": 21, "ratio_h": 9 },
           { "value": "16:9", "label": "16:9", "ratio_w": 16, "ratio_h": 9 },
           { "value": "9:16", "label": "9:16", "ratio_w": 9, "ratio_h": 16 },
           { "value": "1:1", "label": "1:1", "ratio_w": 1, "ratio_h": 1 },
           { "value": "4:3", "label": "4:3", "ratio_w": 4, "ratio_h": 3 },
           { "value": "3:4", "label": "3:4", "ratio_w": 3, "ratio_h": 4 }
         ]
       }
     ],
     "pricing": {
       "currency": "credit",
       "base": 2,
       "modifiers": [
         { "when": { "op": "eq", "key": "resolution", "value": "720p" }, "op": "mul", "value": 2, "label": "720P 加价" },
         { "when": { "op": "eq", "key": "resolution", "value": "1080p" }, "op": "mul", "value": 4, "label": "1080P 加价" }
       ],
       "multiplier_param": "duration",
       "rounding": "ceil"
     },
     "eta": { "p50_seconds": 90, "p90_seconds": 180, "scales_with": "duration" },
     "limits": {
       "max_concurrent_per_user": 2,
       "prompt_max_length": 4000,
       "queue_position_available": false
     }
   }',
   '{
     "body_template": { "model": "", "input": {} },
     "rules": [
       { "from": "model.upstream_model", "to": "model" },
       { "from": "prompt", "to": "input.prompt" },
       { "from": "params.duration", "to": "input.duration", "cast": "int" },
       { "from": "params.resolution", "to": "input.resolution" },
       { "from": "params.ratio", "to": "input.ratio" }
     ]
   }',
   NULL)
ON DUPLICATE KEY UPDATE
  upstream_model  = VALUES(upstream_model),
  protocol_family = VALUES(protocol_family),
  video_protocol  = VALUES(video_protocol),
  capability      = VALUES(capability),
  request_mapping = VALUES(request_mapping),
  enabled         = VALUES(enabled);

-- 播种也是一次配置变更：推一个版本号，让在跑的实例刷新模型缓存并换掉 ETag
INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'seed: gpugeek predictions generation models (seedream 4.5 / seedance 2.0 / seedance 2.0-fast)');
