// Package compose is the L0 driver for local video concatenation:
// it stitches an ordered set of already-produced video assets into one MP4.
//
// # 为什么拼接发生在本地而不是某个上游
//
// 现有的任何上游都没有声明「多段视频输入 → 一条视频输出」的槽位——它们是
// 生成模型，不是剪辑服务。而拼接本身不需要模型：把 N 段编码参数一致的视频
// 首尾相接，是一次容器层面的重写，ffmpeg 的 concat demuxer 加 `-c copy`
// 连重编码都不做，几百毫秒就完事。为这件事去接一个转码 SaaS，等于为了
// 一次文件拼接引入一个新的供应商、新的凭证、新的失败模式。
//
// # 为什么仍然做成一个 driver 而不是在 handler 里就地拼
//
// 因为要的是一条**真任务**：任务表、queued→running→succeeded 状态机、
// 失败退款、SSE 推送、任务监控里能看到，一样不能少。而进入那条链路的
// 唯一入口就是驱动注册表。
//
// # ffmpeg 缺席时报错，不降级
//
// 本包早期版本在没有 ffmpeg 的前提下退而求其次，产出一份「有序片段清单」
// 的 JSON 当作合成结果。那个降级最终被判定为有害：任务是 succeeded、卡片
// 上有产物、监控全绿，用户下载下来却是一份 JSON。**绿灯下的坏产物比红灯
// 难发现得多。** 现在 ffmpeg 不在就直接失败，说清楚是环境缺东西——
// 失败会退款，用户知道自己没拿到片子。
package compose

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Name 是 Registry 的查找键，同时也是 models.protocol_family 的取值。
const Name = string(domain.FamilyCompose)

// Family 是本驱动的协议族。
const Family = domain.FamilyCompose

// SlotSegments 是片段输入槽的 key。
//
// 它同时是三处约定的交汇点：models.capability 里声明的输入槽、httpapi 提交
// 任务时填的 inputs、以及 executor 记血缘时识别 composed_from 边的依据。
// 取名与 domain.LineageComposedFrom 一致不是巧合——这个槽的语义就是那条边。
const SlotSegments = string(domain.LineageComposedFrom)

// OutputMIME 是成片的媒体类型。它决定资产落盘的扩展名与 assets.type
// （video/mp4 归到 video），前端据此挂 <video> 而不是别的什么。
const OutputMIME = "video/mp4"

// concatTimeout 是一次拼接的时间上限。
//
// `-c copy` 的路径是纯 IO，十几段也是秒级；真走到重编码时，一段 4 秒 480p
// 在 veryfast 下大约 1 秒，十二段留足余量。超过这个量级基本可以断定 ffmpeg
// 卡在一个坏文件上了，那时候继续等只是让用户多盯十分钟同一个转圈。
const concatTimeout = 10 * time.Minute

// probeTimeout 是读容器元信息的时间上限。ffprobe 只读文件头，很轻。
const probeTimeout = 15 * time.Second

// maxOutputBytes 是成片体积的上限。
//
// 同步族的产物只能内联回 L2（Inline + KindBase64），也就是必须整个进内存。
// 上限按 512MB 给：本平台可选的最长片段是 15 秒 1080p，槽位最多 12 段，
// 正常成片在几十 MB 量级，真撞上这个数说明输入不对劲。宁可报错，
// 也不能让一次合成把进程的内存打穿——那会连累同一进程里所有别的任务。
const maxOutputBytes = 512 << 20

// durationTolerance 是「拼接结果的时长 ≈ 各段时长之和」的判定容差。
//
// 存在的理由见 concat 的注释：`-c copy` 在各段编码参数不一致时**不报错**，
// 它会产出一个时间戳错乱、播放器只放得出第一段的文件。时长对不上是这种
// 静默损坏最可靠的信号。0.5 秒的容差覆盖容器时基取整的正常偏差。
const durationTolerance = 0.5

