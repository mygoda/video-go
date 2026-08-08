-- =============================================================================
-- 000003_seed_gpugeek_provider.up.sql
--
-- 接入 GPUGeek 作为第一个**真实**供应商，只接 chat 家族。
--
-- 为什么只有 chat：GPUGeek 的 /v1/models 返回 101 个模型，全部是 LLM /
-- 多模态理解，没有任何文生图、文生视频模型；/v1/images/generations 与
-- /v1/video/generations 都返回「模型不存在或无权限」。因此图像与视频继续走
-- mock，接一个不存在的端点只会把失败推迟到用户提交任务的那一刻。
--
-- 为什么不写新 driver：GPUGeek 走标准 OpenAI 兼容协议，
-- internal/adapter/openaicompat 的 chat 分支与 PathChatCompletions 逐字匹配。
-- 「接一个走已知协议的新模型 = 一条配置，零代码」这条判定线在这里第一次
-- 被真实上游检验，本迁移就是那条配置。
--
-- **credential_ref 存的是环境变量名，不是密钥。** 真实 key 只存在于仓库根目录
-- 的 .env（已在 .gitignore 里）。迁移文件进版本库、会被复制、会进 CI 日志，
-- 把密钥写在这里等于把它公开。
--
-- 关于 modality='image'：chat 模型产出的是文本，但 models.modality 只有
-- image / video 两个取值，而前端（CardRerunPanel）对非视频卡片一律按
-- modality='image' 查模型。填 image 是让这几个模型能真正被画布的
-- 剧本/文案路径选中的唯一取值；代价是它们同时出现在图片生成 tab 里。
--
-- 幂等：全部使用 INSERT ... ON DUPLICATE KEY UPDATE，可从零重复执行。
-- =============================================================================

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L0 适配器配置 —— GPUGeek 供应商
-- 鉴权是 Authorization: Bearer <key>，与 auth_style='bearer' 一致。
-- timeout_ms 给到 120s：强档模型输出长文本时单次往返可以到分钟级，
-- 而 chat 是同步驱动（一次往返拿结果，没有轮询兜底），超时就是任务失败。
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO providers
  (id, name, base_url, credential_ref, auth_style, auth_header_name, enabled, timeout_ms, max_concurrency)
VALUES
  ('00000000-0000-4000-8000-000000000002',
   'gpugeek',
   'https://api.gpugeek.com',
   'AIGC_PROVIDER_GPUGEEK_KEY',
   'bearer',
   'Authorization',
   1,
   120000,
   8)
ON DUPLICATE KEY UPDATE
  base_url        = VALUES(base_url),
  credential_ref  = VALUES(credential_ref),
  auth_style      = VALUES(auth_style),
  enabled         = VALUES(enabled),
  timeout_ms      = VALUES(timeout_ms),
  max_concurrency = VALUES(max_concurrency);

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L1 能力声明 —— GPUGeek 快档（Vendor3/qwen-flash）
--
-- upstream_model 与 /v1/models 返回的 id 逐字一致，包含 "Vendor3/" 前缀——
-- 上游按这个字符串路由，少一个前缀就是 404。
--
-- request_mapping 里 messages 用 wrap=user_message 生成
-- {"role":"user","content":"<提示词>"}。system prompt 不在这里配：
-- 画布的 Skill 已经带了 system_prompt，两处都塞会得到两条互相打架的指令。
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO models
  (id, provider_id, upstream_model, protocol_family, video_protocol, modality,
   enabled, display_order, name, vendor, description, preview_url,
   capability, request_mapping, poll_interval_seconds)
