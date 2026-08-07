package mock

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/png"
	"math"
	"strconv"
	"strings"
)

// 占位图的尺寸边界。上限存在是因为 mock 是本地开发路径，
// 有人把 aspect 配成 8192×8192 时不该让开发机去画一张 64M 像素的图。
const (
	minPlaceholderSide = 64
	maxPlaceholderSide = 2048
	defaultImageWidth  = 1024
	defaultImageHeight = 1024
)

// 平台侧参数名。mock 不做 request_mapping 渲染（它没有上游），
// 直接读平台参数来决定画什么——**这不是把参数名硬编码进协议**，
// 而是"占位图长什么样"这件事本身就只有 mock 自己关心。
const (
	paramSeed   = "seed"
	paramAspect = "aspect"
	paramWidth  = "width"
	paramHeight = "height"
	paramSize   = "size"
)

// renderPlaceholderPNG 画一张真正的 PNG。
//
// **画面随参数变化**是刻意的：如果所有 mock 产物长得一模一样，
// 画布上 20 张卡片全是同一张图，"参数确实传到了底层"这件事在肉眼上
// 完全无法证伪——改了 seed 没生效和改了 seed 生效了，看起来一样。
// 让 seed 决定配色与噪声、让比例决定画幅，一眼就能看出链路通没通。
func renderPlaceholderPNG(params map[string]any, seedExtra string, index int) ([]byte, int, int, error) {
	w, h := placeholderSize(params)
	seed := placeholderSeed(params, seedExtra, index)

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	drawGradient(img, seed)
	drawSeedBlocks(img, seed)
	drawBorder(img, seed)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, 0, 0, fmt.Errorf("mock: 占位图编码失败: %w", err)
	}
	return buf.Bytes(), w, h, nil
}

// placeholderSize 从参数推出画幅。
//
// 三条来源按优先级：显式 width/height > size("1024x768") > aspect("16:9")。
// 都没有就用 1024²。aspect 只给比例不给像素，按固定面积换算成边长，
// 这样 16:9 与 9:16 出来的图大小相当而不是一个巨大一个极小。
func placeholderSize(params map[string]any) (int, int) {
	if w, okW := intParam(params, paramWidth); okW {
		if h, okH := intParam(params, paramHeight); okH {
			return clampSide(w), clampSide(h)
		}
	}
	if s, ok := stringParam(params, paramSize); ok {
		if w, h, ok := parseDimensions(s); ok {
			return clampSide(w), clampSide(h)
		}
		if rw, rh, ok := parseRatio(s); ok {
			return sizeFromRatio(rw, rh)
		}
	}
	if s, ok := stringParam(params, paramAspect); ok {
		if rw, rh, ok := parseRatio(s); ok {
			return sizeFromRatio(rw, rh)
		}
		if w, h, ok := parseDimensions(s); ok {
			return clampSide(w), clampSide(h)
		}
	}
	return defaultImageWidth, defaultImageHeight
}

// sizeFromRatio 在保持面积恒定的前提下把比例换算成边长，
// 并把边长对齐到 8 的倍数——真实出图模型基本都有这个约束，
// mock 跟着对齐，免得前端按 mock 的尺寸写死了布局假设。
func sizeFromRatio(rw, rh float64) (int, int) {
	const area = float64(defaultImageWidth * defaultImageHeight)
	if rw <= 0 || rh <= 0 {
		return defaultImageWidth, defaultImageHeight
	}
	scale := math.Sqrt(area / (rw * rh))
	return clampSide(align8(rw * scale)), clampSide(align8(rh * scale))
}

func align8(v float64) int {
	n := int(math.Round(v/8)) * 8
	if n <= 0 {
		return 8
	}
	return n
}

func clampSide(v int) int {
	if v < minPlaceholderSide {
		return minPlaceholderSide
	}
	if v > maxPlaceholderSide {
		return maxPlaceholderSide
	}
	return v
}

