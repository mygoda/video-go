import { createContext, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import type { Asset, Task, StreamEvent } from '@/api/types';
import { qk, mergeTask, patchMe } from '@/api/queries';
import { api } from '@/api/endpoints';
import { createPollingAdapter, createPrimaryAdapter, type StreamState, type TaskStreamAdapter } from './adapters';

interface StreamContextValue {
  state: StreamState;
  /** SSE 连续失败后走轮询：顶栏据此显示一个不打扰的小标 */
  degraded: boolean;
}

const StreamContext = createContext<StreamContextValue>({ state: 'closed', degraded: false });

export function useStreamStatus(): StreamContextValue {
  return useContext(StreamContext);
}

const RETRY_PRIMARY_MS = 60_000;

/**
 * 全局事件通道（frontend-design.md §1.3 / §6.2）。挂在路由之外：任务在生成器提交、
 * 可能在画布页完成，连接不能随页面卸载而断。
 *
 * 事件只用 setQueryData 改缓存，不触发 refetch —— 一次生成十几条事件，refetch 会把后端打穿。
 */
export function TaskStreamProvider({ enabled, children }: { enabled: boolean; children: ReactNode }) {
  const qc = useQueryClient();
  const [state, setState] = useState<StreamState>('closed');
  const [degraded, setDegraded] = useState(false);

  const adapterRef = useRef<TaskStreamAdapter | null>(null);
  const retryTimer = useRef<number | undefined>(undefined);

  useEffect(() => {
    if (!enabled) {
      adapterRef.current?.close();
      adapterRef.current = null;
      setState('closed');
      setDegraded(false);
      return;
    }

    let disposed = false;

    /** 重连/降级后全量对账：断线期间错过的事件靠这一把补齐 */
    async function reconcile(): Promise<void> {
      try {
        const active = await api.activeTasks();
        if (disposed) return;
        qc.setQueryData<Task[]>(qk.tasks, (prev) => {
          if (!prev) return prev;
          const byId = new Map(active.map((t) => [t.id, t]));
          return prev.map((t) => byId.get(t.id) ?? t);
        });
      } catch {
        // 对账失败不影响主通道，下一次重连还会再试
      }
    }

    function handleEvent(e: StreamEvent): void {
      const taskId = e.data.task_id;
      switch (e.event) {
        case 'task.updated':
          if (!taskId) return;
          mergeTask(qc, {
            id: taskId,
            status: e.data.status ?? 'running',
            progress: e.data.progress ?? null,
            queue_position: e.data.queue_position ?? null,
            eta_seconds: e.data.eta_seconds ?? null,
            local: undefined,
          });
          return;
        case 'task.succeeded': {
          if (!taskId) return;
          const assets = (e.data.assets ?? []) as unknown as Asset[];
          mergeTask(qc, {
            id: taskId,
            status: 'succeeded',
            progress: 1,
            assets,
            error: null,
            local: undefined,
          });
          void qc.invalidateQueries({ queryKey: ['assets'] });
          return;
        }
        case 'task.failed':
          if (!taskId) return;
          mergeTask(qc, {
            id: taskId,
            status: 'failed',
            error: (e.data.error ?? null) as Task['error'],
            local: undefined,
          });
          return;
        case 'credit.updated':
          if (typeof e.data.balance === 'number') patchMe(qc, { credits: e.data.balance });
          return;
        case 'canvas.card.updated': {
          const canvasId = e.data.canvas_id;
          if (!canvasId) return;
          void qc.invalidateQueries({ queryKey: qk.canvas(canvasId) });
          return;
        }
        default:
          // ping 以及后端将来新增的事件：忽略而不是报错
          return;
      }
    }

    function start(adapter: TaskStreamAdapter, isFallback: boolean): void {
      adapterRef.current?.close();
      adapterRef.current = adapter;
      setDegraded(isFallback);
      adapter.connect({
        onEvent: handleEvent,
        onStateChange: (s) => {
          if (disposed) return;
          setState(s);
          if (s === 'open') void reconcile();
        },
        onGiveUp: () => {
          if (disposed) return;
          start(createPollingAdapter(), true);
          // 降级不是终点：网络恢复后自己爬回 SSE
          retryTimer.current = window.setTimeout(() => {
            if (!disposed) start(createPrimaryAdapter(), false);
          }, RETRY_PRIMARY_MS);
        },
      });
    }

    start(createPrimaryAdapter(), false);
    const onUnload = () => adapterRef.current?.close();
    window.addEventListener('beforeunload', onUnload);

    return () => {
      disposed = true;
      window.removeEventListener('beforeunload', onUnload);
      window.clearTimeout(retryTimer.current);
      adapterRef.current?.close();
      adapterRef.current = null;
    };
  }, [enabled, qc]);

  const value = useMemo(() => ({ state, degraded }), [state, degraded]);
  return <StreamContext.Provider value={value}>{children}</StreamContext.Provider>;
}
