package asset

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"math"
	"strconv"

	// image.Decode 靠这两个包的 init 注册解码器。不 import 就只会得到
	// "unknown format"，而 PNG 是 AI 出图最常见的格式。
	_ "image/gif"
	_ "image/png"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
)

// thumbJPEGQuality 是缩略图的 JPEG 质量。
//
// 82 是肉眼与体积的常见折中点：再高体积涨得快而画布上 512px 的缩略图看不出差别，
// 再低会在渐变区出现色带，而 AI 产物里渐变（天空、背景虚化）非常多。
const thumbJPEGQuality = 82

// posterTimeout 是抽帧的时间上限。
//
// 抽首帧只需读文件开头，正常是毫秒级；超过这个量级基本可以断定是
// ffmpeg 卡在一个坏文件上了。派生允许失败，因此这里宁可早点放弃，
// 也不能让它拖住任务的收尾。
const posterTimeout = 30 * time.Second

// probeTimeout 是读容器元信息的时间上限。ffprobe 只读文件头，比抽帧还轻。
const probeTimeout = 15 * time.Second

// maxDecodePixels 是解码缩略图源图的像素上限。
//
// 没有它，一张声称 50000x50000 的 PNG 会在解码时申请几十 GB 内存把进程打死
// （解压炸弹）。上限按 1 亿像素给，正常 AI 出图（最大约 4K×4K = 1600 万）
// 远在其下。
const maxDecodePixels = 100_000_000

// DeriverDeps 是 Deriver 的依赖。
type DeriverDeps struct {
	// Blobs 读原始对象、写派生对象。
	Blobs Store
	// Assets 用于把派生结果写回 assets 行（SetVariants）。
	// 为 nil 时 Derive 只返回结果不落库，供调用方自行安排落库时机。
	Assets store.AssetRepo
	// FFmpegPath 指定 ffmpeg 可执行文件；为空时从 PATH 查找。
	// 找不到时视频不生成 poster——这是明确的降级路径，不是错误。
	FFmpegPath string
	// FFprobePath 指定 ffprobe 可执行文件；为空时先看 ffmpeg 的同目录，再查 PATH。
	// 找不到时视频的宽高与时长留空，同样是降级而不是错误。
	FFprobePath string
	Logger      *slog.Logger
	Now         func() time.Time
}

// deriver 是 Deriver 的默认实现。
type deriver struct {
	blobs   Store
	assets  store.AssetRepo
	ffmpeg  string
	ffprobe string
	log     *slog.Logger
	now     func() time.Time
}

// NewDeriver 组装一个派生器。
//
// ffmpeg 的探测在构造时做一次而不是每次抽帧现查：exec.LookPath 会走一遍 PATH
// 上的每个目录，放在热路径上是白花的 syscall。进程运行期间 PATH 不变，
// 探测一次的结果就是最终答案。
func NewDeriver(d DeriverDeps) (Deriver, error) {
	if d.Blobs == nil {
		return nil, fmt.Errorf("asset: DeriverDeps.Blobs 不能为空")
	}
	dv := &deriver{
		blobs:   d.Blobs,
		assets:  d.Assets,
		ffmpeg:  d.FFmpegPath,
		ffprobe: d.FFprobePath,
		log:     d.Logger,
		now:     d.Now,
	}
	if dv.log == nil {
		dv.log = slog.Default()
	}
	if dv.now == nil {
		dv.now = time.Now
	}
	if dv.ffmpeg == "" {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			dv.ffmpeg = p
		} else {
			dv.log.Info("PATH 上没有 ffmpeg，视频将不生成封面帧")
		}
	}
	if dv.ffprobe == "" {
		dv.ffprobe = lookProbe(dv.ffmpeg)
		if dv.ffprobe == "" {
			dv.log.Info("PATH 上没有 ffprobe，视频的宽高与时长将留空")
		}
	}
	return dv, nil
}

// lookProbe 找 ffprobe：先看 ffmpeg 的同目录，再查 PATH。
//
// 同目录优先是因为这两个可执行文件同属一个 ffmpeg 发行版，版本必须配套；
// 而一台机器上装了两份 ffmpeg（brew 一份、手动解压一份）时，先查 PATH
// 会把 A 的 ffmpeg 和 B 的 ffprobe 配到一起。
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

