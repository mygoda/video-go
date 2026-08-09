/** 共用的展示格式化。数值单位在这里统一，页面里不各写各的。 */

export function formatBytes(bytes: number): string {
  if (bytes <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const exp = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const value = bytes / 1024 ** exp;
  return `${value >= 100 || exp === 0 ? Math.round(value) : value.toFixed(1)} ${units[exp]}`;
}

export function formatRelative(iso: string | null | undefined): string {
  if (!iso) return '—';
  const diffMinutes = Math.round((Date.now() - Date.parse(iso)) / 60_000);
  if (diffMinutes < 1) return '刚刚';
  if (diffMinutes < 60) return `${diffMinutes} 分钟前`;
  const hours = Math.round(diffMinutes / 60);
  if (hours < 24) return `${hours} 小时前`;
  return `${Math.round(hours / 24)} 天前`;
}

export function formatDuration(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined) return '—';
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.round(seconds / 60)} 分钟`;
  return `${Math.round(seconds / 360) / 10} 小时`;
}

/**
 * 视频时长角标。后端给的是毫秒（duration_ms），不是秒。
 *
 * 不知道时长时返回 null 让角标整个不出现，而不是显示 0s——「▶ 0s」看起来
 * 像一段坏掉的空视频，比没有角标误导得多。
 */
export function formatMediaDuration(ms: number | null | undefined): string | null {
  if (!ms || ms <= 0) return null;
  const seconds = ms / 1000;
  if (seconds < 10) return `${Math.round(seconds * 10) / 10}s`;
  if (seconds < 60) return `${Math.round(seconds)}s`;
  const m = Math.floor(seconds / 60);
  const s = Math.round(seconds % 60);
  return `${m}:${String(s).padStart(2, '0')}`;
}
