-- =============================================================================
-- 000015_model_param_completeness.up.sql
--
-- 把三个公开模型的能力声明补到与上游**实际**支持的一致。000007 播下这三行时
-- 只探到了「文生」那一半，`capability.inputs` 一律是 `[]`——平台等于对外宣称
-- 这些模型不接受任何输入素材。整套输入槽机制（InputSlotSpec / 隐式分流 /
-- inputs.<slot> 映射源）在生产数据上一次都没被点亮过。
--
-- 本迁移改四件事，前三件全部有 2026-08-10 的实测依据：
--   1. Seedance 两个模型开首帧 / 尾帧输入槽，走 images[] + inline
--   2. Seedance 两个模型补 seed 参数；seedance-fast 撤掉上游不支持的 1080p
--   3. Seedream 的 size 从 5 个写死的 2K 档扩到覆盖真实取值域的 11 档
--   4. mock-image-1 补两条漏掉的映射规则（由本轮新加的不变量测试查出来的）
--
-- ── 探测手法：靠参数校验报错，不烧积分 ──────────────────────────────────
-- 上游对非法取值一律先 400 再谈生成，因此「这个字段存不存在、取值域是什么」
-- 全部可以零成本问出来。下面每条结论后面跟的都是上游原样返回的错误消息。
--
-- =============================================================================


-- ─────────────────────────────────────────────────────────────────────────────
-- 1 / Seedance 2.0（主力）
--
-- ## 首帧 / 尾帧：上游只有一个按位置定序的 images[]
--
-- 实测（POST /predictions，Volcengine/Doubao-Seedance-2.0[-fast]）：
--   {"images":["a","b","c"]}  → first/last_frame type images[] must contain 1 or 2 images
--   {"images":[<data uri>]}   → 200（请求被接受）
--   {"images":["http://localhost:18080/..."]}
--                             → The parameter `content[0].image_url` specified in
--                               the request is not valid: resource download failed.
--
-- 三条结论，逐条对应下面的配置：
--
-- (a) **必须 inline。** 上游回源拉不到本平台的地址——本地部署时 PublicBaseURL
--     就是 localhost，线上也未必对上游可达。给地址的失败形态是几十秒后一条
--     「resource download failed」，而 inline 把它变成确定性的一步。
--     mapping 的 Inline 走 data:<mime>;base64,<...>，上游实测收。
--
-- (b) **wrap 留空。** images[] 的元素是裸字符串，不是 {"type":"image_url",...}。
--     套壳子会被顶回来（images[0] must be a non-empty string）。
--
-- (c) **规则顺序就是首帧尾帧的语义。** images[0] 是首帧、images[1] 是尾帧，
--     上游没有任何字段能表达「只给尾帧」——taskType 穷举 20 个候选值全部无效，
--     报错一律回落成 first/last_frame。因此尾帧槽声明 requires_slot=first_frame：
--     只传尾帧的提交在校验期就被拒，而不是渲染成 images[0]=尾帧、被上游当首帧用，
--     产出一条「成功」但语义相反的片子。
--     mapping 包保证追加顺序即规则顺序（见其包注释），这条契约在这里被用上了。
--
-- ## 尾帧槽为什么一起开
--
-- 上游收 2 张时语义就是首+尾，这是真实存在、且只差一条配置的能力。不开它，
-- 「首尾帧」这个上游能力在平台上永远不可达；开它而不声明依赖，则会多出一类
-- 沉默的坏产物。requires_slot 让两者都不必发生。
--
-- ## min_pixels 300×300
--
-- 小于 300×300 的首帧上游会 failed。声明成 min_pixels，前端读完图就能拦、
-- 后端提交时再拦一道（httpapi.assertInputUploads），都发生在花钱之前。
--
-- ## seed
--
-- 实测 {"seed":"zzz"} → seed must be an integer；{"seed":-5} →
-- seed must be in range [-1, 2^32-1]。是真字段，我们一直没暴露。
-- default 为 null = 每次随机，配 allow_random；omit_when_empty 默认为真，
-- 因此不填时请求体里根本没有 seed，与本迁移之前的行为逐字一致。
-- ─────────────────────────────────────────────────────────────────────────────
UPDATE models SET
  capability = '{
     "id": "gpugeek-seedance-2-0",
     "name": "Doubao Seedance 2.0 · 文生视频",
     "vendor": "GPUGeek",
     "modality": "video",
     "enabled": true,
     "order": 5,
     "description": "真实文生视频模型，4 秒片子约 4 分钟出片；可选传首帧 / 尾帧图",
     "inputs": [
       {
         "key": "first_frame",
         "label": "首帧",
         "kind": "image",
         "required": false,
         "min_count": 0,
         "max_count": 1,
         "accept": ["image/png", "image/jpeg", "image/webp"],
         "max_bytes": 8388608,
         "min_pixels": { "width": 300, "height": 300 },
         "hint": "传图后从该画面开始生成；小于 300×300 上游会失败"
       },
       {
         "key": "last_frame",
         "label": "尾帧",
         "kind": "image",
         "required": false,
         "min_count": 0,
         "max_count": 1,
         "accept": ["image/png", "image/jpeg", "image/webp"],
         "max_bytes": 8388608,
         "min_pixels": { "width": 300, "height": 300 },
         "requires_slot": "first_frame",
         "hint": "生成到该画面结束；上游按位置识别首尾帧，因此必须与首帧一起传"
       }
     ],
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
       },
       {
         "key": "seed",
         "label": "随机种子",
         "control": "seed",
         "group": "advanced",
         "order": 20,
         "default": null,
         "min": 0,
         "max": 4294967295,
         "allow_random": true,
         "help": "固定种子可复现同一次生成；留空则每次随机"
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
       { "from": "inputs.first_frame", "to": "input.images[]", "inline": true },
       { "from": "inputs.last_frame", "to": "input.images[]", "inline": true },
       { "from": "params.duration", "to": "input.duration", "cast": "int" },
       { "from": "params.resolution", "to": "input.resolution" },
       { "from": "params.ratio", "to": "input.ratio" },
       { "from": "params.seed", "to": "input.seed", "cast": "int" }
     ]
   }',
  description = '真实文生视频模型，4 秒片子约 4 分钟出片；可选传首帧 / 尾帧图'