// Derive 为一件资产生成全部适用的派生规格，并把结果写回 assets 行。
//
// **它永不因为派生失败而返回错误。** 派生是任务成功的收尾动作，而原始字节
// 此刻已经安全落地了；为了一张缩略图把任务判失败要退款、要让用户重跑一次
// 真实生成，代价完全不对等。失败只记日志，前端回退到原图。
//
// 视频顺带补上宽高与时长：这三项上游多半不报（GPUGeek 就不报），而它们
// 只有解开容器才知道——而解容器正是抽封面帧那一步已经在做的事。不补的话，
// 资产库里每条视频都是「▶ 0s」，瀑布流也拿不到宽高比只能退回统一行高。
//
// 入参是指针：补出来的宽高时长要回填给调用方。紧随其后的 task.succeeded
// 事件按这个结构体投影，不回填的话事件里没有尺寸、REST 拉同一条却有。
//
// 返回的 error 只可能来自落库——那说明数据库出了问题，
// 调用方需要知道，但它同样不该让任务失败（调用方按注释里的约定吞掉它）。
func (d *deriver) Derive(ctx context.Context, a *domain.Asset) (map[Variant]string, error) {
	out := make(map[Variant]string, 2)

	var posterKey *string
	var probed bool
	if a.Type == domain.AssetTypeVideo && (d.ffmpeg != "" || d.ffprobe != "") {
		// ffmpeg 与 ffprobe 都要一个可 seek 的输入（管道喂 mp4 常常读不到
		// moov box），而它们读的是同一个文件——落一次临时文件给两个人用。
		tmp, err := d.spillToTemp(ctx, a.StorageKey)
		if err != nil {
			d.log.Warn("落地视频临时文件失败，跳过封面帧与元信息", "asset_id", a.ID, "err", err)
		} else {
			defer func() { _ = os.Remove(tmp) }()
			probed = d.fillVideoMeta(ctx, a, tmp)

			key, err := d.posterFrom(ctx, *a, tmp)
			switch {
			case err == nil && key != "":
				out[VariantPoster] = key
				k := key
				posterKey = &k
			case errors.Is(err, ErrUnsupported):
				// ffmpeg 不在，属于已知降级，不必每条任务都吼一遍。
				d.log.Debug("跳过封面帧", "asset_id", a.ID, "reason", err)
			case err != nil:
				d.log.Warn("生成封面帧失败，资产仍然有效", "asset_id", a.ID, "err", err)
			}
		}
	}

	// 视频的缩略图从封面帧缩，而不是再抽一次帧：抽帧是这两步里唯一昂贵的部分。
	thumbSrc := *a
	if posterKey != nil {
		thumbSrc.StorageKey = *posterKey
		thumbSrc.MIME = "image/jpeg"
	}

	var thumbKey *string
	if key, err := d.thumbnailFrom(ctx, *a, thumbSrc); err == nil && key != "" {
		out[VariantThumb512] = key
		k := key
		thumbKey = &k
	} else if err != nil && !errors.Is(err, ErrUnsupported) {
		d.log.Warn("生成缩略图失败，资产仍然有效", "asset_id", a.ID, "err", err)
	}

	if d.assets == nil {
		return out, nil
	}
	if probed {
		if err := d.assets.SetMedia(ctx, a.ID, a.Width, a.Height, a.DurationMS); err != nil {
			return out, fmt.Errorf("asset: 落库资产 %s 的宽高时长: %w", a.ID, err)
		}
	}
	if thumbKey == nil && posterKey == nil {
		return out, nil
	}
	if err := d.assets.SetVariants(ctx, a.ID, thumbKey, posterKey); err != nil {
		return out, fmt.Errorf("asset: 落库资产 %s 的派生规格: %w", a.ID, err)
	}
	return out, nil
}