// Driver 是本驱动的配置。
type Driver struct {
	// FFmpegPath 指定 ffmpeg 可执行文件；为空时从 PATH 查找。
	FFmpegPath string
	// FFprobePath 指定 ffprobe；为空时先看 ffmpeg 的同目录，再查 PATH。
	FFprobePath string
}

type driver struct {
	ffmpeg  string
	ffprobe string
}

// New 返回一个本地合成驱动。
//
// ffmpeg 的探测在构造时做一次而不是每次合成现查：exec.LookPath 会走一遍
// PATH 上的每个目录，而进程运行期间 PATH 不变，探测一次就是最终答案。
// 探测不到**不在这里报错**——注册驱动发生在启动期，为一个可能整天没人用的
// 功能拒绝启动整个服务不成比例。真正提交合成任务时才失败，见 Invoke。
func New(cfg Driver) adapter.SyncDriver {
	d := driver{ffmpeg: cfg.FFmpegPath, ffprobe: cfg.FFprobePath}
	if d.ffmpeg == "" {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			d.ffmpeg = p
		}
	}
	if d.ffprobe == "" {
		d.ffprobe = lookProbe(d.ffmpeg)
	}
	return d
}

// lookProbe 找 ffprobe：先看 ffmpeg 的同目录，再查 PATH。
//
// 同目录优先是因为这两个可执行文件同属一个 ffmpeg 发行版，版本必须配套；
// 一台机器上装了两份 ffmpeg 时，先查 PATH 会把 A 的 ffmpeg 和 B 的 ffprobe
// 配到一起。
func lookProbe(ffmpeg string) string {
	if ffmpeg != "" {
		sibling := filepath.Join(filepath.Dir(ffmpeg), "ffprobe")
		if info, err := os.Stat(sibling); err == nil && !info.IsDir() {
			return sibling
		}
	}
	if p, err := exec.LookPath("ffprobe"); err == nil {
		return p
	}
	return ""
}

func (d driver) Name() string                  { return Name }
func (d driver) Family() domain.ProtocolFamily { return Family }

// Invoke 把输入槽里的片段按顺序拼成一条 MP4 并直接返回。
//
// 同步而不是异步：拼接是本地进程里的一次 exec，没有"等上游"这件事。
// 它仍然是一条真任务——L2 走的是与 chat / images 完全相同的同步收尾
// （转存 → 派生 → 血缘 → 结算 → SSE）。
//
// 片段少于 2 段直接报参数错误：一段的"合成"没有意义，而让它成功会产出
// 一条和输入一模一样的视频，用户会以为拼接生效了。
func (d driver) Invoke(ctx context.Context, in adapter.SubmitInput) (adapter.SubmitResult, error) {
	if err := ctx.Err(); err != nil {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, err, "合成已取消")
	}

	segments := make([]adapter.InputRef, 0, len(in.Inputs))
	for _, ref := range in.Inputs {
		if ref.Slot == SlotSegments {
			segments = append(segments, ref)
		}
	}
	if len(segments) < 2 {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInvalidParam, nil,
			"合成至少需要 2 个片段，收到 %d 个", len(segments))
	}
	if d.ffmpeg == "" {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"服务器上没有 ffmpeg，无法拼接视频；请在运行 aigcd 的机器上安装 ffmpeg 后重试")
	}
	if in.Blobs == nil {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, nil,
			"合成需要读取片段原始字节，但没有注入 BlobReader")
	}

	work, err := os.MkdirTemp("", "aigc-compose-*")
	if err != nil {
		return adapter.SubmitResult{}, adapter.CallError(domain.TaskErrorInternal, err, "创建合成工作目录失败")
	}
	defer func() { _ = os.RemoveAll(work) }()

	paths, wantDuration, err := d.spillSegments(ctx, work, in.Blobs, segments)
	if err != nil {
		return adapter.SubmitResult{}, err
	}

	out := filepath.Join(work, "output.mp4")
	if err := d.concat(ctx, work, paths, out, wantDuration); err != nil {
		return adapter.SubmitResult{}, err
	}

	body, meta, err := d.readOutput(ctx, out)
	if err != nil {
		return adapter.SubmitResult{}, err
	}

	return adapter.SubmitResult{
		Status:    adapter.StatusSucceeded,
		RawStatus: "succeeded",
		Inline: []adapter.ArtifactRef{{
			Kind:       adapter.KindBase64,
			Type:       domain.AssetTypeVideo,
			MIME:       OutputMIME,
			Base64:     base64.StdEncoding.EncodeToString(body),
			Bytes:      int64(len(body)),
			Width:      meta.width(),
			Height:     meta.height(),
			DurationMS: meta.durationMS(),
		}},
	}, nil
}

