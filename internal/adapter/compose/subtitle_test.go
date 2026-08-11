package compose

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
)

// TestSRTLaysOutTheTimeline 钉住时间轴的累加方式：第 n 条从前 n-1 段的时长
// 之和开始。算错这件事在产物上是"字幕整体偏移"，而偏移的片子照样能播、
// 照样绿灯，只有逐条对时间码才看得见。
func TestSRTLaysOutTheTimeline(t *testing.T) {
	got := SRT([]Cue{
		{Text: "你终于来了", DurationMS: 4000},
		{Text: "我等了很久", DurationMS: 3500},
	})
	want := "1\n00:00:00,000 --> 00:00:04,000\n你终于来了\n\n" +
		"2\n00:00:04,000 --> 00:00:07,500\n我等了很久\n\n"
	if got != want {
		t.Fatalf("SRT 不对：\n得到:\n%s\n期望:\n%s", got, want)
	}
}

// TestSRTSkipsSilentSegmentsButKeepsTime：空镜不出条目，但它占的时间必须
// 照样推进——不推进的话，它后面每一条字幕都会提前，越往后错得越多。
//
// 顺带钉住序号：SubRip 的序号必须从 1 连续递增，用段号编会跳号，
// 而跳号会让一部分播放器从那一条起整条轨都不再显示。
func TestSRTSkipsSilentSegmentsButKeepsTime(t *testing.T) {
	got := SRT([]Cue{
		{Text: "  ", DurationMS: 2000},
		{Text: "第二镜", DurationMS: 1000},
		{Text: "", DurationMS: 5000},
		{Text: "第四镜", DurationMS: 1500},
	})
	want := "1\n00:00:02,000 --> 00:00:03,000\n第二镜\n\n" +
		"2\n00:00:08,000 --> 00:00:09,500\n第四镜\n\n"
	if got != want {
		t.Fatalf("空镜应只占时间不出条目、序号连续：\n得到:\n%s\n期望:\n%s", got, want)
	}
}

// TestSRTTimeCodeFormat 覆盖跨分、跨时的进位与毫秒补零。
//
// 分隔符必须是逗号：WebVTT 用点、SubRip 用逗号，写成点的那份 ffmpeg 会当成
// 格式错误把**整份**丢掉，而不是只丢那一条——成片里就一个字幕都没有。
func TestSRTTimeCodeFormat(t *testing.T) {
	got := SRT([]Cue{{Text: "x", DurationMS: 3_723_004}})
	if !strings.Contains(got, "00:00:00,000 --> 01:02:03,004") {
		t.Fatalf("时间码格式不对：%q", got)
	}
	if strings.Contains(got, ".") {
		t.Fatalf("SubRip 的毫秒分隔符是逗号不是点：%q", got)
	}
}

// TestCopyArgsUnchangedWithoutOptions 是本次改动最要紧的一条回归：两个开关
// 都不开时，零转码那一轮交给 ffmpeg 的参数必须与加开关之前**逐字相同**。
//
// 断言完整参数序列而不是"包含 -c copy"：多一个 -map、少一个 -movflags 都不会
// 让测试变红，却足以让成片丢掉音轨或不再支持边下边播。
func TestCopyArgsUnchangedWithoutOptions(t *testing.T) {
	got := copyArgs("/w/concat.txt", "/w/out.mp4", composeOptions{})
	want := []string{
		"-nostdin", "-loglevel", "error", "-y",
		"-f", "concat", "-safe", "0", "-i", "/w/concat.txt",
		"-c", "copy",
		"-movflags", "+faststart", "/w/out.mp4",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("不开开关时参数应与改动前一致：\n得到: %v\n期望: %v", got, want)
	}
}

// TestCopyArgsKeepsCopyWithOptions 钉住"开了开关也不重编码"。
//
// 静音是丢一条轨、字幕是往容器里多写一条文本轨，两件事都在容器层，视频包
// 一个字节都不用重编。把静音实现成"重编码时不接音频"会让每次静音合成白跑
// 一遍 libx264——十二段就是分钟级的等待，而用户只是想关个声音。
func TestCopyArgsKeepsCopyWithOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts composeOptions
		want []string
		deny []string
	}{
		{
			name: "只静音",
			opts: composeOptions{mute: true},
			want: []string{"-c copy", "-an"},
			deny: []string{"-map", "libx264", "filter_complex"},
		},
		{
			name: "只字幕",
			opts: composeOptions{subtitlePath: "/w/subtitles.srt"},
			want: []string{"-i /w/subtitles.srt", "-c copy", "-map 0:v:0", "-map 0:a?", "-map 1:s:0", "-c:s " + SubtitleCodec},
			deny: []string{"-an", "libx264", "filter_complex"},
		},
		{
			name: "静音加字幕",
			opts: composeOptions{mute: true, subtitlePath: "/w/subtitles.srt"},
			want: []string{"-c copy", "-map 0:v:0", "-map 1:s:0", "-an"},
			// 静音时那一路音频不能映射，否则 -an 与 -map 0:a 自相矛盾。
			deny: []string{"-map 0:a", "libx264", "filter_complex"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := strings.Join(copyArgs("/w/concat.txt", "/w/out.mp4", tc.opts), " ")
			for _, w := range tc.want {
				if !strings.Contains(line, w) {
					t.Fatalf("参数里应有 %q，实际：%s", w, line)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(line, d) {
					t.Fatalf("参数里不该有 %q，实际：%s", d, line)
				}
			}
		})
	}
}