// FillMeta 为一件已经落库的资产补上缺失的宽高与时长，并写回 assets 行。
//
// 它是 Derive 的补课通道：Derive 只在生成的当下跑一次，而库里躺着的历史资产
// 是在这条路铺好之前落的。图片走字节解码、视频走 ffprobe，判据与 Derive 完全
// 一致——两条路分叉的话，回填出来的数据会和新产物对不上。
func (d *deriver) FillMeta(ctx context.Context, a *domain.Asset) (bool, error) {
	var changed bool
	switch a.Type {
	case domain.AssetTypeImage:
		changed = fillImageSize(ctx, d.blobs, d.log, a)
	case domain.AssetTypeVideo:
		if d.ffprobe == "" {
			return false, fmt.Errorf("%w: PATH 上没有 ffprobe", ErrUnsupported)
		}
		tmp, err := d.spillToTemp(ctx, a.StorageKey)
		if err != nil {
			return false, err
		}
		defer func() { _ = os.Remove(tmp) }()
		changed = d.fillVideoMeta(ctx, a, tmp)
	default:
		return false, fmt.Errorf("%w: %s 没有宽高时长", ErrUnsupported, a.Type)
	}

	if !changed || d.assets == nil {
		return changed, nil
	}
	if err := d.assets.SetMedia(ctx, a.ID, a.Width, a.Height, a.DurationMS); err != nil {
		return false, fmt.Errorf("asset: 落库资产 %s 的宽高时长: %w", a.ID, err)
	}
	return true, nil
}

// fillVideoMeta 把容器里的宽高与时长补进资产，返回是否补上了新东西。
//
// 逐项补而不是整体替换：上游报了宽高但没报时长（或反过来）是常见情形，
// 把上游给过的值覆盖掉没有好处——它比我们从文件里读的更接近"上游眼中的产物"。
//
// 探测失败只记日志：这三项是排版用的，不值得让一次成功的生成挂掉。
func (d *deriver) fillVideoMeta(ctx context.Context, a *domain.Asset, path string) bool {
	if d.ffprobe == "" || (a.Width != nil && a.Height != nil && a.DurationMS != nil) {
		return false
	}
	m, err := d.probe(ctx, path)
	if err != nil {
		d.log.Warn("读不出视频元信息，宽高时长留空", "asset_id", a.ID, "err", err)
		return false
	}

	changed := false
	if a.Width == nil && m.Width > 0 {
		w := m.Width
		a.Width, changed = &w, true
	}
	if a.Height == nil && m.Height > 0 {
		h := m.Height
		a.Height, changed = &h, true
	}
	if a.DurationMS == nil && m.DurationMS > 0 {
		ms := m.DurationMS
		a.DurationMS, changed = &ms, true
	}
	return changed
}

// videoMeta 是 ffprobe 报回来的容器元信息。
type videoMeta struct {
	Width      int
	Height     int
	DurationMS int
}

// probe 用 ffprobe 读第一条视频流的宽高与容器时长。
//
// 时长取 format 而不是 stream：不少编码器不给视频流写 duration，
// 而 format 那一层几乎总是有的（它是 moov box 里的 mvhd 时长）。
func (d *deriver) probe(ctx context.Context, path string) (videoMeta, error) {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(cctx, d.ffprobe,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height:format=duration",
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
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		return videoMeta{}, fmt.Errorf("解析 ffprobe 输出: %w", err)
	}

	var m videoMeta
	if len(raw.Streams) > 0 {
		m.Width, m.Height = raw.Streams[0].Width, raw.Streams[0].Height
	}
	// duration 是秒，带小数（"4.041667"）。N/A 与空串解不出来，当作没有。
	if secs, err := strconv.ParseFloat(raw.Format.Duration, 64); err == nil && secs > 0 {
		m.DurationMS = int(math.Round(secs * 1000))
	}
	return m, nil
}

// Thumbnail 生成长边 Thumb512MaxEdge 的缩略图并返回其存储键。
func (d *deriver) Thumbnail(ctx context.Context, a domain.Asset) (string, error) {
	return d.thumbnailFrom(ctx, a, a)
}

// thumbnailFrom 从 src 的字节缩出 a 的缩略图。
//
// 分出 src 是为了让视频复用刚抽好的封面帧：src 决定读哪个对象，
// a 决定缩略图挂在哪件资产名下。
func (d *deriver) thumbnailFrom(ctx context.Context, a, src domain.Asset) (string, error) {
	if src.StorageKey == "" {
		return "", fmt.Errorf("asset: 资产 %s 没有存储键", a.ID)
	}
	if !decodableImage(src.MIME) {
		// 只用标准库，因此 webp / avif 这些解不了。
		return "", fmt.Errorf("%w: 无法解码 %q 生成缩略图", ErrUnsupported, src.MIME)
	}

	rc, info, err := d.blobs.Get(ctx, src.StorageKey)
	if err != nil {
		return "", fmt.Errorf("asset: 读取资产 %s 的原始对象: %w", a.ID, err)
	}
	defer func() { _ = rc.Close() }()

	img, err := decodeLimited(rc, info.Bytes)
	if err != nil {
		return "", fmt.Errorf("asset: 解码资产 %s: %w", a.ID, err)
	}

	thumb := resizeMaxEdge(img, Thumb512MaxEdge)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: thumbJPEGQuality}); err != nil {
		return "", fmt.Errorf("asset: 编码资产 %s 的缩略图: %w", a.ID, err)
	}

	key := BuildAssetKey(a.UserID, a.ID, VariantThumb512, "image/jpeg", assetTime(a, d.now))
	if _, err := d.blobs.Put(ctx, key, &buf, "image/jpeg"); err != nil {
		return "", fmt.Errorf("asset: 写入资产 %s 的缩略图: %w", a.ID, err)
	}
	return key, nil
}

