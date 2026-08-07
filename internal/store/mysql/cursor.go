package mysql

import (
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// 分页边界，与 openapi 的 limit 参数（min 1 / max 100 / default 30）一致。
const (
	defaultLimit = 30
	maxLimit     = 100
)

// clampLimit 把调用方给的 limit 收进合法区间。
//
// 越界不报错而是收敛：limit 来自 query string，一个手写 URL 的用户
// 传了 limit=100000 不该收到 400，更不该让一条查询把库拖垮。
func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}

// cursor 是游标的解码形态。
//
// 只带 (时间, id) 两个字段：所有列表的排序键都是 (created_at DESC, id DESC)
// 或纯 (id DESC)，前者需要时间兜底、后者只用 id。UUIDv7 的高位是毫秒时间戳，
// 因此纯 id 排序与时间序天然一致，同毫秒内也由 id 给出稳定的全序——
// 这正是 uid 包选 v7 而不是 v4 的原因。
type cursor struct {
	// At 是排序主键的时间部分。零值表示这个游标只按 id 排。
	At time.Time
	// ID 是排序的兜底键，也是"从哪条之后继续"的锚点。
	ID string
	// Seq 用于 task_events 这类自增整数主键的游标。
	Seq int64
}

// 游标编码格式的版本前缀。带上它，将来换编码时旧游标能被识别成"过期"
// 而不是被当成新格式解出一个乱七八糟的锚点。
const cursorVersion = "v1"

// encodeCursor 把锚点编成不透明字符串。
//
// base64url 无填充：游标会出现在 query string 里，标准 base64 的 + / =
// 都要再转义一次，凭空多出一层可能被中间件搞坏的编码。
func encodeCursor(c cursor) string {
	var b strings.Builder
	b.WriteString(cursorVersion)
	b.WriteByte('|')
	if c.At.IsZero() {
		b.WriteByte('-')
	} else {
		b.WriteString(strconv.FormatInt(c.At.UTC().UnixMilli(), 10))
	}
	b.WriteByte('|')
	b.WriteString(c.ID)
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(c.Seq, 10))
	return base64.RawURLEncoding.EncodeToString([]byte(b.String()))
}

// decodeCursor 解出锚点。
//
// **任何解不开的输入都返回 (零值, false)，绝不报错。** 游标是客户端回传的
// 不透明串，被截断、被浏览器改写、被用户手贱改了一个字符都是常态；
// 对垃圾游标报 500 会让一次列表刷新变成一个错误页，而正确的降级是
// "当作没有游标，回到第一页"。
func decodeCursor(raw string) (cursor, bool) {
	if raw == "" {
		return cursor{}, false
	}
	blob, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return cursor{}, false
	}
	parts := strings.Split(string(blob), "|")
	if len(parts) != 4 || parts[0] != cursorVersion {
		return cursor{}, false
	}

	var c cursor
	if parts[1] != "-" {
		ms, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return cursor{}, false
		}
		c.At = time.UnixMilli(ms).UTC()
	}
	c.ID = parts[2]
	// id 是 CHAR(36) / CHAR(64) 的主键，超长的一定不是本库发出来的游标。
	if len(c.ID) > 64 {
		return cursor{}, false
	}
	seq, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return cursor{}, false
	}
	c.Seq = seq

	if c.At.IsZero() && c.ID == "" && c.Seq == 0 {
		return cursor{}, false
	}
	return c, true
}

// encodeTimeCursor 编一个 (created_at, id) 复合锚点。
//
// 单独提出来是因为各仓储里 cursor 这个名字被形参占着（List 的参数就叫 cursor），
// 在方法体里没法再写 cursor{...} 这样的复合字面量。
func encodeTimeCursor(at time.Time, id string) string {
	return encodeCursor(cursor{At: at, ID: id})
}

// encodeIDCursor 编一个纯 id 锚点，用于按 UUIDv7 主键单列排序的列表。
func encodeIDCursor(id string) string {
	return encodeCursor(cursor{ID: id})
}

// pageSlice 按"多取一条"的结果切出这一页，并报告是否还有下一页。
//
// 多取一条而不是再发一次 COUNT：COUNT 在这些表上要重扫一遍索引，
// 而"下一页有没有内容"只需要知道第 limit+1 条存不存在。
func pageSlice[T any](items []T, limit int) (page []T, hasMore bool) {
	if len(items) > limit {
		return items[:limit], true
	}
	return items, false
}
