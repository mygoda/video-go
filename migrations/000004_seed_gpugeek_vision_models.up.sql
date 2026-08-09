-- =============================================================================
-- 000004_seed_gpugeek_vision_models.up.sql
--
-- 在 000003 的 gpugeek 供应商下再播三个**多模态输入（视觉理解）**模型。
--
-- 与 000003 的区别只有一句话：那三个是纯文本进、文本出；这三个是
-- **图 + 文进、文本出**。协议还是同一个 /v1/chat/completions，driver 还是
-- openaicompat，差异全部落在 capability.inputs 与 request_mapping 上——
-- 这正是「接一个走已知协议的新模型 = 一条配置，零代码」该有的样子。
--
-- **措辞要分清：这是多模态输入（理解），不是多模态输出（生成）。**
-- 2026-08-09 复核实测（DEM-76）：给 qwen3-omni-flash / qwen-omni-turbo /
-- qwen-vl-max / Gemini-3.1-pro / Doubao-Seed-1.6 传 modalities=["text","image"]
-- 与 ["text","audio"]，五个模型一律回 object=chat.completion 且只有 content
-- 文本，没有 images / audio 字段——该参数被网关静默忽略。**本 key 能调到的
-- 模型里没有任何一个能输出图像或视频**，多模态仅指输入侧。平台主线的
-- 文生图 / 文生视频因此仍然是 mock。把这批模型说成「已接入真实 AIGC 上游」
-- 会让验收看错东西。
--
-- 注：早先此处写的「GPUGeek 网关对 /v1/images/generations 回 method not
-- supported，它只代理 chat/completions」是错的——通用端点是 /predictions，
-- 且真正的卡点是账号未开通生成类模型。详见 000003 头部的复核说明。
--
-- ── 为什么图必须内联（inline: true）─────────────────────────────────────
-- 上游拉不到本平台的素材地址：给远程 URL 实测回 `400 Timeout while
-- downloading url`，而本平台的素材地址在本地部署下是 127.0.0.1，上游更不可能
-- 拉到。因此这三条 mapping 的图片规则一律 inline，把字节 base64 进请求体。
-- 代价是请求体变大，上限见 httpx.MaxInlineBytes，与下面 max_bytes 对齐。
--
-- ── 为什么图槽是可选的（required: false）─────────────────────────────────
-- 输入槽的 Required=false 是平台的**隐式分流点**（见 InputSlotSpec 注释）：
-- 传了图就走看图说话，没传就退化成一次普通文本对话，前端不必发 mode 字段。
-- 设成必填会把「先让模型写一段，再拿它看图」这类连续用法切成两个模型。
--
-- ── 关于 modality='image' ────────────────────────────────────────────────
-- 与 000003 同一个理由、同一个代价：models.modality 只有 image / video
-- 两个取值，而前端对非视频卡片一律按 modality='image' 查模型。这批模型
-- 产出的是文本，填 image 是让它们能被选中的唯一取值，代价是它们也会出现在
-- 图片生成 tab 里、且 text 产物在资产网格里渲染成裂图。干净的解法是新增
-- 一个 text modality（ALTER TABLE + domain + 能力校验 + 前端），
-- 那超出本次接入的范围。
--
-- **credential_ref 存的是环境变量名，不是密钥**，沿用 000003 播下的
-- AIGC_PROVIDER_GPUGEEK_KEY；本迁移不碰 providers 表。
--
-- 幂等：全部 INSERT ... ON DUPLICATE KEY UPDATE，可从零重复执行。
-- =============================================================================

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L1 能力声明 —— 主力视觉理解（Vendor3/qwen3-vl-plus）
--
-- request_mapping 的形状是 OpenAI 视觉消息的标准形态：
--   messages[0].content = [ {type:image_url,...}, {type:text,...} ]
-- body_template 先把 messages[0] 这条空的 user 消息铺好，两条规则再往
-- 它的 content 里按顺序追加。**图在前、文在后**：提问指的是"这张图"，
-- 图先出现更贴近各家的示例，也让追加顺序这件事有确定答案。
--
-- 图槽为空时，inputs 规则走 OmitWhenEmpty 被整条跳过，content 里只剩文本，
-- 仍然是一个合法的 chat 请求——隐式分流就落在这一步。
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO models
  (id, provider_id, upstream_model, protocol_family, video_protocol, modality,
   enabled, display_order, name, vendor, description, preview_url,
   capability, request_mapping, poll_interval_seconds)