// Poster 抽取视频首帧作为封面。
//
// 走 ffmpeg 而不是纯 Go 解码：标准库没有任何视频容器的解析能力，
// 而引入一个第三方解码器只为了取一帧完全不成比例。ffmpeg 不在 PATH 上时
// 返回 ErrUnsupported，调用方据此静默降级——视频卡片没有封面只是挂载前
// 会白一下，不是故障。
func (d *deriver) Poster(ctx context.Context, a domain.Asset) (string, error) {
	if a.Type != domain.AssetTypeVideo {
		return "", fmt.Errorf("%w: 只有视频需要封面帧", ErrUnsupported)
	}
	if d.ffmpeg == "" {
		return "", fmt.Errorf("%w: PATH 上没有 ffmpeg", ErrUnsupported)
	}
	if a.StorageKey == "" {
		return "", fmt.Errorf("asset: 资产 %s 没有存储键", a.ID)
	}

	// ffmpeg 要一个可 seek 的输入，管道喂 mp4 常常读不到 moov box。
	// 因此先落一个临时文件——这是本包唯一一处把整件产物写到 Store 之外的地方，
	// 而它只发生在视频上，且用完立刻删。
	tmp, err := d.spillToTemp(ctx, a.StorageKey)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp) }()

	return d.posterFrom(ctx, a, tmp)
}

// posterFrom 从一个已经落地的临时文件抽帧。
//
// 与 Poster 分开是为了让 Derive 复用同一份临时文件：那里 ffprobe 也要读它，
// 而一段视频有几百 MB，为了两个只读文件头的命令落两次盘没有道理。
func (d *deriver) posterFrom(ctx context.Context, a domain.Asset, path string) (string, error) {
	if d.ffmpeg == "" {
		return "", fmt.Errorf("%w: PATH 上没有 ffmpeg", ErrUnsupported)
	}

	cctx, cancel := context.WithTimeout(ctx, posterTimeout)
	defer cancel()

	var stderr bytes.Buffer
	var out bytes.Buffer
	cmd := exec.CommandContext(cctx, d.ffmpeg,
		"-nostdin",
		"-loglevel", "error",
		"-i", path,
		"-frames:v", "1",
		"-an",
		"-f", "image2",
		"-vcodec", "mjpeg",
		"-q:v", "3",
		"pipe:1",
	)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("asset: 抽取资产 %s 的封面帧: %w (%s)", a.ID, err, truncate(stderr.String(), 200))
	}
	if out.Len() == 0 {
		return "", fmt.Errorf("asset: 抽取资产 %s 的封面帧得到空结果", a.ID)
	}

	key := BuildAssetKey(a.UserID, a.ID, VariantPoster, "image/jpeg", assetTime(a, d.now))
	if _, err := d.blobs.Put(ctx, key, &out, "image/jpeg"); err != nil {
		return "", fmt.Errorf("asset: 写入资产 %s 的封面帧: %w", a.ID, err)
	}
	return key, nil
}

// spillToTemp 把一个存储对象落到本地临时文件，返回其路径。
func (d *deriver) spillToTemp(ctx context.Context, key string) (string, error) {
	rc, _, err := d.blobs.Get(ctx, key)
	if err != nil {
		return "", fmt.Errorf("asset: 读取对象 %q: %w", key, err)
	}
	defer func() { _ = rc.Close() }()

	f, err := os.CreateTemp("", "aigc-poster-*"+filepath.Ext(key))
	if err != nil {
		return "", fmt.Errorf("asset: 创建临时文件: %w", err)
	}
	name := f.Name()
	_, copyErr := io.Copy(f, rc)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("asset: 落地临时文件 %q: %w", name, errors.Join(copyErr, closeErr))
	}
	return name, nil
}

