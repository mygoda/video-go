-- =============================================================================
-- 000018_compose_voice_subtitle.up.sql
--
-- 一键合成多两个开关：**去音轨**与**软字幕轨**。它们必须在 capability 里
-- 声明出来，否则 Validator 会把带这两个 key 的提交当作未知参数整条拒掉
-- （internal/capability/validator.go：「未知的 params key 一律拒绝」）。
--
-- ── 为什么字幕正文是一个 textarea 参数，而不是驱动自己去转写 ────────────
-- 台词是用户在镜头卡里逐字写的。对成片跑语音转写去"认"回这些字，是把一件
-- 零误差的事做成有误差的事。所以时间轴与文本都由 httpapi 按卡片顺序算好，
-- 驱动只负责把它挂成一条轨——而驱动能收到的东西只有 SubmitInput 那几样，
-- prompt 是成片标题、inputs 是片段的存储键，字幕正文两头都不属于，只能走
-- params。走了 params 就得在这里声明，于是这条链路上每一个流进 ffmpeg 的值
-- 都在 L1 有据可查。
--
-- max_length 20000 是按上限估的：12 镜 × 每镜几十字，离一万都远；给到两万
-- 是让「用户把整段独白写进一镜」这种写法也塞得下，同时挡住有人拿这个字段
-- 当通用存储往里灌兆级文本。
--
-- ── 这两个参数不会出现在任何用户表单里 ──────────────────────────────────
-- local-compose-1 的 visibility 是 internal（见 000008），它不进 GET /api/models
-- 的用户目录，创作页的通用参数表单永远渲染不到它。开关的界面在画布的合成条上，
-- 由 POST /api/canvas/{id}/compose 的 mute / subtitles 两个布尔字段进来，
-- 服务端翻译成这里声明的 params。声明是**服务端自己的契约**，不是给前端看的。
--
-- pricing 不动：去一条轨、挂一条文本轨都是容器层的改写，视频包一个字节都不
-- 重编，成本与原来那次拼接没有区别。收钱说不过去。
--
-- 幂等：JSON_SET 覆盖同一条路径，可重复执行。
-- =============================================================================

UPDATE models
   SET capability = JSON_SET(
         capability,
         '$.description', '把选中的片段按顺序拼接成一条 MP4。各段编码参数一致时零转码，不一致时自动重编码。可选去掉音轨、挂一条软字幕轨。',
         '$.params', JSON_ARRAY(
           JSON_OBJECT(
             'key',     'mute',
             'label',   '去掉音轨',
             'control', 'toggle',
             'group',   'primary',
             'order',   10,
             'default', FALSE,
             'help',    '成片不带声音。人声本身在出片那一步由每镜的台词开关决定；这里是给已经出好的片子一条不必重出的静音路径。'
           ),
           JSON_OBJECT(
             'key',        'subtitle_srt',
             'label',      '字幕',
             'control',    'textarea',
             'group',      'advanced',
             'order',      20,
             'default',    '',
             'max_length', 20000,
             'help',       '一整份 SRT，由各镜的台词与时长在服务端生成，挂成 mov_text 软字幕轨。不烧进画面。'
           )
         )
       )
 WHERE id = 'local-compose-1';

INSERT INTO config_versions (changed_by, note)
VALUES (NULL, 'compose: optional mute and soft subtitle track');