// spillSegments 把每段的字节落到工作目录，顺带累计期望总时长。
//
// 落临时文件而不是管道喂给 ffmpeg：concat demuxer 要按名字引用一串输入，
// 而且 mp4 的 moov box 常在文件末尾，可 seek 是硬要求。
//
// 文件名带序号前缀，顺序就是槽位里给的顺序——**这个顺序就是成片的剪辑顺序**，
// 由 httpapi 按用户勾选卡片的次序排好，本包原样照做，不重排。
func (d driver) spillSegments(ctx context.Context, dir string, blobs adapter.BlobReader, segs []adapter.InputRef) ([]string, float64, error) {
	paths := make([]string, 0, len(segs))
	var total float64

	for i, seg := range segs {
		if seg.StorageKey == "" {
			return nil, 0, adapter.CallError(domain.TaskErrorInvalidParam, nil,
				"第 %d 段没有存储键，无法参与拼接", i+1)
		}
		raw, err := blobs.ReadBlob(ctx, seg.StorageKey)
		if err != nil {
			return nil, 0, adapter.CallError(domain.TaskErrorInternal, err, "读取第 %d 段的原始字节失败", i+1)
		}

		path := filepath.Join(dir, fmt.Sprintf("seg-%03d%s", i, segmentExt(seg)))
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			return nil, 0, adapter.CallError(domain.TaskErrorInternal, err, "落地第 %d 段失败", i+1)
		}
		paths = append(paths, path)

		// 时长探测失败不阻断拼接：它只用来事后校验拼接是不是真的拼上了，
		// 探不到就退回"不校验"，总比因为一次 ffprobe 抽风让成片作废好。
		if m, err := d.probe(ctx, path); err == nil && m.Duration > 0 {
			total += m.Duration
		}
	}
	return paths, total, nil
}

// segmentExt 猜一个扩展名给临时文件。
//
// ffmpeg 主要靠探测内容而不是扩展名，这里给对了只是让 concat 列表更好读、
// 排查时能直接把某一段拖进播放器。猜不出来就统一 .mp4——本平台的视频产物
// 目前只有 mp4。
func segmentExt(seg adapter.InputRef) string {
	if ext := filepath.Ext(seg.StorageKey); ext != "" && len(ext) <= 5 {
		return ext
	}
	switch strings.ToLower(strings.TrimSpace(seg.MIME)) {
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	default:
		return ".mp4"
	}
}

