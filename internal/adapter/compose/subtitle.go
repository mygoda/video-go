package compose

import (
	"fmt"
	"strings"
)

// StillSegmentMS 是一张静态图参与合成时占的毫秒数。
//
// 导出它是因为字幕的时间轴必须与实际画面对得上，而算时间轴的是上一层
// （它手里有各段资产的 duration_ms），图片资产在库里根本没有时长——那个数
// 是本包在 stillToClip 里现给的。两处各写一个 2000 的那天，图片一多，
// 后面每一条字幕都会提前或延后。
const StillSegmentMS = int(stillSeconds * 1000)

// Cue 是字幕轨上的一段：这一段片子里说的话，以及它占多长。
//
// 不带绝对起止时刻而带时长，是因为**字幕的时间轴就是片段的时间轴**：
// 第 n 条从前 n-1 段的时长之和开始。让调用方自己去累加，等于把这条不变式
// 复制一份出去，而它一旦被抄错就是整条字幕的偏移。
type Cue struct {
	// Text 是这一段的台词原文，逐字来自镜头卡。空串表示这一段没有人说话
	// （空镜、纯环境音），它照样占时间，只是不出字。
	Text string
	// DurationMS 是这一段在成片里占的毫秒数。
	DurationMS int
}

// SRT 把按片段顺序排好的一串 cue 拼成一份 SubRip 字幕。
//
// # 为什么是 SRT 而不是直接生成 mp4 的字幕轨
//
// SRT 是 ffmpeg 认得最广的字幕输入格式，而输出端要什么容器由 `-c:s` 决定
// （本包挂的是 mov_text）。中间隔一层纯文本还有个好处：出问题时可以把它
// 单独打出来看，而不必去 demux 一个二进制轨。
//
// # 空文本不出条目，但仍然推进时间轴
//
// 一条只有序号和时间、正文为空的 SRT 条目在播放器上是一次"字幕闪了一下
// 又没字"，而空镜本来就该什么都不显示。所以空的那几段只参与累加。
//
// 序号按**实际写出的条目**连续编号，不能用段号：SubRip 的序号必须从 1 连续
// 递增，跳号会让一部分播放器从跳号那一条起整条轨都不再显示。
func SRT(cues []Cue) string {
	var b strings.Builder
	var at int
	n := 0
	for _, c := range cues {
		start := at
		at += c.DurationMS
		text := strings.TrimSpace(c.Text)
		if text == "" || c.DurationMS <= 0 {
			continue
		}
		n++
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", n, srtTime(start), srtTime(at), text)
	}
	return b.String()
}

// srtTime 把毫秒格成 SubRip 的 HH:MM:SS,mmm。
//
// 分隔符是逗号不是小数点——WebVTT 用点，SubRip 用逗号，写错的那份 ffmpeg
// 会当成格式错误整份丢掉，而不是只丢那一条。
func srtTime(ms int) string {
	if ms < 0 {
		ms = 0
	}
	h := ms / 3_600_000
	m := ms / 60_000 % 60
	s := ms / 1_000 % 60
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms%1000)
}
