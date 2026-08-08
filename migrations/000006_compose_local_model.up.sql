-- =============================================================================
-- 000006_compose_local_model.up.sql
--
-- 一键合成短片（画布 POST /api/canvas/{id}/compose）落地所需的配置。
--
-- 它加的是**配置**而不是代码路径：合成走的是与生成完全相同的任务链路
-- （tasks 表 → 状态机 → 转存 → 血缘 → 记账 → SSE），入口是一个新的协议族
-- compose，对应 internal/adapter/compose 那个本地驱动。
--
-- 关于降级：本地驱动只把片段按顺序编成一份「分镜序列」清单，不做转码拼接
-- （本项目明确不引入 ffmpeg 之类的外部二进制）。清单里已经带上拼接所需的
-- 全部信息，等真正的转码能力接进来时，换掉的只是那个 driver，这里一行不改。
--
-- credential_ref 存的是**环境变量名**，不是密钥；本地驱动根本不读它，
-- 保留该字段只为让它与真实供应商走完全相同的配置形态（同 mock 供应商）。
--
-- 幂等：MODIFY COLUMN 与 INSERT ... ON DUPLICATE KEY UPDATE 都可重复执行。
-- =============================================================================

-- ─────────────────────────────────────────────────────────────────────────────
-- 协议族枚举加一个取值。MySQL 的 ENUM 只能整列重写，因此这里把四个老取值
-- 原样抄回来 —— 漏抄任何一个都会让已有的模型行在下次写入时被拒。
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE models
  MODIFY COLUMN protocol_family ENUM('chat','images','video','mock','compose') NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 层：L0 适配器配置 —— 本地合成"供应商"
-- base_url 写成一个不可路由的本地标识：这条链路不出网，写一个真地址反而
-- 会让人以为它在打某个服务。
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO providers
  (id, name, base_url, credential_ref, auth_style, auth_header_name, enabled, timeout_ms, max_concurrency)
VALUES
  ('00000000-0000-4000-8000-000000000003',
   'local',
   'local://compose',
   'AIGC_PROVIDER_LOCAL_KEY',
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
-- 层：L1 能力声明 —— 本地分镜合成
--
-- 输入槽 composed_from 是三处约定的交汇点：driver 按它挑片段、httpapi 提交
-- 任务时填它、executor 按它记 composed_from 血缘边。min_count=2 是硬要求，
-- 一段的"合成"没有意义。
--
-- 没有任何 params：顺序由 inputs 的数组顺序决定，除此之外没有可调的东西。
-- pricing.base=0 —— 本地编一份清单不打上游，收积分说不过去。
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO models
  (id, provider_id, upstream_model, protocol_family, video_protocol, modality,
   enabled, display_order, name, vendor, description, preview_url,
   capability, request_mapping, poll_interval_seconds)
VALUES
  ('local-compose-1',
   '00000000-0000-4000-8000-000000000003',
   'local-compose-1',
   'compose',
   NULL,
   'video',
   1,
   900,
   '分镜合成（本地）',
   'local',
   '把画布上选中的卡片按顺序编成一份分镜序列；不做转码拼接',
   NULL,
   '{
     "id": "local-compose-1",
     "name": "分镜合成（本地）",
     "vendor": "local",
     "modality": "video",
     "enabled": true,
     "order": 900,
     "description": "把选中的卡片按顺序编成一份分镜序列清单。它是降级形态：只编排顺序，不做转码拼接。",
     "inputs": [
       {
         "key": "composed_from",
         "label": "片段",
         "kind": "video",
         "required": true,
         "min_count": 2,
         "max_count": 12,
         "accept": ["video/mp4", "video/webm", "image/png", "image/jpeg", "image/webp"],
         "max_bytes": 524288000,
         "hint": "按选中的顺序合成"
       }
     ],
     "params": [],
     "pricing": {
       "currency": "credit",
       "base": 0,
       "modifiers": [],
       "rounding": "ceil"
     },
     "eta": { "p50_seconds": 1, "p90_seconds": 3 },
     "limits": {
       "max_concurrent_per_user": 2,
       "prompt_max_length": 200,
       "queue_position_available": false
     }
   }',
   NULL,
   1)
ON DUPLICATE KEY UPDATE
  capability      = VALUES(capability),
  request_mapping = VALUES(request_mapping),
  enabled         = VALUES(enabled);

-- 配置变更推一个版本号，让在跑的实例刷新模型缓存并换掉 ETag
INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'seed: local compose provider + local-compose-1 model');