// concat 把若干段拼成一条，优先零转码。
//
// # 两轮策略
//
// 第一轮走 concat demuxer + `-c copy`：各段来自同一个模型、同一组参数时
// 编码参数天然一致，这一轮不解码任何一帧，十二段也就几百毫秒。
//
// 但 `-c copy` 在参数不一致时**不会报错**——它照样吐出一个文件，只是时间戳
// 错乱，播放器多半只放得出第一段。因此第一轮之后要拿 ffprobe 量一下成片时长
// 对不对得上各段之和；对不上就当第一轮没发生过，走第二轮重编码。
// 只看 ffmpeg 的退出码会让这种静默损坏一路走到用户的下载目录里。
//
// 第二轮用 concat filter 重编码：它先把每段解码成帧再重新编一遍，
// 分辨率/帧率/像素格式不一致都由 filter 图统一掉。代价是真的要跑编码器，
// 所以它是回退而不是默认。
func (d driver) concat(ctx context.Context, dir string, paths []string, out string, wantDuration float64) error {
	list := filepath.Join(dir, "concat.txt")
	var b strings.Builder
	for _, p := range paths {
		// concat 列表的路径要转义单引号。这些路径是本包自己造的（seg-000.mp4），
		// 不含引号；转义留着是因为"输入永远由我构造"这个前提一旦被改动打破，
		// 出的就是一条能被摆布的 ffmpeg 指令。
		b.WriteString("file '" + strings.ReplaceAll(p, "'", `'\''`) + "'\n")
	}
	if err := os.WriteFile(list, []byte(b.String()), 0o600); err != nil {
		return adapter.CallError(domain.TaskErrorInternal, err, "写入拼接清单失败")
	}

	copyErr := d.run(ctx, concatTimeout,
		"-nostdin", "-loglevel", "error", "-y",
		"-f", "concat", "-safe", "0", "-i", list,
		"-c", "copy",
		"-movflags", "+faststart",
		out)
	if copyErr == nil && d.outputLooksWhole(ctx, out, wantDuration) {
		return nil
	}

	args := []string{"-nostdin", "-loglevel", "error", "-y"}
	for _, p := range paths {
		args = append(args, "-i", p)
	}
	// 每段都取 v:0 与 a:0 送进 concat filter。a=1 要求每段都有音轨，
	// 而文生视频的产物未必有——因此先探一遍，只要有一段没音轨就整片按无声拼，
	// 让 filter 图的形状始终自洽（缺一路输入的 concat filter 会直接报错）。
	audio := d.everySegmentHasAudio(ctx, paths)
	var fg strings.Builder
	for i := range paths {
		fg.WriteString(fmt.Sprintf("[%d:v:0]", i))
		if audio {
			fg.WriteString(fmt.Sprintf("[%d:a:0]", i))
		}
	}
	fg.WriteString(fmt.Sprintf("concat=n=%d:v=1:a=%d[v]", len(paths), boolToInt(audio)))
	if audio {
		fg.WriteString("[a]")
	}

	args = append(args, "-filter_complex", fg.String(), "-map", "[v]")
	if audio {
		args = append(args, "-map", "[a]", "-c:a", "aac", "-b:a", "128k")
	}
	args = append(args,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p",
		"-movflags", "+faststart",
		out)

	if err := d.run(ctx, concatTimeout, args...); err != nil {
		// 两轮都失败时把第一轮的错也带上：`-c copy` 的报错往往更直白
		// （"Non-monotonous DTS"、"could not find codec parameters"），
		// 只报重编码那一轮会丢掉判断输入到底哪里不对的线索。
		if copyErr != nil {
			return adapter.CallError(domain.TaskErrorInternal, err,
				"拼接失败：零转码与重编码都没能完成（零转码：%v；重编码：%v）", copyErr, err)
		}
		return adapter.CallError(domain.TaskErrorInternal, err, "拼接失败：重编码没能完成（%v）", err)
	}
	return nil
}

// outputLooksWhole 判断零转码那一轮是不是真的把每一段都拼进去了。
//
// wantDuration 为 0 表示各段时长根本没探出来，此时无从校验——放行，
// 让重编码那一轮的成本只花在有证据的失败上。
func (d driver) outputLooksWhole(ctx context.Context, out string, wantDuration float64) bool {
	if wantDuration <= 0 {
		return true
	}
	m, err := d.probe(ctx, out)
	if err != nil || m.Duration <= 0 {
		return false
	}
	return math.Abs(m.Duration-wantDuration) <= durationTolerance
}