WHERE id = 'gpugeek-seedance-2-0';


-- ─────────────────────────────────────────────────────────────────────────────
-- 2 / Seedance 2.0 Fast
--
-- 与主力同一套输入槽与 mapping。**但清晰度取值域不同**：
--
--   fast  {"resolution":"zzz"} → resolution must be one of: 480p, 720p
--   主力  {"resolution":"zzz"} → resolution must be one of: 480p, 720p, 1080p
--
-- 000007 的注释说两个模型「实测三条错误消息逐字相同」，那一条是错的。
-- 后果不是少一个选项：1080p 的计价 modifier 会把预估费用乘 4、按这个数冻结积分，
-- 然后上游 400。撤掉这个选项才是与上游对齐。
--
-- ## max_concurrent_per_user 必须是 4，不是 000007 里的 2
--
-- 000007 播的是 2，000013 用 JSON_SET 把它改成了 4——分镜一次派 3 条任务，
-- 而并发上限连排队中的任务一起算，上限 2 会让第 3 个镜头在提交时就被顶回来。
-- 本迁移整块重写 capability，**等于把 000013 的那一笔悄悄擦掉**：JSON 列是
-- 整值写入，先前所有针对某个路径的 JSON_SET 都会被后来的整值写覆盖，而这件事
-- 不会有任何报错。所以这里逐字带上 4。
--
-- 同理，将来任何一次整块重写 capability 的迁移，都要先查一遍在它之前有没有
-- 针对同一行的 JSON_SET。catalog_test.go 的 TestSeedanceFastFitsStoryboardBatch
-- 把这条具体的约束钉住了，但它只拦得住这一个字段，别的字段仍要靠人过一遍。
-- ─────────────────────────────────────────────────────────────────────────────
UPDATE models SET
  capability = '{
     "id": "gpugeek-seedance-2-0-fast",
     "name": "Doubao Seedance 2.0 Fast · 文生视频（快）",
     "vendor": "GPUGeek",
     "modality": "video",
     "enabled": true,
     "order": 6,
     "description": "轻量文生视频模型，出片更快、更便宜；可选传首帧 / 尾帧图",
     "inputs": [
       {
         "key": "first_frame",
         "label": "首帧",
         "kind": "image",
         "required": false,
         "min_count": 0,
         "max_count": 1,
         "accept": ["image/png", "image/jpeg", "image/webp"],
         "max_bytes": 8388608,
         "min_pixels": { "width": 300, "height": 300 },
         "hint": "传图后从该画面开始生成；小于 300×300 上游会失败"
       },
       {
         "key": "last_frame",
         "label": "尾帧",
         "kind": "image",
         "required": false,
         "min_count": 0,
         "max_count": 1,
         "accept": ["image/png", "image/jpeg", "image/webp"],
         "max_bytes": 8388608,
         "min_pixels": { "width": 300, "height": 300 },
         "requires_slot": "first_frame",
         "hint": "生成到该画面结束；上游按位置识别首尾帧，因此必须与首帧一起传"
       }
     ],
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
         "help": "快档上游只支持到 720P，要 1080P 请换主力模型",
         "options": [
           { "value": "480p", "label": "480P" },
           { "value": "720p", "label": "720P", "badge": "×2 积分" }
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
       },
       {
         "key": "seed",
         "label": "随机种子",
         "control": "seed",
         "group": "advanced",
         "order": 20,
         "default": null,
         "min": 0,
         "max": 4294967295,
         "allow_random": true,
         "help": "固定种子可复现同一次生成；留空则每次随机"
       }
     ],
     "pricing": {
       "currency": "credit",
       "base": 2,
       "modifiers": [
         { "when": { "op": "eq", "key": "resolution", "value": "720p" }, "op": "mul", "value": 2, "label": "720P 加价" }
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
       { "from": "inputs.first_frame", "to": "input.images[]", "inline": true },
       { "from": "inputs.last_frame", "to": "input.images[]", "inline": true },
       { "from": "params.duration", "to": "input.duration", "cast": "int" },
       { "from": "params.resolution", "to": "input.resolution" },
       { "from": "params.ratio", "to": "input.ratio" },
       { "from": "params.seed", "to": "input.seed", "cast": "int" }
     ]
   }',
  description = '轻量文生视频模型，出片更快、更便宜；可选传首帧 / 尾帧图'
