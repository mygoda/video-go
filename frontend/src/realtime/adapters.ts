import type { StreamEvent } from '@/api/types';
import { API_BASE, USE_MOCK } from '@/api/client';
import { api } from '@/api/endpoints';
import { getToken } from '@/api/token';
import { subscribe as subscribeMock } from '@/mock/stream';

export type StreamState = 'connecting' | 'open' | 'closed';

export interface StreamHandlers {
  onEvent(e: StreamEvent): void;
  onStateChange(s: StreamState): void;
  /** 连续重连失败达到阈值：上层据此降级到轮询 */
  onGiveUp(): void;
}

export interface TaskStreamAdapter {
  connect(handlers: StreamHandlers): void;
  close(): void;
}

const MAX_RETRIES = 3;

/**
 * SSE 适配器。原生 EventSource 不能带 Authorization header，而 token 在 header 里，
 * 所以按 frontend-design.md §4.3 手写 fetch + ReadableStream 解析（不引第三方依赖）。
 */
export function createSseAdapter(): TaskStreamAdapter {
  let controller: AbortController | null = null;
  let closed = false;
  let retries = 0;
  let lastEventId: string | null = null;
  let timer: number | undefined;

  async function run(handlers: StreamHandlers): Promise<void> {
    if (closed) return;
    handlers.onStateChange('connecting');
    controller = new AbortController();

    try {
      const headers: Record<string, string> = { Accept: 'text/event-stream' };
      const token = getToken();
      if (token) headers.Authorization = `Bearer ${token}`;
      if (lastEventId) headers['Last-Event-ID'] = lastEventId;

      const res = await fetch(`${API_BASE}/stream`, { headers, signal: controller.signal });
      if (!res.ok || !res.body) throw new Error(`stream ${res.status}`);

      retries = 0;
      handlers.onStateChange('open');

      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buffer = '';

      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });

        let split = buffer.indexOf('\n\n');
        while (split !== -1) {
          const frame = buffer.slice(0, split);
          buffer = buffer.slice(split + 2);
          const parsed = parseFrame(frame);
          if (parsed) {
            if (parsed.id) lastEventId = parsed.id;
            handlers.onEvent(parsed);
          }
          split = buffer.indexOf('\n\n');
        }
      }
      throw new Error('stream ended');
    } catch (err) {
      if (closed || (err as Error).name === 'AbortError') return;
      handlers.onStateChange('closed');
      retries += 1;
      if (retries >= MAX_RETRIES) {
        handlers.onGiveUp();
        return;
      }
      const backoff = Math.min(1000 * 2 ** retries, 15_000);
      timer = window.setTimeout(() => void run(handlers), backoff);
    }
  }

  return {
    connect(handlers) {
      closed = false;
      void run(handlers);
    },
    close() {
      closed = true;
      window.clearTimeout(timer);
      controller?.abort();
    },
  };
}

function parseFrame(frame: string): StreamEvent | null {
  let event = '';
  let id: string | undefined;
  const dataLines: string[] = [];

  for (const line of frame.split('\n')) {
    if (line.startsWith(':')) continue;
    const idx = line.indexOf(':');
    const field = idx === -1 ? line : line.slice(0, idx);
    const value = idx === -1 ? '' : line.slice(idx + 1).trimStart();
    if (field === 'event') event = value;
    else if (field === 'id') id = value;
    else if (field === 'data') dataLines.push(value);
  }

  if (!event) return null;
  try {
    return { event: event as StreamEvent['event'], id, data: JSON.parse(dataLines.join('\n') || '{}') };
  } catch {
    return null;
  }
}

/** mock 模式下的等价通道：直接订阅内存事件总线，同样支持 Last-Event-ID 补数 */
export function createMockAdapter(): TaskStreamAdapter {
  let unsubscribe: (() => void) | null = null;
  let lastEventId: string | null = null;

  return {
    connect(handlers) {
      handlers.onStateChange('connecting');
      unsubscribe = subscribeMock(lastEventId, (e) => {
        if (e.id) lastEventId = e.id;
        handlers.onEvent(e);
      });
      handlers.onStateChange('open');
    },
    close() {
      unsubscribe?.();
      unsubscribe = null;
    },
  };
}

/**
 * 轮询兜底（frontend-design.md §4.3）。只在 SSE 连续重连失败后启用，
 * 5s 拉一次活跃任务并合成事件，让上层逻辑与 SSE 完全一致。
 */
export function createPollingAdapter(): TaskStreamAdapter {
  let timer: number | undefined;
  let stopped = false;
  const seen = new Map<string, string>();

  async function tick(handlers: StreamHandlers): Promise<void> {
    if (stopped) return;
    try {
      const tasks = await api.tasks();
      for (const t of tasks) {
        const stamp = `${t.status}:${t.progress ?? ''}`;
        if (seen.get(t.id) === stamp) continue;
        seen.set(t.id, stamp);
        if (t.status === 'succeeded') {
          handlers.onEvent({ event: 'task.succeeded', data: { task_id: t.id, assets: t.assets as never } });
        } else if (t.status === 'failed') {
          handlers.onEvent({ event: 'task.failed', data: { task_id: t.id, error: t.error as never } });
        } else {
          handlers.onEvent({
            event: 'task.updated',
            data: {
              task_id: t.id,
              status: t.status,
              progress: t.progress,
              queue_position: t.queue_position,
              eta_seconds: t.eta_seconds,
            },
          });
        }
      }
      handlers.onStateChange('open');
    } catch {
      handlers.onStateChange('closed');
    }
    timer = window.setTimeout(() => void tick(handlers), 5000);
  }

  return {
    connect(handlers) {
      stopped = false;
      handlers.onStateChange('connecting');
      void tick(handlers);
    },
    close() {
      stopped = true;
      window.clearTimeout(timer);
    },
  };
}

export function createPrimaryAdapter(): TaskStreamAdapter {
  return USE_MOCK ? createMockAdapter() : createSseAdapter();
}
