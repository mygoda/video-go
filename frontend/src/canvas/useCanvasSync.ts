import { useCallback, useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import type { CanvasOp, CanvasState } from '@/api/types';
import { api } from '@/api/endpoints';
import { qk } from '@/api/queries';
import { toast } from '@/stores/toast';

const DEBOUNCE_MS = 500;

export type SaveState = 'saved' | 'pending' | 'error';

/**
 * op 队列。拖一张卡会产生几十个中间位置，不能一个位置一个请求；
 * 攒够 500ms 再发，冲突了（409）就以服务端为准重取（frontend-design.md §5.6）。
 */
export function useCanvasSync(projectId: string) {
  const qc = useQueryClient();
  const pending = useRef<CanvasOp[]>([]);
  const timer = useRef<number | undefined>(undefined);
  const inflight = useRef(false);
  const [saveState, setSaveState] = useState<SaveState>('saved');

  const flush = useCallback(async (): Promise<void> => {
    if (inflight.current || !pending.current.length) return;
    const state = qc.getQueryData<CanvasState>(qk.canvas(projectId));
    if (!state) return;

    const ops = pending.current;
    pending.current = [];
    inflight.current = true;
    try {
      const res = await api.patchCanvas(projectId, state.revision, ops);
      qc.setQueryData<CanvasState>(qk.canvas(projectId), (prev) =>
        prev ? { ...prev, revision: res.revision } : prev,
      );
      setSaveState(pending.current.length ? 'pending' : 'saved');
    } catch {
      // 版本冲突或网络问题：本地乐观态已经不可信，拉一份权威状态回来
      await qc.invalidateQueries({ queryKey: qk.canvas(projectId) });
      setSaveState('error');
      toast('画布有更新，已重新同步', 'danger');
    } finally {
      inflight.current = false;
      if (pending.current.length) void flush();
    }
  }, [projectId, qc]);

  const enqueue = useCallback(
    (ops: CanvasOp[], optimistic: (prev: CanvasState) => CanvasState) => {
      qc.setQueryData<CanvasState>(qk.canvas(projectId), (prev) => (prev ? optimistic(prev) : prev));
      pending.current.push(...ops);
      setSaveState('pending');
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => void flush(), DEBOUNCE_MS);
    },
    [projectId, qc, flush],
  );

  useEffect(() => () => window.clearTimeout(timer.current), []);

  return { enqueue, flush, saveState };
}
