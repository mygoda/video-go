// Package uid generates the application-side identifiers used as primary keys.
//
// 用 UUIDv7 而不是 v4：v7 的高 48 位是毫秒时间戳，因此 ORDER BY id 与
// ORDER BY created_at 同序。credit_ledger 与 assets 的游标分页都按 (时间, id)
// 排，v4 在同毫秒内的顺序是随机的，会让游标在边界上跳条目。
package uid

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// New 返回一个 UUIDv7 的标准字符串形态（36 字符，带连字符）。
func New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在现代系统上不会失败；真失败了继续跑会产生可预测 id，
		// 那比崩溃更危险。
		panic("uid: crypto/rand failed: " + err.Error())
	}

	ms := uint64(time.Now().UTC().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)

	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

// Token 返回 n 字节随机数据的十六进制串，用于 client_token、幂等键这类
// 不需要时间序的不透明标识。
func Token(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("uid: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
