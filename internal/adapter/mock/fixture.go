package mock

import (
	_ "embed"
	"encoding/base64"
)

// sampleMP4 是内嵌的占位视频：256×144、2 秒、h264 + aac，约 9 KB。
//
// # 为什么内嵌一个真文件而不是现场合成
//
// 转存链路后面站着 asset.Deriver：它要抽首帧做 poster、要读出时长与画幅。
// 一段字节数正确但结构无效的假 mp4 会让 Deriver 在本地永远失败，
// 于是"视频卡片的封面"这条路在零凭证环境下就验证不了——而那正是
// mock 存在的全部理由。用 ffmpeg 生成一次、提交进版本库，
// 换来的是本地不依赖 ffmpeg 也能跑通全链路。
//
//go:embed fixtures/sample.mp4
var sampleMP4 []byte

// encodeBase64 是同步驱动交付 KindBase64 产物时用的编码。
// 用标准编码（带 padding）与 OpenAI 的 b64_json 保持一致，
// L3 那边解码走的是同一个分支。
func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
