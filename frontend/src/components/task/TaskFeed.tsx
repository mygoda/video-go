import { useMemo } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import type { Task } from '@/api/types';
import { api } from '@/api/endpoints';
import { qk, useTasks } from '@/api/queries';
import { toast } from '@/stores/toast';
import { TaskCard } from './TaskCard';

interface TaskFeedProps {
  modality: 'image' | 'video';
  onRetry(task: Task): void;
  onEditPrompt(task: Task): void;
}

function dayLabel(iso: string): string {
  const d = new Date(iso);
  const today = new Date();
  const diff = Math.floor(
    (new Date(today.getFullYear(), today.getMonth(), today.getDate()).getTime() -
      new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()) /
      86_400_000,
  );
  if (diff <= 0) return '今天';
  if (diff === 1) return '昨天';
  return `${d.getMonth() + 1} 月 ${d.getDate()} 日`;
}

export function TaskFeed({ modality, onRetry, onEditPrompt }: TaskFeedProps) {
  const qc = useQueryClient();
  const { data: tasks, isLoading } = useTasks();

  const groups = useMemo(() => {
    // 画布里产的任务（首帧 / 出片 / 剧本）带 canvas_id，只属于那张画布，不该
    // 混进生成器的任务流——生成器只列用户在这个页面直接发起的任务。
    const mine = (tasks ?? []).filter((t) => t.modality === modality && !t.canvas_id);
    const byDay = new Map<string, Task[]>();
    for (const t of mine) {
      const key = dayLabel(t.created_at);
      const bucket = byDay.get(key);
      if (bucket) bucket.push(t);
      else byDay.set(key, [t]);
    }
    return [...byDay.entries()];
  }, [tasks, modality]);

  async function cancel(task: Task): Promise<void> {
    try {
      await api.cancelTask(task.id);
      qc.setQueryData<Task[]>(qk.tasks, (prev) =>
        prev?.map((t) => (t.id === task.id ? { ...t, status: 'canceled' } : t)),
      );
      toast('已取消，积分已退回');
    } catch {
      toast('取消失败，任务可能已经开始', 'danger');
    }
  }

  async function dismiss(task: Task): Promise<void> {
    try {
      await api.deleteTask(task.id);
    } finally {
      qc.setQueryData<Task[]>(qk.tasks, (prev) => prev?.filter((t) => t.id !== task.id));
    }
  }

  if (isLoading) return <div className="empty">加载中…</div>;
  if (!groups.length) {
    return (
      <div className="empty">
        <div className="title">还没有作品</div>
        写一句提示词，点「生成」开始。
      </div>
    );
  }

  return (
    <div className="feed-wrap">
      {groups.map(([day, items]) => (
        <section key={day}>
          <div className="feed-header">{day}</div>
          <div className="task-grid">
            {items.map((t) => (
              <TaskCard
                key={t.client_token ?? t.id}
                task={t}
                onCancel={(x) => void cancel(x)}
                onRetry={onRetry}
                onEditPrompt={onEditPrompt}
                onDismiss={(x) => void dismiss(x)}
              />
            ))}
          </div>
        </section>
      ))}
    </div>
  );
}
