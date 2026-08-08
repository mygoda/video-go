import type { StreamEvent } from '@/api/types';

/**
 * mock 模式下的事件总线，替代 GET /api/stream。
 * 保留事件日志以支持 Last-Event-ID 补数 —— 这样 SSE 降级/重连逻辑在 mock 下也能被真正走到。
 */
type Listener = (e: StreamEvent) => void;

const log: StreamEvent[] = [];
const listeners = new Set<Listener>();
let seq = 10240;

export function emit(event: StreamEvent['event'], data: StreamEvent['data']): void {
  const e: StreamEvent = { event, id: String(++seq), data };
  log.push(e);
  if (log.length > 500) log.shift();
  listeners.forEach((fn) => fn(e));
}

export function subscribe(lastEventId: string | null, listener: Listener): () => void {
  if (lastEventId) {
    const from = log.findIndex((e) => Number(e.id) > Number(lastEventId));
    if (from >= 0) log.slice(from).forEach(listener);
  }
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}