VALUES
  ('gpugeek-qwen-flash',
   '00000000-0000-4000-8000-000000000002',
   'Vendor3/qwen-flash',
   'chat',
   NULL,
   'image',
   1,
   110,
   'Qwen Flash · 文本',
   'GPUGeek',
   '快档文本模型，用于剧本分镜、文案与提示词润色，秒级返回',
   NULL,
   '{
     "id": "gpugeek-qwen-flash",
     "name": "Qwen Flash · 文本",
     "vendor": "GPUGeek",
     "modality": "image",
     "enabled": true,
     "order": 110,
     "description": "快档文本模型，用于剧本分镜、文案与提示词润色，秒级返回",
     "inputs": [],
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
     "rules": [
       { "from": "model.upstream_model", "to": "model" },
       { "from": "prompt", "to": "messages[]", "wrap": "user_message" },
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
-- 层：L1 能力声明 —— GPUGeek 强档（Vendor3/qwen3-max）
-- 与快档同一套参数形态，只是慢、贵、更会写长剧本。
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO models
  (id, provider_id, upstream_model, protocol_family, video_protocol, modality,
   enabled, display_order, name, vendor, description, preview_url,
   capability, request_mapping, poll_interval_seconds)
VALUES
  ('gpugeek-qwen3-max',
   '00000000-0000-4000-8000-000000000002',
   'Vendor3/qwen3-max',
   'chat',
   NULL,
   'image',
   1,
   120,
   'Qwen3 Max · 文本',
   'GPUGeek',
   '强档文本模型，长剧本与复杂改写用它，比快档慢但结构更完整',
   NULL,
   '{
     "id": "gpugeek-qwen3-max",
     "name": "Qwen3 Max · 文本",
     "vendor": "GPUGeek",
     "modality": "image",
     "enabled": true,
     "order": 120,
     "description": "强档文本模型，长剧本与复杂改写用它，比快档慢但结构更完整",
     "inputs": [],
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
         "default": 4096,
         "min": 256,
         "max": 16384,
         "step": 256,
         "unit": "token"
       }
     ],
     "pricing": {
       "currency": "credit",
       "base": 4,
       "rounding": "ceil"
     },
     "eta": { "p50_seconds": 12, "p90_seconds": 45 },
     "limits": {
       "max_concurrent_per_user": 2,
       "prompt_max_length": 8000,
       "queue_position_available": false
     }
   }',
   '{
     "rules": [
       { "from": "model.upstream_model", "to": "model" },
       { "from": "prompt", "to": "messages[]", "wrap": "user_message" },
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
-- 层：L1 能力声明 —— GPUGeek 强档备选（Vendor2/GPT-5.2）
-- 留一个非 Qwen 系的强档：同一条链路上换一家上游，
-- 是验证「零代码接模型」这条判定线最直接的方式。
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO models
  (id, provider_id, upstream_model, protocol_family, video_protocol, modality,
   enabled, display_order, name, vendor, description, preview_url,
   capability, request_mapping, poll_interval_seconds)
VALUES
  ('gpugeek-gpt-5-2',
   '00000000-0000-4000-8000-000000000002',
   'Vendor2/GPT-5.2',
   'chat',
   NULL,
   'image',
   1,
   130,
   'GPT-5.2 · 文本',
   'GPUGeek',
   '强档文本模型，英文与结构化输出更稳',
   NULL,
   '{
     "id": "gpugeek-gpt-5-2",
     "name": "GPT-5.2 · 文本",
     "vendor": "GPUGeek",
     "modality": "image",
     "enabled": true,
     "order": 130,
     "description": "强档文本模型，英文与结构化输出更稳",
     "inputs": [],
     "params": [
       {
         "key": "temperature",
         "label": "发散度",
         "control": "slider",
         "group": "primary",
         "order": 10,
         "default": 1,
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
         "default": 4096,
         "min": 256,
         "max": 16384,
         "step": 256,
         "unit": "token"
       }
     ],
     "pricing": {
       "currency": "credit",
       "base": 6,
       "rounding": "ceil"
     },
     "eta": { "p50_seconds": 15, "p90_seconds": 60 },
     "limits": {
       "max_concurrent_per_user": 2,
       "prompt_max_length": 8000,
       "queue_position_available": false
     }
   }',
   '{
     "rules": [
       { "from": "model.upstream_model", "to": "model" },
       { "from": "prompt", "to": "messages[]", "wrap": "user_message" },
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
VALUES (NULL, 'seed: gpugeek provider + 3 chat models');
