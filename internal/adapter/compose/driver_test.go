package compose

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// 本文件的用例分两类：
//
//   - 不需要 ffmpeg 的（片段不足、缺 ffmpeg 时的报错形态），任何机器上都跑；
//   - 需要 ffmpeg 的，用 requireFFmpeg 自跳过。跳过而不是失败，是因为一台机器
//     上有没有 ffmpeg 不由本仓库决定；但**跳过是显式的**，看测试输出就知道
//     "拼接这条最关键的路径这次没被验证过"。
func requireFFmpeg(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("跳过：本机 PATH 上没有 ffmpeg")
	}
	return p
}

// blobs 是最小的 BlobReader：存储键直接就是本地文件路径。
type blobs map[string][]byte

func (b blobs) ReadBlob(_ context.Context, key string) ([]byte, error) {
	raw, ok := b[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return raw, nil
}

// makeClip 用 ffmpeg 现造一段纯色测试视频，避免往仓库里塞二进制夹具。
func makeClip(t *testing.T, ffmpeg, path string, extra ...string) []byte {
	t.Helper()
	args := append([]string{"-nostdin", "-loglevel", "error", "-y"}, extra...)
	args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", path)
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("造测试片段失败: %v (%s)", err, out)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读测试片段失败: %v", err)
	}
	return raw
}

// makeImage 现造一张纯色静态图。尺寸可以给奇数——用户上传的图什么尺寸都有，
// 而 yuv420p 对奇数宽高是会让 libx264 直接退出的。
func makeImage(t *testing.T, ffmpeg, path, color string, w, h int) []byte {
	t.Helper()
	out, err := exec.Command(ffmpeg, "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=%s:s=%dx%d", color, w, h),
		"-frames:v", "1", path).CombinedOutput()
	if err != nil {
		t.Fatalf("造测试图片失败: %v (%s)", err, out)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读测试图片失败: %v", err)
	}
	return raw
}

// recordingFFmpeg 包一层壳把每次 ffmpeg 调用的参数记下来，再原样转给真的
// ffmpeg。**真的编码照跑**，产物是真的——壳只是让测试能断言驱动选了哪条路径。
// 零转码那一轮是纯视频合成的关键优化，而它成功与否不体现在产物上（两条路径
// 拼出来的时长一样），只能从参数看。
func recordingFFmpeg(t *testing.T, real string) (shim string, calls func() []string) {
	t.Helper()
	dir := t.TempDir()
	shim = filepath.Join(dir, "ffmpeg")
	log := filepath.Join(dir, "calls.log")

	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(log) + "\nexec " + strconv.Quote(real) + " \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatalf("写 ffmpeg 壳失败: %v", err)
	}
	return shim, func() []string {
		raw, err := os.ReadFile(log)
		if err != nil {
			return nil
		}
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
}

// assertPlaysWhole 把成片落盘后完整解一遍，断言它是一条真能从头放到尾的片子。
//
// 只断言时长是不够的：`-c copy` 把几何不同的两段包原样接上时，时长天然加得
// 起来，坏的是轨道里混着两种画幅、DTS 不单调——播放器多半只放得出第一段。
// 这里查两件事：每一帧的画幅是否只有一种，以及完整解码有没有报错。
func assertPlaysWhole(t *testing.T, ffmpeg string, body []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.mp4")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("落地成片失败: %v", err)
	}

	ffprobe := filepath.Join(filepath.Dir(ffmpeg), "ffprobe")
	raw, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "frame=width,height", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("逐帧探测成片失败: %v", err)
	}
	shapes := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		// csv 行偶尔带个空尾字段（"320,240,"），那是格式化的事，不是画幅变了。
		if line = strings.Trim(strings.TrimSpace(line), ","); line != "" {
			shapes[line]++
		}
	}
	if len(shapes) != 1 {
		t.Fatalf("成片里混着 %d 种画幅 %v——播放器多半只放得出第一段", len(shapes), shapes)
	}

	// 完整解码一遍：DTS 错乱、坏包这类问题只有真解才暴露，探元信息看不出来。
	out, err := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("成片解不下来: %v (%s)", err, out)
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		t.Fatalf("完整解码成片时 ffmpeg 报错，说明产物是坏的：%s", out)
	}
}