VALUES
  ('gpugeek-qwen3-vl-plus',
   '00000000-0000-4000-8000-000000000002',
   'Vendor3/qwen3-vl-plus',
   'chat',
   NULL,
   'image',
   1,
   140,
   'Qwen3-VL Plus · 看图说话',
   'GPUGeek',
   '主力视觉理解模型：给一张图加一句问题，返回文字答案',
   NULL,
   '{
     "id": "gpugeek-qwen3-vl-plus",
     "name": "Qwen3-VL Plus · 看图说话",
     "vendor": "GPUGeek",
     "modality": "image",
     "enabled": true,
     "order": 140,
     "description": "主力视觉理解模型：给一张图加一句问题，返回文字答案",
     "inputs": [
       {
         "key": "image",
         "label": "待识别图片",
         "kind": "image",
         "required": false,
         "min_count": 0,
         "max_count": 1,
         "accept": ["image/png", "image/jpeg", "image/webp"],
         "max_bytes": 8388608,
         "hint": "不传图就是一次普通文本对话"
       }
     ],
     "params": [
       {
         "key": "temperature",
         "label": "发散度",
         "control": "slider",
         "group": "primary",
         "order": 10,
         "default": 0.7,
         "min": 0,
         "max": 2,
         "step": 0.1,
         "help": "越低越稳定复现，越高越发散"
       },
       {
         "key": "max_tokens",
         "label": "最长输出",
         "control": "stepper",
         "group": "advanced",
         "order": 10,
         "default": 2048,
         "min": 256,
         "max": 8192,
         "step": 256,
         "unit": "token"
       }
     ],
     "pricing": {
       "currency": "credit",
       "base": 3,
       "rounding": "ceil"
     },
     "eta": { "p50_seconds": 6, "p90_seconds": 25 },
     "limits": {
       "max_concurrent_per_user": 2,
       "prompt_max_length": 4000,
       "queue_position_available": false
     }
   }',
   '{
     "body_template": { "messages": [ { "role": "user", "content": [] } ] },
     "rules": [
       { "from": "model.upstream_model", "to": "model" },
       { "from": "inputs.image", "to": "messages.0.content[]", "wrap": "image_url_part", "inline": true },
       { "from": "prompt", "to": "messages.0.content[]", "wrap": "text_part" },
       { "from": "params.temperature", "to": "temperature", "cast": "float" },
       { "from": "params.max_tokens", "to": "max_tokens", "cast": "int" }
     ]
   }',
   NULL)
ON DUPLICATE KEY UPDATE
  upstream_model  = VALUES(upstream_model),
  capability      = VALUES(capability),
  request_mapping = VALUES(request_mapping),
  enabled         = VALUES(enabled);

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L1 能力声明 —— 轻量档（Vendor3/qwen3-vl-flash）
-- 与主力同一套形状，只是快、便宜、适合高并发的批量识图。
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO models
  (id, provider_id, upstream_model, protocol_family, video_protocol, modality,
   enabled, display_order, name, vendor, description, preview_url,
   capability, request_mapping, poll_interval_seconds)
VALUES
  ('gpugeek-qwen3-vl-flash',
   '00000000-0000-4000-8000-000000000002',
   'Vendor3/qwen3-vl-flash',
   'chat',
   NULL,
   'image',
   1,
   150,
   'Qwen3-VL Flash · 看图说话（快）',
   'GPUGeek',
   '轻量视觉理解模型，适合批量识图与高并发场景',
   NULL,
   '{
     "id": "gpugeek-qwen3-vl-flash",
     "name": "Qwen3-VL Flash · 看图说话（快）",
     "vendor": "GPUGeek",
     "modality": "image",
     "enabled": true,
     "order": 150,
     "description": "轻量视觉理解模型，适合批量识图与高并发场景",
     "inputs": [
       {
         "key": "image",
         "label": "待识别图片",
         "kind": "image",
         "required": false,
         "min_count": 0,
         "max_count": 1,
         "accept": ["image/png", "image/jpeg", "image/webp"],
         "max_bytes": 8388608,
         "hint": "不传图就是一次普通文本对话"
       }
     ],
     "params": [
       {
         "key": "temperature",
         "label": "发散度",
         "control": "slider",
         "group": "primary",
         "order": 10,
         "default": 0.7,
         "min": 0,
         "max": 2,
         "step": 0.1,
         "help": "越低越稳定复现，越高越发散"
       },
       {
         "key": "max_tokens",
         "label": "最长输出",
         "control": "stepper",
         "group": "advanced",
         "order": 10,
         "default": 1024,
         "min": 256,
         "max": 8192,
         "step": 256,
         "unit": "token"
       }
     ],
     "pricing": {
       "currency": "credit",
       "base": 1,
       "rounding": "ceil"
     },
     "eta": { "p50_seconds": 3, "p90_seconds": 12 },
     "limits": {
       "max_concurrent_per_user": 4,
       "prompt_max_length": 4000,
       "queue_position_available": false
     }
   }',
   '{
     "body_template": { "messages": [ { "role": "user", "content": [] } ] },
     "rules": [
       { "from": "model.upstream_model", "to": "model" },
       { "from": "inputs.image", "to": "messages.0.content[]", "wrap": "image_url_part", "inline": true },
       { "from": "prompt", "to": "messages.0.content[]", "wrap": "text_part" },
       { "from": "params.temperature", "to": "temperature", "cast": "float" },
       { "from": "params.max_tokens", "to": "max_tokens", "cast": "int" }
     ]
   }',
   NULL)
