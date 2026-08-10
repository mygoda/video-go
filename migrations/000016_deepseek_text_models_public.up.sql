-- =============================================================================
-- 000016_deepseek_text_models_public.up.sql
--
-- 两件事：把 DeepSeek 两个对话模型播进目录，并把「能拿来写剧本」的文本模型
-- 从 internal 放出到 public，让用户在创建剧本时能自己挑模型。
--
-- ── 为什么现在要放出来 ──────────────────────────────────────────────────
-- 000009 把六条 chat 模型一律置成 internal，理由写得很清楚：「text 没有对应
-- 的 tab」。那句话在当时是对的——用户端只有图片 / 视频两个下拉，文本模型
-- 摆出去没有任何位置能选它。现在剧本卡这一层已经存在（000014），剧本正文
-- 就是 chat 模型的产物，「用哪个模型写这一版」第一次成了用户真正要做的选择。
-- 目录里没有 public 的 text 模型，那个选择就无从做起。
--
-- ── 放哪几条，不放哪几条 ────────────────────────────────────────────────
-- 放：DeepSeek-V4-Pro / DeepSeek-V4-Flash（本次新播）、qwen-flash、qwen3-max、
--     GPT-5.2 —— 纯文本进、文本出，正是写剧本要的形状。
-- 不放：qwen3-vl-plus / qwen3-vl-flash / qwen-vl-max —— 这三条是**视觉理解**
--     模型（000004 播的），capability.inputs 里有一个「待识别图片」输入槽。
--     它们确实也能当文本模型用（不传图就退化成普通对话），但摆进「写剧本」
--     的下拉里，用户看到的是三个名字里带「看图说话」、还带一个填不了的图片
--     槽的选项。对这个场景它们是噪音，不是能力。需要时管理端随时能改。
--
-- ── 与 DEM-78「上线前清场」的口径冲突，这里说明 ──────────────────────────
-- DEM-78 的验收是「demo 身份 GET /api/models（不带 modality）只返回 3 个」。
-- 本迁移之后这个数会变成 8（3 个出片模型 + 5 个文本模型）。**这不是回归。**
-- DEM-78 要挡的是 mock 驱动与 QA 夹具混进用户目录（那会让假图冒充作品），
-- 这条线由 000008 的 visibility + chk_models_mock_internal 继续兜着，本迁移
-- 一个字都没动。而「3」这个数字是当时目录里恰好只有 3 条真模型的快照，
-- 不是一条约束。用户端的图片页 / 视频页取模型都带 modality（前端
-- api.models(modality) 的形参是必填的），因此 text 模型不会漏进那两个下拉。
--
-- ── credential 与 provider ──────────────────────────────────────────────
-- 沿用 000003 播下的 gpugeek 供应商与它的 credential_ref（存的是环境变量名，
-- 不是密钥）。上游确实供这两个模型：GET /v1/models 实测 200，101 条里含
-- Vendor3/DeepSeek-V4-Pro 与 Vendor3/DeepSeek-V4-Flash。
--
-- ── 已知：V4-Pro 当前这把 key 调不通 ────────────────────────────────────
-- 实测 Vendor3/DeepSeek-V4-Flash 正常出剧本；Vendor3/DeepSeek-V4-Pro 打
-- /v1/chat/completions 稳定回 400 {"code":400,"message":"model not bound to
-- requested spec"}。裸 curl 同样复现，与本仓代码无关——是这把 key 只列出
-- 了该模型、没有实际绑定算力规格。仍然把它播进目录并置 public：这是账号
-- 开通面的事，等上游绑定后无需再改库。用户点到它会收到上游的 400 原文，
-- 不会静默失败。
--
-- 幂等：INSERT ... ON DUPLICATE KEY UPDATE + 按 id 的 UPDATE，可重复执行。
-- =============================================================================

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L1 能力声明 —— DeepSeek V4 Pro（强档）
--
-- 形状与 000003 的纯文本模型完全一致：没有输入槽，prompt 直接包成一条 user
-- 消息。走 /v1/chat/completions，driver 是 openaicompat，零代码接入。
--
-- max_tokens 默认给 4096 而不是 2048：这一条是给「长剧本 / 整篇改写」用的，
-- 2048 token 写不完一份完整短剧本，会在半句话上被截断。
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO models
  (id, provider_id, upstream_model, protocol_family, video_protocol, modality,
   enabled, visibility, display_order, name, vendor, description, preview_url,
   capability, request_mapping, poll_interval_seconds)
VALUES
  ('gpugeek-deepseek-v4-pro',
   '00000000-0000-4000-8000-000000000002',
   'Vendor3/DeepSeek-V4-Pro',
   'chat',
   NULL,
   'text',
   1,
   'public',
   100,
   'DeepSeek V4 Pro · 剧本',
   'GPUGeek',
   '强档中文文本模型，长剧本与整篇改写用它，结构完整、人物一致性好',
   NULL,
   '{
     "id": "gpugeek-deepseek-v4-pro",
     "name": "DeepSeek V4 Pro · 剧本",
     "vendor": "GPUGeek",
     "modality": "text",
     "enabled": true,
     "order": 100,
     "description": "强档中文文本模型，长剧本与整篇改写用它，结构完整、人物一致性好",
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
         "max": 8192,
         "step": 256,
         "unit": "token"
       }
     ],
     "pricing": {
       "currency": "credit",
       "base": 2,
       "rounding": "ceil"
     },
     "eta": { "p50_seconds": 8, "p90_seconds": 30 },
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
  modality        = VALUES(modality),
  visibility      = VALUES(visibility),
  capability      = VALUES(capability),
  request_mapping = VALUES(request_mapping),
  enabled         = VALUES(enabled);

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L1 能力声明 —— DeepSeek V4 Flash（快档）
-- 同一套形状，快、便宜。「改一句话再看看」这种来回试的用法用它，
-- 每改一版等半分钟会让人干脆不改了。
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO models
  (id, provider_id, upstream_model, protocol_family, video_protocol, modality,
   enabled, visibility, display_order, name, vendor, description, preview_url,
   capability, request_mapping, poll_interval_seconds)