// everySegmentHasAudio 报告是不是每一段都带音轨。
func (d driver) everySegmentHasAudio(ctx context.Context, paths []string) bool {
	if d.ffprobe == "" {
		return false
	}
	for _, p := range paths {
		m, err := d.probe(ctx, p)
		if err != nil || !m.HasAudio {
			return false
		}
	}
	return true
}

// readOutput 读回成片并量一遍它的宽高时长。
//
// 宽高时长在这里就取，不留给 L3 的 deriver 事后补：deriver 要为此把成片
// 再落一次临时文件（它只拿得到存储键），而这里手上正好有文件。
func (d driver) readOutput(ctx context.Context, out string) ([]byte, videoMeta, error) {
	info, err := os.Stat(out)
	if err != nil {
		return nil, videoMeta{}, adapter.CallError(domain.TaskErrorInternal, err, "拼接结果不存在")
	}
	if info.Size() == 0 {
		return nil, videoMeta{}, adapter.CallError(domain.TaskErrorInternal, nil, "拼接结果是空文件")
	}
	if info.Size() > maxOutputBytes {
		return nil, videoMeta{}, adapter.CallError(domain.TaskErrorInvalidParam, nil,
			"拼接结果 %d 字节，超过上限 %d；请减少片段数量或降低清晰度", info.Size(), int64(maxOutputBytes))
	}

	body, err := os.ReadFile(out)
	if err != nil {
		return nil, videoMeta{}, adapter.CallError(domain.TaskErrorInternal, err, "读取拼接结果失败")
	}
	meta, err := d.probe(ctx, out)
	if err != nil {
		// 量不出来不影响成片本身，宽高时长留空由 L3 事后补。
		return body, videoMeta{}, nil
	}
	return body, meta, nil
}

// videoMeta 是 ffprobe 报回来的容器元信息。
type videoMeta struct {
	Width    int
	Height   int
	Duration float64
	HasAudio bool
}

func (m videoMeta) width() *int {
	if m.Width <= 0 {
		return nil
	}
	w := m.Width
	return &w
}

func (m videoMeta) height() *int {
	if m.Height <= 0 {
		return nil
	}
	h := m.Height
	return &h
}

func (m videoMeta) durationMS() *int {
	if m.Duration <= 0 {
		return nil
	}
	ms := int(math.Round(m.Duration * 1000))
	return &ms
}

// probe 用 ffprobe 读宽高、容器时长与有无音轨。
//
// 时长取 format 而不是 stream：不少编码器不给视频流写 duration，
// 而 format 那一层几乎总是有的（mp4 里它就是 moov 的 mvhd 时长）。
func (d driver) probe(ctx context.Context, path string) (videoMeta, error) {
	if d.ffprobe == "" {
		return videoMeta{}, fmt.Errorf("compose: PATH 上没有 ffprobe")
	}
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(cctx, d.ffprobe,
		"-v", "error",
		"-show_entries", "stream=codec_type,width,height:format=duration",
		"-of", "json",
		path,
	)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return videoMeta{}, fmt.Errorf("ffprobe: %w (%s)", err, truncate(stderr.String(), 200))
	}

	var raw struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		return videoMeta{}, fmt.Errorf("解析 ffprobe 输出: %w", err)
	}

	var m videoMeta
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			if m.Width == 0 {
				m.Width, m.Height = s.Width, s.Height
			}
		case "audio":
			m.HasAudio = true
		}
	}
	// duration 是秒，带小数（"4.041667"）。N/A 与空串解不出来，当作没有。
	if secs, err := strconv.ParseFloat(raw.Format.Duration, 64); err == nil && secs > 0 {
		m.Duration = secs
	}
	return m, nil
}

// run 跑一次 ffmpeg，把 stderr 收进错误里。
func (d driver) run(ctx context.Context, timeout time.Duration, args ...string) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(cctx, d.ffmpeg, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w (%s)", err, truncate(stderr.String(), 400))
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// truncate 截断日志与错误里的外部输出，防止 ffmpeg 的长 stderr 淹掉一切。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var _ adapter.SyncDriver = driver{}
