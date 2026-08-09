package compose

import (
	"context"
	"encoding/base64"
	"errors"
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

func segment(key string) adapter.InputRef {
	return adapter.InputRef{Slot: SlotSegments, StorageKey: key, MIME: OutputMIME}
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
	body, err := base64.StdEncoding.DecodeString(art.Base64)
	if err != nil {
		t.Fatalf("产物 base64 解码失败: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("产物是空文件")
	}

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