VALUES
  ('gpugeek-deepseek-v4-flash',
   '00000000-0000-4000-8000-000000000002',
   'Vendor3/DeepSeek-V4-Flash',
   'chat',
   NULL,
   'text',
   1,
   'public',
   105,
   'DeepSeek V4 Flash · 剧本（快）',
   'GPUGeek',
   '快档中文文本模型，秒级返回，适合反复改写与短剧本',
   NULL,
   '{
     "id": "gpugeek-deepseek-v4-flash",
     "name": "DeepSeek V4 Flash · 剧本（快）",
     "vendor": "GPUGeek",
     "modality": "text",
     "enabled": true,
     "order": 105,
     "description": "快档中文文本模型，秒级返回，适合反复改写与短剧本",
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
     "eta": { "p50_seconds": 4, "p90_seconds": 15 },
     "limits": {
       "max_concurrent_per_user": 4,
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
  modality        = VALUES(modality),
  visibility      = VALUES(visibility),
  capability      = VALUES(capability),
  request_mapping = VALUES(request_mapping),
  enabled         = VALUES(enabled);

-- ─────────────────────────────────────────────────────────────────────────────
-- 把三条纯文本模型放进用户目录。按 id 逐条列出而不是 WHERE modality='text'：
-- 后者会把三条视觉理解模型一起放出去（000009 之后它们的 modality 也是 text）。
-- ─────────────────────────────────────────────────────────────────────────────
UPDATE models
   SET visibility = 'public'
 WHERE id IN ('gpugeek-qwen-flash', 'gpugeek-qwen3-max', 'gpugeek-gpt-5-2');

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'seed deepseek v4 chat models; publish text models to the user catalog');
