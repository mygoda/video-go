-- =============================================================================
-- 000015_model_param_completeness.down.sql
--
-- 把四行模型配置还原成 **000015 执行前那一刻** 的样子。
--
-- 为什么是整段抄回而不是「反向打补丁」：up 改的是几个 JSON 列的整体取值，
-- 没有增量可撤。要么把原文抄回来，要么写一堆 JSON_REMOVE / JSON_SET ——
-- 后者每多一处改动就多一条路径，且任何一条写错都只在回退那一刻才暴露。
-- 抄回来是可以逐字比对的：下面几段 JSON 不含 up 新增的 inputs、seed、
-- 4K size 档，fast 的 1080p 与它的计价 modifier 也一并回来。
--
-- ## 「迁移前那一刻」不等于「000007 的原文」
--
-- 三个 gpugeek 模型是 000007 播的、mock-image-1 是 000002 播的，但那之后
-- 000013 又用 JSON_SET 把 seedance-fast 的 max_concurrent_per_user 从 2 抬到
-- 了 4（分镜一次派 3 条任务，2 会让第 3 条在提交时被顶回来）。回退的语义是
-- 「回到迁移前那一刻」，所以这里写 4 而不是 000007 里的 2——照抄 000007
-- 会让一次本该无损的回退顺手把分镜功能弄坏。
--
-- 只写这三列。up 也只碰这三列 —— upstream_model / protocol_family /
-- display_order / provider_id 一个都没动过，回退就不该去写它们。
-- =============================================================================


-- 1 / Seedance 2.0 —— 回到无输入槽、无 seed
UPDATE models SET
  capability = '{
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
  request_mapping = '{
     "body_template": { "model": "", "input": {} },
     "rules": [
       { "from": "model.upstream_model", "to": "model" },
       { "from": "prompt", "to": "input.prompt" },
       { "from": "params.duration", "to": "input.duration", "cast": "int" },
       { "from": "params.resolution", "to": "input.resolution" },
       { "from": "params.ratio", "to": "input.ratio" }
     ]
   }',
  description = '真实文生视频模型，4 秒片子约 4 分钟出片'
WHERE id = 'gpugeek-seedance-2-0';


-- 2 / Seedance 2.0 Fast —— 连同 1080p 一起回来
--
-- 注意这里**故意**把 1080p 与它的 ×4 modifier 抄回去。回退的语义是「回到
-- 迁移前那一刻的状态」，不是「保留我认为对的那部分」；1080p 在 fast 上会
-- 400 是 up 修的问题，回退就该把那个问题原样还回去，否则 down 之后的库
-- 与 000007 之后的库不是同一个东西，下次再 up 就不可比。
UPDATE models SET
  capability = '{
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
       "max_concurrent_per_user": 4,
       "prompt_max_length": 4000,
       "queue_position_available": false
     }
   }',
  request_mapping = '{
     "body_template": { "model": "", "input": {} },
     "rules": [
       { "from": "model.upstream_model", "to": "model" },
       { "from": "prompt", "to": "input.prompt" },
       { "from": "params.duration", "to": "input.duration", "cast": "int" },
       { "from": "params.resolution", "to": "input.resolution" },
       { "from": "params.ratio", "to": "input.ratio" }
     ]
   }',
  description = '轻量文生视频模型，出片更快、更便宜'
WHERE id = 'gpugeek-seedance-2-0-fast';


-- 3 / Seedream 4.5 —— size 收回原来的 5 档
--
-- request_mapping 不写：up 没改过它（size / watermark 两条规则原样保留），
-- 这里再写一遍只会给「down 到底改了什么」增加噪声。
UPDATE models SET
  capability = '{
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
  description = '真实文生图模型，一次调用约 10 秒出一张 2K 级别的图'
WHERE id = 'gpugeek-seedream-4-5';


-- 4 / mock-image-1 —— 把两条补上的规则再摘掉
--
-- 同上一条的道理：回退就是回到 000007 之后、000015 之前那一刻，
-- 哪怕摘掉的是一处明摆着的错。摘掉之后仓库根的 TestSeededModelsParamCoverage
-- 会重新变红，那是对的——它红的正是这个 down 恢复出来的那个缺陷。
UPDATE models SET
  request_mapping = '{
     "body_template": { "model": "mock-image-1" },
     "rules": [
       { "from": "prompt", "to": "prompt" },
       { "from": "params.aspect", "to": "size" },
       { "from": "params.count", "to": "n", "cast": "int" },
       { "from": "params.seed", "to": "seed", "cast": "int" },
       { "from": "params.negative_prompt", "to": "negative_prompt" },
       { "from": "inputs.reference_images", "to": "image[]", "wrap": "image_url_part" }
     ]
   }'
WHERE id = 'mock-image-1';


-- 回退同样是一次配置变更，照样推版本号。
INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'revert: models back to 000007 param surface');