// makeSoundClip 造一段带音轨的测试视频。默认的 makeClip 是纯视频，
// 用它测静音等于什么都没测——本来就没有音轨可去。
func makeSoundClip(t *testing.T, ffmpeg, path, color string, seconds float64) []byte {
	t.Helper()
	dur := strconv.FormatFloat(seconds, 'f', -1, 64)
	out, err := exec.Command(ffmpeg, "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=%s:s=320x240:r=25:d=%s", color, dur),
		"-f", "lavfi", "-i", "sine=frequency=440:duration="+dur,
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", path).CombinedOutput()
	if err != nil {
		t.Fatalf("造带声测试片段失败: %v (%s)", err, out)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读测试片段失败: %v", err)
	}
	return raw
}

// probeStreams 报告成片里各类流各有几条。
func probeStreams(t *testing.T, ffmpeg, path string) map[string]int {
	t.Helper()
	ffprobe := filepath.Join(filepath.Dir(ffmpeg), "ffprobe")
	raw, err := exec.Command(ffprobe, "-v", "error",
		"-show_entries", "stream=codec_name,codec_type", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("探测流失败: %v", err)
	}
	got := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			got[line]++
		}
	}
	return got
}

// writeOutput 把内联产物落到磁盘，返回路径。
func writeOutput(t *testing.T, art adapter.ArtifactRef) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.mp4")
	if err := os.WriteFile(path, decodeArtifact(t, art), 0o600); err != nil {
		t.Fatalf("落地成片失败: %v", err)
	}
	return path
}

// soundSegments 造 n 段带声片段并返回输入槽。
func soundSegments(t *testing.T, ffmpeg, dir string, colors ...string) (blobs, []adapter.InputRef) {
	t.Helper()
	src := blobs{}
	segs := make([]adapter.InputRef, 0, len(colors))
	for i, color := range colors {
		key := filepath.Join(dir, "snd-"+strconv.Itoa(i)+".mp4")
		src[key] = makeSoundClip(t, ffmpeg, key, color, 2)
		segs = append(segs, segment(key))
	}
	return src, segs
}