ON DUPLICATE KEY UPDATE
  upstream_model  = VALUES(upstream_model),
  capability      = VALUES(capability),
  request_mapping = VALUES(request_mapping),
  enabled         = VALUES(enabled);

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L1 能力声明 —— 备选（Vendor3/qwen-vl-max）
-- 上一代 VL 的强档。留着它是为了在同一条链路上有个可对照的备选：
-- 主力那条出问题时，换 model_id 就能继续跑，不用改任何代码。
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO models
  (id, provider_id, upstream_model, protocol_family, video_protocol, modality,
   enabled, display_order, name, vendor, description, preview_url,
   capability, request_mapping, poll_interval_seconds)
VALUES
  ('gpugeek-qwen-vl-max',
   '00000000-0000-4000-8000-000000000002',
   'Vendor3/qwen-vl-max',
   'chat',
   NULL,
   'image',
   1,
   160,
   'Qwen-VL Max · 看图说话（备选）',
   'GPUGeek',
   '上一代强档视觉理解模型，作为主力档的备选',
   NULL,
   '{
     "id": "gpugeek-qwen-vl-max",
     "name": "Qwen-VL Max · 看图说话（备选）",
     "vendor": "GPUGeek",
     "modality": "image",
     "enabled": true,
     "order": 160,
     "description": "上一代强档视觉理解模型，作为主力档的备选",
     "inputs": [
       {
         "key": "image",
         "label": "待识别图片",
         "kind": "image",
         "required": false,
         "min_count": 0,
         "max_count": 1,
         "accept": ["image/png", "image/jpeg", "image/webp"],
         "max_bytes": 8388608,
         "hint": "不传图就是一次普通文本对话"
       }
     ],
     "params": [
       {
         "key": "temperature",
         "label": "发散度",
         "control": "slider",
         "group": "primary",
         "order": 10,
         "default": 0.7,
         "min": 0,
         "max": 2,
         "step": 0.1,
         "help": "越低越稳定复现，越高越发散"
       },
       {
         "key": "max_tokens",
         "label": "最长输出",
         "control": "stepper",
         "group": "advanced",
         "order": 10,
         "default": 2048,
         "min": 256,
         "max": 8192,
         "step": 256,
         "unit": "token"
       }
     ],
     "pricing": {
       "currency": "credit",
       "base": 3,
       "rounding": "ceil"
     },
     "eta": { "p50_seconds": 8, "p90_seconds": 30 },
     "limits": {
       "max_concurrent_per_user": 2,
       "prompt_max_length": 4000,
       "queue_position_available": false
     }
   }',
   '{
     "body_template": { "messages": [ { "role": "user", "content": [] } ] },
     "rules": [
       { "from": "model.upstream_model", "to": "model" },
       { "from": "inputs.image", "to": "messages.0.content[]", "wrap": "image_url_part", "inline": true },
       { "from": "prompt", "to": "messages.0.content[]", "wrap": "text_part" },
       { "from": "params.temperature", "to": "temperature", "cast": "float" },
       { "from": "params.max_tokens", "to": "max_tokens", "cast": "int" }
     ]
   }',
   NULL)
ON DUPLICATE KEY UPDATE
  upstream_model  = VALUES(upstream_model),
  capability      = VALUES(capability),
  request_mapping = VALUES(request_mapping),
  enabled         = VALUES(enabled);

-- 播种也是一次配置变更：推一个版本号，让在跑的实例刷新模型缓存并换掉 ETag
INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'seed: gpugeek vision (multimodal-input) chat models');