// decodeArtifact 取出内联产物的原始字节。
func decodeArtifact(t *testing.T, art adapter.ArtifactRef) []byte {
	t.Helper()
	body, err := base64.StdEncoding.DecodeString(art.Base64)
	if err != nil {
		t.Fatalf("产物 base64 解码失败: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("产物是空文件")
	}
	return body
}

func segment(key string) adapter.InputRef {
	return adapter.InputRef{Slot: SlotSegments, StorageKey: key, MIME: OutputMIME}
}

func imageSegment(key, mime string) adapter.InputRef {
	return adapter.InputRef{Slot: SlotSegments, StorageKey: key, MIME: mime}
}

func errorCode(t *testing.T, err error) domain.ErrorCode {
	t.Helper()
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("错误应带分类码（*domain.Error），实际 %T: %v", err, err)
	}
	return de.Code
}

// TestInvokeConcatenates 是本包的验收用例：三段 2 秒的片子拼出来必须是 6 秒。
//
// 断言时长而不只断言"有输出"，是因为 `-c copy` 在各段参数不一致时会静默产出
// 一个只放得出第一段的文件——那种坏产物退出码是 0、文件也非空，只有时长揭穿它。
func TestInvokeConcatenates(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	dir := t.TempDir()

	src := blobs{}
	segs := make([]adapter.InputRef, 0, 3)
	for i, color := range []string{"red", "green", "blue"} {
		key := filepath.Join(dir, "src-"+strconv.Itoa(i)+".mp4")
		src[key] = makeClip(t, ffmpeg, key, "-f", "lavfi", "-i", "color=c="+color+":s=320x240:r=25:d=2")
		segs = append(segs, segment(key))
	}

	out, err := New(Driver{}).Invoke(context.Background(), adapter.SubmitInput{
		TaskID: "t-compose", Inputs: segs, Blobs: src,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.Status != adapter.StatusSucceeded {
		t.Fatalf("状态应为 succeeded，实际 %s", out.Status)
	}
	if len(out.Inline) != 1 {
		t.Fatalf("应产出 1 件产物，实际 %d 件", len(out.Inline))
	}

	art := out.Inline[0]
	if art.Type != domain.AssetTypeVideo || art.MIME != OutputMIME {
		t.Fatalf("产物应是 video/mp4，实际 %s / %s", art.Type, art.MIME)
	}
	if art.Kind != adapter.KindBase64 {
		t.Fatalf("同步族的产物只能内联回 L2，Kind 应为 base64，实际 %s", art.Kind)
	}
	body := decodeArtifact(t, art)
	assertPlaysWhole(t, ffmpeg, body)

	// 6 秒 ±0.5：证明三段都在里面，不是把第一段改了个名。
	if art.DurationMS == nil {
		t.Fatal("产物没有时长，无法证明拼接生效")
	}
	if ms := *art.DurationMS; ms < 5500 || ms > 6500 {
		t.Fatalf("成片时长 %dms，三段 2 秒应拼出约 6000ms", ms)
	}
	if art.Width == nil || *art.Width != 320 {
		t.Fatalf("成片宽度不对: %v", art.Width)
	}
}

// TestInvokeReencodesMismatchedSegments 覆盖回退路径：分辨率不一致时
// `-c copy` 会静默产出坏文件，驱动必须自己发现并改走重编码。
func TestInvokeReencodesMismatchedSegments(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	dir := t.TempDir()

	a := filepath.Join(dir, "a.mp4")
	b := filepath.Join(dir, "b.mp4")
	src := blobs{
		a: makeClip(t, ffmpeg, a, "-f", "lavfi", "-i", "color=c=red:s=320x240:r=25:d=2"),
		b: makeClip(t, ffmpeg, b, "-f", "lavfi", "-i", "color=c=blue:s=640x480:r=25:d=2"),
	}

	out, err := New(Driver{}).Invoke(context.Background(), adapter.SubmitInput{
		TaskID: "t-mismatch", Inputs: []adapter.InputRef{segment(a), segment(b)}, Blobs: src,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	art := out.Inline[0]
	if art.DurationMS == nil {
		t.Fatal("产物没有时长")
	}
	if ms := *art.DurationMS; ms < 3500 || ms > 4500 {
		t.Fatalf("成片时长 %dms，两段 2 秒应拼出约 4000ms——说明零转码的坏结果被当成了成功", ms)
	}
	// 时长这道断言拦不住这一路的真问题：`-c copy` 把 320x240 与 640x480 接在
	// 一起时长照样是 4 秒，坏的是轨道里混着两种画幅。必须真解一遍才看得见。
	assertPlaysWhole(t, ffmpeg, decodeArtifact(t, art))
}

// TestInvokeGivesStillImagesADuration 是本票的验收用例：两张静态图拼出来
// 必须是 ≈4 秒，不是 0.08 秒。
//
// 图片进 concat 只贡献 1 帧，n 张图 = n 帧；那种产物文件是真的、能播、任务
// 绿灯，只有量时长才揭穿它。两张图故意给奇数宽高与不同尺寸——用户上传的图
// 什么尺寸都有，而 yuv420p 对奇数宽高会让 libx264 直接退出。
func TestInvokeGivesStillImagesADuration(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	dir := t.TempDir()

	a := filepath.Join(dir, "a.png")
	b := filepath.Join(dir, "b.jpg")
	src := blobs{
		a: makeImage(t, ffmpeg, a, "red", 321, 241),
		b: makeImage(t, ffmpeg, b, "blue", 640, 480),
	}

	out, err := New(Driver{}).Invoke(context.Background(), adapter.SubmitInput{
		TaskID: "t-stills",
		Inputs: []adapter.InputRef{imageSegment(a, "image/png"), imageSegment(b, "image/jpeg")},
		Blobs:  src,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	art := out.Inline[0]
	if art.DurationMS == nil {
		t.Fatal("产物没有时长——落库 duration_ms 会是 NULL")
	}
	if ms := *art.DurationMS; ms < 3500 || ms > 4500 {
		t.Fatalf("成片时长 %dms，两张图各停 %g 秒应约 4000ms（0.08 秒那种就是没给图片时间轴）", ms, stillSeconds)
	}
	if art.Width == nil || art.Height == nil {
		t.Fatalf("成片没量出宽高，落库会是 NULL: %v x %v", art.Width, art.Height)
	}
	// 奇数宽高被抹到偶数，否则 libx264 根本编不出来。
	if *art.Width%2 != 0 || *art.Height%2 != 0 {
		t.Fatalf("成片宽高应为偶数，实际 %dx%d", *art.Width, *art.Height)
	}
}

// TestInvokeMixesImageAndVideo：图 + 视频混合时两段都要在成片里。
func TestInvokeMixesImageAndVideo(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	dir := t.TempDir()

	img := filepath.Join(dir, "still.png")
	clip := filepath.Join(dir, "clip.mp4")
	src := blobs{
		img:  makeImage(t, ffmpeg, img, "red", 320, 240),
		clip: makeClip(t, ffmpeg, clip, "-f", "lavfi", "-i", "color=c=blue:s=640x480:r=25:d=3"),
	}

	out, err := New(Driver{}).Invoke(context.Background(), adapter.SubmitInput{
		TaskID: "t-mixed",
		Inputs: []adapter.InputRef{imageSegment(img, "image/png"), segment(clip)},
		Blobs:  src,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	art := out.Inline[0]
	if art.DurationMS == nil {
		t.Fatal("产物没有时长")
	}
	// 图 2 秒 + 视频 3 秒 = 5 秒。少于这个数说明有一段被吞了。
	if ms := *art.DurationMS; ms < 4500 || ms > 5500 {
		t.Fatalf("成片时长 %dms，图 %g 秒 + 视频 3 秒应约 5000ms", ms, stillSeconds)
	}
	// 「两段都看得到」不是时长对得上就算数：图和视频的画幅本来就不同，
	// 不归一化拼出来是一条画幅中途变、DTS 错乱的片子。
	assertPlaysWhole(t, ffmpeg, decodeArtifact(t, art))
}

// TestInvokeVideoOnlyStaysOnCopyPath 钉住"给图片加停留时长不能拖累纯视频"。
//
// 零转码那一轮是十二段几百毫秒的关键优化，而它成功与否不体现在产物上——
// 两条路径拼出来的时长一样。只能看驱动到底把什么参数交给了 ffmpeg：
// 纯视频合成必须只调一次 ffmpeg，带 `-c copy`，且不出现 filter_complex。
func TestInvokeVideoOnlyStaysOnCopyPath(t *testing.T) {
	real := requireFFmpeg(t)
	shim, calls := recordingFFmpeg(t, real)
	dir := t.TempDir()

	src := blobs{}
	segs := make([]adapter.InputRef, 0, 3)
	for i, color := range []string{"red", "green", "blue"} {
		key := filepath.Join(dir, "src-"+strconv.Itoa(i)+".mp4")
		src[key] = makeClip(t, real, key, "-f", "lavfi", "-i", "color=c="+color+":s=320x240:r=25:d=2")
		segs = append(segs, segment(key))
	}

	out, err := New(Driver{FFmpegPath: shim}).Invoke(context.Background(), adapter.SubmitInput{
		TaskID: "t-copyfast", Inputs: segs, Blobs: src,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if ms := out.Inline[0].DurationMS; ms == nil || *ms < 5500 || *ms > 6500 {
		t.Fatalf("成片时长 %v，三段 2 秒应拼出约 6000ms", ms)
	}

	got := calls()
	if len(got) != 1 {
		t.Fatalf("纯视频合成应只调一次 ffmpeg（零转码那一轮），实际调了 %d 次：\n%s",
			len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[0], "-c copy") {
		t.Fatalf("那一次调用应是零转码，实际：%s", got[0])
	}
	if strings.Contains(got[0], "filter_complex") {
		t.Fatalf("纯视频合成不该走重编码，实际：%s", got[0])
	}
}

// TestInvokeRejectsSingleSegment：一段的"合成"没有意义，让它成功会产出一条
// 和输入一模一样的视频，用户会以为拼接生效了。
func TestInvokeRejectsSingleSegment(t *testing.T) {
	_, err := New(Driver{}).Invoke(context.Background(), adapter.SubmitInput{
		TaskID: "t-one", Inputs: []adapter.InputRef{segment("only.mp4")}, Blobs: blobs{},
	})
	if err == nil {
		t.Fatal("只有一段就该报参数错误")
	}
	if code := errorCode(t, err); code != domain.ErrorCode(domain.TaskErrorInvalidParam) {
		t.Fatalf("错误码应为 invalid_param，实际 %s", code)
	}
}

// TestInvokeWithoutFFmpegFailsLoudly 钉住本包最重要的那条决定：缺 ffmpeg 时
// 报错，而不是退回产出一份 JSON 清单假装成功。直接构造 driver 而不走 New，
// 是因为 New 会去 PATH 上找 ffmpeg——在装了 ffmpeg 的机器上就复现不出这一路。
func TestInvokeWithoutFFmpegFailsLoudly(t *testing.T) {
	d := driver{}
	_, err := d.Invoke(context.Background(), adapter.SubmitInput{
		TaskID: "t-noffmpeg",
		Inputs: []adapter.InputRef{segment("a.mp4"), segment("b.mp4")},
		Blobs:  blobs{},
	})
	if err == nil {
		t.Fatal("没有 ffmpeg 就该失败")
	}
	if !strings.Contains(err.Error(), "ffmpeg") {
		t.Fatalf("错误信息该说清楚是缺 ffmpeg，实际：%v", err)
	}
}