// TestInvokeMutesWithoutReencoding 是静音这一半的验收：成片里不能有音频流，
// 而且这件事不许花一次重编码换来。
func TestInvokeMutesWithoutReencoding(t *testing.T) {
	real := requireFFmpeg(t)
	shim, calls := recordingFFmpeg(t, real)
	dir := t.TempDir()
	src, segs := soundSegments(t, real, dir, "red", "green")

	out, err := New(Driver{FFmpegPath: shim}).Invoke(context.Background(), adapter.SubmitInput{
		TaskID: "t-mute", Inputs: segs, Blobs: src,
		Params: map[string]any{ParamMute: true},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	streams := probeStreams(t, real, writeOutput(t, out.Inline[0]))
	for k, n := range streams {
		if strings.HasSuffix(k, ",audio") {
			t.Fatalf("成片仍带 %d 条音频流 %q——`-an` 没生效", n, k)
		}
	}
	if streams["h264,video"] != 1 {
		t.Fatalf("成片应有且只有一条 h264 视频流，实际：%v", streams)
	}
	if ms := out.Inline[0].DurationMS; ms == nil || *ms < 3500 || *ms > 4500 {
		t.Fatalf("成片时长 %v，两段 2 秒应约 4000ms", ms)
	}

	got := calls()
	if len(got) != 1 {
		t.Fatalf("静音是容器层的事，应只调一次 ffmpeg（零转码那一轮），实际 %d 次：\n%s",
			len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[0], "-c copy") || strings.Contains(got[0], "filter_complex") {
		t.Fatalf("静音不该把零转码挤掉，实际：%s", got[0])
	}
}

// TestInvokeAttachesSoftSubtitleTrack 是字幕这一半的验收：成片里要有一条
// mov_text 字幕轨，正文与传进来的**逐字相同**，视频与音频都还在，
// 且整件事仍然是零转码。
//
// 断言正文而不只是"有一条字幕轨"：一条内容被截断或乱码的轨在 ffprobe 眼里
// 一样是流，用户点开才发现字不对。
func TestInvokeAttachesSoftSubtitleTrack(t *testing.T) {
	real := requireFFmpeg(t)
	shim, calls := recordingFFmpeg(t, real)
	dir := t.TempDir()
	src, segs := soundSegments(t, real, dir, "red", "blue")

	srt := SRT([]Cue{
		{Text: "你终于来了", DurationMS: 2000},
		{Text: "我等了很久", DurationMS: 2000},
	})
	out, err := New(Driver{FFmpegPath: shim}).Invoke(context.Background(), adapter.SubmitInput{
		TaskID: "t-subs", Inputs: segs, Blobs: src,
		Params: map[string]any{ParamSubtitleSRT: srt},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	path := writeOutput(t, out.Inline[0])
	streams := probeStreams(t, real, path)
	if streams[SubtitleCodec+",subtitle"] != 1 {
		t.Fatalf("成片应有一条 %s 字幕轨，实际：%v", SubtitleCodec, streams)
	}
	if streams["h264,video"] != 1 {
		t.Fatalf("挂字幕不该动视频轨，实际：%v", streams)
	}
	// 音轨必须还在：挂第二个输入之后 ffmpeg 的默认挑流会丢掉片段那一路的音频，
	// 这正是 copyArgs 要显式写 `-map 0:a?` 的原因。
	if streams["aac,audio"] != 1 {
		t.Fatalf("挂字幕不该把音轨挤掉，实际：%v", streams)
	}

	// 把字幕轨解回文本，逐字比对。
	back := filepath.Join(filepath.Dir(path), "back.srt")
	if o, err := exec.Command(real, "-nostdin", "-loglevel", "error", "-y",
		"-i", path, "-map", "0:s:0", "-c:s", "srt", back).CombinedOutput(); err != nil {
		t.Fatalf("从成片里取回字幕失败: %v (%s)", err, o)
	}
	raw, err := os.ReadFile(back)
	if err != nil {
		t.Fatalf("读回字幕失败: %v", err)
	}
	for _, line := range []string{"你终于来了", "我等了很久"} {
		if !strings.Contains(string(raw), line) {
			t.Fatalf("字幕正文与台词对不上，成片里的是：\n%s", raw)
		}
	}

	got := calls()
	if len(got) != 1 {
		t.Fatalf("挂软字幕是容器层的事，应只调一次 ffmpeg，实际 %d 次：\n%s",
			len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[0], "-c copy") || strings.Contains(got[0], "filter_complex") {
		t.Fatalf("挂字幕不该把零转码挤掉，实际：%s", got[0])
	}
}

// TestInvokeIgnoresEmptySubtitle：一份空 SRT 不该挂出一条空轨。
//
// 空轨在播放器的字幕菜单里照样占一项，用户点开发现全程没字，只会以为
// 字幕功能坏了——比没有这一项更糟。
func TestInvokeIgnoresEmptySubtitle(t *testing.T) {
	real := requireFFmpeg(t)
	dir := t.TempDir()
	src, segs := soundSegments(t, real, dir, "red", "green")

	out, err := New(Driver{}).Invoke(context.Background(), adapter.SubmitInput{
		TaskID: "t-emptysubs", Inputs: segs, Blobs: src,
		Params: map[string]any{ParamSubtitleSRT: "   \n"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	for k := range probeStreams(t, real, writeOutput(t, out.Inline[0])) {
		if strings.HasSuffix(k, ",subtitle") {
			t.Fatalf("空字幕不该挂出轨道，实际有 %q", k)
		}
	}
}

// TestInvokeSubtitlesSurviveReencode：回退到重编码那一路时字幕也要挂上。
//
// 两段几何不同会逼出第二轮，而第二轮的参数是另一套代码——字幕在那条路上
// 掉了的话，用户看到的是"分辨率一致就有字幕、不一致就没有"这种没法解释的现象。
func TestInvokeSubtitlesSurviveReencode(t *testing.T) {
	real := requireFFmpeg(t)
	dir := t.TempDir()

	a := filepath.Join(dir, "a.mp4")
	b := filepath.Join(dir, "b.mp4")
	src := blobs{
		a: makeClip(t, real, a, "-f", "lavfi", "-i", "color=c=red:s=320x240:r=25:d=2"),
		b: makeClip(t, real, b, "-f", "lavfi", "-i", "color=c=blue:s=640x480:r=25:d=2"),
	}

	out, err := New(Driver{}).Invoke(context.Background(), adapter.SubmitInput{
		TaskID: "t-subs-reencode", Inputs: []adapter.InputRef{segment(a), segment(b)}, Blobs: src,
		Params: map[string]any{ParamSubtitleSRT: SRT([]Cue{{Text: "重编码也要有字", DurationMS: 2000}})},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	streams := probeStreams(t, real, writeOutput(t, out.Inline[0]))
	if streams[SubtitleCodec+",subtitle"] != 1 {
		t.Fatalf("重编码那一路也该挂上字幕轨，实际：%v", streams)
	}
}

// TestStillSegmentMSMatchesDriver 钉住导出常量与驱动实际给图片的时长同源。
//
// 这两个数一旦分开写，图片一多，上层算出来的字幕时间轴会与画面越差越远——
// 而偏移的字幕比没有字幕更难被发现。
func TestStillSegmentMSMatchesDriver(t *testing.T) {
	if want := int(stillSeconds * 1000); StillSegmentMS != want {
		t.Fatalf("StillSegmentMS = %d，驱动给图片的是 %d ms", StillSegmentMS, want)
	}
}
