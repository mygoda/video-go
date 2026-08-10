package httpapi

import (
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/capability"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

func firstFrameSlot() capability.InputSlotSpec {
	return capability.InputSlotSpec{
		Key: "first_frame", Label: "首帧", Kind: "image", MaxCount: 1,
		Accept:    []string{"image/png", "image/jpeg"},
		MaxBytes:  8 << 20,
		MinPixels: &capability.PixelSize{Width: 300, Height: 300},
	}
}

func pixels(w, h int) (*int, *int) { return &w, &h }

// TestCheckUploadAgainstSlot 钉住「花钱之前就拦下不合规的素材」。
//
// 首帧小于 300×300 时上游会 failed，但那是在积分已经冻结、几十秒之后才发生的。
// 这一层判定发生在 submitTask 落库之前，所以它决定的是用户会不会为一张注定
// 失败的图付钱。
func TestCheckUploadAgainstSlot(t *testing.T) {
	w200, h200 := pixels(200, 200)
	w1024, h576 := pixels(1024, 576)

	cases := []struct {
		name string
		up   domain.Upload
		want bool // 是否应当被拒
	}{
		{
			name: "合规的首帧",
			up:   domain.Upload{MIME: "image/png", Bytes: 1024, Width: w1024, Height: h576},
		},
		{
			name: "200×200 低于 min_pixels",
			up:   domain.Upload{MIME: "image/png", Bytes: 1024, Width: w200, Height: h200},
			want: true,
		},
		{
			name: "MIME 不在 accept 内",
			up:   domain.Upload{MIME: "image/gif", Bytes: 1024, Width: w1024, Height: h576},
			want: true,
		},
		{
			name: "超过 max_bytes",
			up:   domain.Upload{MIME: "image/png", Bytes: 9 << 20, Width: w1024, Height: h576},
			want: true,
		},
		{
			// 解不出尺寸（webp / 视频）时放行，否则声明了 min_pixels
			// 就等于把这类合法输入一并拒掉。
			name: "尺寸未知时放行",
			up:   domain.Upload{MIME: "image/png", Bytes: 1024},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := checkUploadAgainstSlot(firstFrameSlot(), tc.up)
			if (msg != "") != tc.want {
				t.Fatalf("want rejected=%v, got msg=%q", tc.want, msg)
			}
		})
	}
}

func TestCheckUploadAgainstSlotMaxPixels(t *testing.T) {
	slot := capability.InputSlotSpec{
		Key: "ref", Label: "参考图", Kind: "image", MaxCount: 1,
		Accept:    []string{"image/png"},
		MaxBytes:  1 << 20,
		MaxPixels: &capability.PixelSize{Width: 4096, Height: 4096},
	}
	w, h := pixels(8192, 8192)
	if msg := checkUploadAgainstSlot(slot, domain.Upload{MIME: "image/png", Bytes: 1, Width: w, Height: h}); msg == "" {
		t.Fatal("超过 max_pixels 应被拒")
	}
}
