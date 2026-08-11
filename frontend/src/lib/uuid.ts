/**
 * crypto.randomUUID 只在安全上下文（HTTPS 或 localhost）里存在。本项目按 IP + 明文
 * HTTP 部署时它是 undefined，直接调用会抛错并让提交流程静默卡死。
 * crypto.getRandomValues 在非安全上下文照样可用，用它拼一个 v4 UUID 兜底。
 */
export function uuid(): string {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID();
  const b = crypto.getRandomValues(new Uint8Array(16));
  b[6] = (b[6] & 0x0f) | 0x40; // version 4
  b[8] = (b[8] & 0x3f) | 0x80; // variant 10
  const h = Array.from(b, (x) => x.toString(16).padStart(2, '0'));
  return `${h[0]}${h[1]}${h[2]}${h[3]}-${h[4]}${h[5]}-${h[6]}${h[7]}-${h[8]}${h[9]}-${h[10]}${h[11]}${h[12]}${h[13]}${h[14]}${h[15]}`;
}