WHERE id = 'gpugeek-seedance-2-0-fast';


-- ─────────────────────────────────────────────────────────────────────────────
-- 3 / Seedream 4.5 —— size 的真实取值域
--
-- 000007 只给了 5 个写死的 2K 档，注释说「档位串在这里没有可用的取值域」。
-- 那条结论半对：'1k' 确实被拒，但 '2k' / '4k' 是收的。逐条实测（2026-08-10）：
--
--   1k            → the specified size is not supported for model doubao-seedream-4-5
--   2k            → 200，ffprobe 产物 2048×2048
--   4k            → 200，ffprobe 产物 4096×4096
--   3000x1230     → 200，ffprobe 产物 3000×1230（任意宽高确实按位生效）
--   512x512 / 1024x1024 / 1440x1440 / 1919x1920
--                 → image size must be at least 3686400 pixels
--   8192x8192 / 16384x16384
--                 → image size must be at most 16777216 pixels
--   3686400x1 / 8192x450
--                 → aspect ratio must be between 1/16 and 16
--   0x0 / -1x-1   → invalid width
--   2048 / 2048*2048
--                 → size must be one of 'WIDTHxHEIGHT', '1k', '2k', or '4k'
--
-- 所以真实取值域是一个**连续区间**：任意 WIDTHxHEIGHT，满足
--   3686400 ≤ 宽×高 ≤ 16777216 且 1/16 ≤ 宽/高 ≤ 16
--
-- ── 为什么不把它做成两个数字输入框 ──────────────────────────────────────
-- 控件是 aspect_select，它的语义是「选一个比例」，前端据此画比例框；
-- 连续区间要表达就得换成 compound(两个 stepper) + 一条乘积约束，而 ParamSpec
-- 里没有跨字段约束这种东西，加它等于为一个模型改契约。取值域是连续的这件事
-- 写进 help 让用户知道，选项则给覆盖两个像素档、各比例的常用点位。
--
-- 新增的 6 档全部落在实测区间内（4K 档 = 2K 档宽高各 ×2，比例不变）：
--   4096x4096 = 16777216（上限，实测通过）   4096x2304 = 9437184
--   2304x4096 同上转置                        4096x3072 = 12582912
--   3072x4096 同上转置
-- 2K 档五个维持原样，默认值也仍是 2048x2048——默认值该服务于「便宜地试一次」，
-- 4K 出图更慢（实测 17.6s vs 10.2s）。
-- ─────────────────────────────────────────────────────────────────────────────
UPDATE models SET
  capability = '{
     "id": "gpugeek-seedream-4-5",
     "name": "Doubao Seedream 4.5 · 文生图",
     "vendor": "GPUGeek",
     "modality": "image",
     "enabled": true,
     "order": 5,
     "description": "真实文生图模型，一次调用约 10 秒出一张 2K~4K 的图",
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
         "help": "上游收任意 宽x高，只要总像素在 3686400 与 16777216 之间、宽高比在 1/16 与 16 之间；下面是各比例的常用档位",
         "options": [
           { "value": "2048x2048", "label": "1:1", "ratio_w": 1, "ratio_h": 1, "pixels": { "width": 2048, "height": 2048 } },
           { "value": "2560x1440", "label": "16:9", "ratio_w": 16, "ratio_h": 9, "pixels": { "width": 2560, "height": 1440 } },
           { "value": "1440x2560", "label": "9:16", "ratio_w": 9, "ratio_h": 16, "pixels": { "width": 1440, "height": 2560 } },
           { "value": "2304x1728", "label": "4:3", "ratio_w": 4, "ratio_h": 3, "pixels": { "width": 2304, "height": 1728 } },
           { "value": "1728x2304", "label": "3:4", "ratio_w": 3, "ratio_h": 4, "pixels": { "width": 1728, "height": 2304 } },
           { "value": "4096x4096", "label": "1:1", "badge": "4K", "ratio_w": 1, "ratio_h": 1, "pixels": { "width": 4096, "height": 4096 } },
           { "value": "4096x2304", "label": "16:9", "badge": "4K", "ratio_w": 16, "ratio_h": 9, "pixels": { "width": 4096, "height": 2304 } },
           { "value": "2304x4096", "label": "9:16", "badge": "4K", "ratio_w": 9, "ratio_h": 16, "pixels": { "width": 2304, "height": 4096 } },
           { "value": "4096x3072", "label": "4:3", "badge": "4K", "ratio_w": 4, "ratio_h": 3, "pixels": { "width": 4096, "height": 3072 } },
           { "value": "3072x4096", "label": "3:4", "badge": "4K", "ratio_w": 3, "ratio_h": 4, "pixels": { "width": 3072, "height": 4096 } },
           { "value": "4k", "label": "4K 方图", "badge": "简写", "ratio_w": 1, "ratio_h": 1, "pixels": { "width": 4096, "height": 4096 } }
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
  description = '真实文生图模型，一次调用约 10 秒出一张 2K~4K 的图'
WHERE id = 'gpugeek-seedream-4-5';


-- ─────────────────────────────────────────────────────────────────────────────
-- 4 / mock-image-1 —— 补两条漏掉的映射规则
--
-- 这一条不是探上游探出来的，是本轮新加的 TestSeededModelsParamCoverage
-- （见仓库根 catalog_test.go）跑出来的第一条真错：
--
--   mock-image-1: 参数 style_model 没有对应的映射规则，它的取值到不了上游
--   mock-image-1: 参数 strength   没有对应的映射规则，它的取值到不了上游
--
-- 两个参数从 000002 播下那天起就只进了 capability，没进 request_mapping。
-- 后果是静默的：控件照渲染，style_model 选 film_a24 还照样 +2 积分
-- （它在 pricing.modifiers 里），只是这个选择永远不会出现在请求体里。
-- strength 更直接——「重绘强度」这个滑杆从来没有连到任何东西上。
--
-- mock 驱动确实不读渲染出来的请求体（它直接看 in.Params 画图），所以这两条
-- 规则对 mock 的行为没有影响。补它仍然是对的：这份配置是「一个模型该怎么配」
-- 的活样板，样板里带着一处参数接不上的错，比 mock 少画一笔贵得多。
-- ─────────────────────────────────────────────────────────────────────────────
UPDATE models SET
  request_mapping = '{
     "body_template": { "model": "mock-image-1" },
     "rules": [
       { "from": "prompt", "to": "prompt" },
       { "from": "params.style_model", "to": "style_model" },
       { "from": "params.aspect", "to": "size" },
       { "from": "params.count", "to": "n", "cast": "int" },
       { "from": "params.strength", "to": "strength", "cast": "float" },
       { "from": "params.seed", "to": "seed", "cast": "int" },
       { "from": "params.negative_prompt", "to": "negative_prompt" },
       { "from": "inputs.reference_images", "to": "image[]", "wrap": "image_url_part" }
     ]
   }'
WHERE id = 'mock-image-1';


-- 模型目录变了就要推版本号，让在跑的实例刷新缓存并换掉 ETag。
INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'models: seedance first/last frame slots + seed, seedance-fast drop 1080p, seedream size domain');