// parseRatio 解析 "16:9" 这类比例串。
func parseRatio(s string) (float64, float64, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	h, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// parseDimensions 解析 "1024x768" 这类像素串，x 与 × 都认。
func parseDimensions(s string) (int, int, bool) {
	norm := strings.ToLower(strings.TrimSpace(s))
	norm = strings.ReplaceAll(norm, "×", "x")
	parts := strings.Split(norm, "x")
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// placeholderSeed 把参数里的 seed 与调用侧补充的串揉成一个稳定的哈希。
//
// 同一个 seed + 同一组参数必须出同一张图：真实模型就是这个语义，
// mock 若每次都随机，"seed 锁定构图"这条产品行为在本地永远验证不了。
func placeholderSeed(params map[string]any, extra string, index int) uint64 {
	h := fnv.New64a()
	if v, ok := params[paramSeed]; ok && v != nil {
		fmt.Fprintf(h, "seed=%v;", v)
	}
	// 其余参数按固定顺序参与哈希，让改任何一个参数都能看出画面变了。
	for _, k := range []string{paramAspect, paramSize, paramWidth, paramHeight} {
		if v, ok := params[k]; ok && v != nil {
			fmt.Fprintf(h, "%s=%v;", k, v)
		}
	}
	fmt.Fprintf(h, "extra=%s;idx=%d", extra, index)
	return h.Sum64()
}

// drawGradient 铺一层由 seed 决定色相的双向渐变。
func drawGradient(img *image.RGBA, seed uint64) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	hue := float64(seed%360) / 360.0
	c1 := hsvToRGB(hue, 0.55, 0.32)
	c2 := hsvToRGB(math.Mod(hue+0.28, 1.0), 0.62, 0.86)

	for y := 0; y < h; y++ {
		fy := float64(y) / float64(maxInt(h-1, 1))
		for x := 0; x < w; x++ {
			fx := float64(x) / float64(maxInt(w-1, 1))
			t := (fx + fy) / 2
			img.SetRGBA(x, y, color.RGBA{
				R: lerp8(c1.R, c2.R, t),
				G: lerp8(c1.G, c2.G, t),
				B: lerp8(c1.B, c2.B, t),
				A: 255,
			})
		}
	}
}

// drawSeedBlocks 画一片由 seed 决定的方块图案。
//
// 它是"这张图对应哪个 seed"的视觉指纹：渐变色相变化太柔和，
// 相邻两个 seed 肉眼分不出来；离散方块能让差异一眼可见。
func drawSeedBlocks(img *image.RGBA, seed uint64) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	const grid = 8
	cw, ch := maxInt(w/grid, 1), maxInt(h/grid, 1)

	rnd := seed
	for gy := 0; gy < grid; gy++ {
		for gx := 0; gx < grid; gx++ {
			rnd = rnd*6364136223846793005 + 1442695040888963407
			if rnd>>60&1 == 0 {
				continue
			}
			shade := uint8(rnd >> 32 & 0x7F)
			x0, y0 := gx*cw, gy*ch
			x1, y1 := minInt(x0+cw, w), minInt(y0+ch, h)
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					c := img.RGBAAt(x, y)
					img.SetRGBA(x, y, color.RGBA{
						R: lerp8(c.R, 255-shade, 0.35),
						G: lerp8(c.G, 255-shade, 0.35),
						B: lerp8(c.B, shade, 0.35),
						A: 255,
					})
				}
			}
		}
	}
}

// drawBorder 画一圈边框，让缩略图在画布上有明确的边界。
func drawBorder(img *image.RGBA, seed uint64) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	thickness := maxInt(minInt(w, h)/64, 2)
	c := hsvToRGB(math.Mod(float64(seed%360)/360.0+0.5, 1.0), 0.7, 0.95)

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < thickness || y < thickness || x >= w-thickness || y >= h-thickness {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

func hsvToRGB(hue, sat, val float64) color.RGBA {
	i := math.Floor(hue * 6)
	f := hue*6 - i
	p := val * (1 - sat)
	q := val * (1 - f*sat)
	t := val * (1 - (1-f)*sat)

	var r, g, b float64
	switch int(i) % 6 {
	case 0:
		r, g, b = val, t, p
	case 1:
		r, g, b = q, val, p
	case 2:
		r, g, b = p, val, t
	case 3:
		r, g, b = p, q, val
	case 4:
		r, g, b = t, p, val
	default:
		r, g, b = val, p, q
	}
	return color.RGBA{R: uint8(r * 255), G: uint8(g * 255), B: uint8(b * 255), A: 255}
}

func lerp8(a, b uint8, t float64) uint8 {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	return uint8(float64(a) + (float64(b)-float64(a))*t)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// intParam / stringParam 读平台参数。JSON 解出来的数字是 float64，
// 但配置直接构造时可能是 int，两种都要认。
func intParam(params map[string]any, key string) (int, bool) {
	v, ok := params[key]
	if !ok || v == nil {
		return 0, false
	}
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func stringParam(params map[string]any, key string) (string, bool) {
	v, ok := params[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}