// decodableImage 报告标准库能否解码该 MIME。
//
// 只有 jpeg / png / gif 三种——没有第三方 imaging 库，webp 与 avif 解不了。
// 它们目前不是本平台的产物格式；真出现了，缩略图缺失而已，前端回退原图。
func decodableImage(mime string) bool {
	switch normalizeMIME(mime) {
	case "image/jpeg", "image/png", "image/gif":
		return true
	default:
		return false
	}
}

// decodeLimited 在读取上限内解码一张图片。
//
// 上限是 hint 的两倍再加 1MB：正常图片读完就是 hint 那么多，多给的余量
// 覆盖 Stat 与实际长度的偏差；hint 不可信时（<=0）退回一个固定上限。
// 有上限是必须的，见 maxDecodePixels 的理由。
func decodeLimited(r io.Reader, hint int64) (image.Image, error) {
	limit := hint*2 + (1 << 20)
	if hint <= 0 {
		limit = 256 << 20
	}
	lr := io.LimitReader(r, limit)

	// DecodeConfig 先看一眼尺寸再决定要不要真解码，避免解压炸弹。
	buf := &bytes.Buffer{}
	cfg, _, err := image.DecodeConfig(io.TeeReader(lr, buf))
	if err != nil {
		return nil, fmt.Errorf("读取图片头: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("图片尺寸非法: %dx%d", cfg.Width, cfg.Height)
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxDecodePixels {
		return nil, fmt.Errorf("图片像素数 %dx%d 超过上限 %d", cfg.Width, cfg.Height, maxDecodePixels)
	}

	img, _, err := image.Decode(io.MultiReader(bytes.NewReader(buf.Bytes()), lr))
	if err != nil {
		return nil, err
	}
	return img, nil
}

// resizeMaxEdge 把图片缩到长边不超过 maxEdge，等比。
//
// 用面积平均（box filter）而不是最近邻：缩略图的缩放比例往往在 1/4 以下，
// 最近邻在这个比例上会把细节抽成一堆摩尔纹，而画布上密排的缩略图正是
// 摩尔纹最刺眼的场景。box filter 只需一遍累加，代价可以接受。
//
// 图片已经小于上限时原样返回——放大缩略图没有意义，只是白占空间。
func resizeMaxEdge(src image.Image, maxEdge int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return src
	}
	if sw <= maxEdge && sh <= maxEdge {
		return src
	}

	dw, dh := sw, sh
	if sw >= sh {
		dw = maxEdge
		dh = int(float64(sh) * float64(maxEdge) / float64(sw))
	} else {
		dh = maxEdge
		dw = int(float64(sw) * float64(maxEdge) / float64(sh))
	}
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}

	// 先摊平到 RGBA，避免在内层循环里反复走 At() 的接口分派与色彩模型转换。
	flat := image.NewRGBA(image.Rect(0, 0, sw, sh))
	draw.Draw(flat, flat.Bounds(), src, b.Min, draw.Src)

	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		y0 := y * sh / dh
		y1 := (y + 1) * sh / dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < dw; x++ {
			x0 := x * sw / dw
			x1 := (x + 1) * sw / dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sr, sg, sb, sa uint32
			var n uint32
			for yy := y0; yy < y1; yy++ {
				row := yy * flat.Stride
				for xx := x0; xx < x1; xx++ {
					i := row + xx*4
					sr += uint32(flat.Pix[i])
					sg += uint32(flat.Pix[i+1])
					sb += uint32(flat.Pix[i+2])
					sa += uint32(flat.Pix[i+3])
					n++
				}
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(sr / n),
				G: uint8(sg / n),
				B: uint8(sb / n),
				A: uint8(sa / n),
			})
		}
	}
	return dst
}

// assetTime 返回派生对象该落在哪个年月分区下。
//
// 跟着原资产的创建时间走而不是取当前时间：同一件资产的三个规格应该待在
// 同一个目录里，否则跨月生成的缩略图会散到另一棵子树，按前缀清理时漏掉。
func assetTime(a domain.Asset, now func() time.Time) time.Time {
	if !a.CreatedAt.IsZero() {
		return a.CreatedAt
	}
	return now()
}

// truncate 截断日志里的外部输出，防止 ffmpeg 的长 stderr 淹掉日志。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
