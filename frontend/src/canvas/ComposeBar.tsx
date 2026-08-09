import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import type { CanvasCard, Task } from '@/api/types';
import { api } from '@/api/endpoints';
import { ApiError } from '@/api/client';
import { qk, useTasks } from '@/api/queries';
import { toast } from '@/stores/toast';
import { TaskCard } from '@/components/task/TaskCard';
import { cardTitle } from './cardTitle';

interface ComposeBarProps {
  projectId: string;
  /** 有序：用户点选卡片的先后就是成片里的片段顺序 */
  picks: string[];
  cards: CanvasCard[];
  onClear(): void;
  onExit(): void;
}

/**
 * 合成任务的标题。取前两张卡的标题拼一下，剩下的用数量交代——
 * 它会落进任务的 prompt 字段，在任务监控里是这条任务唯一的辨识物，
 * 写死一句"画布合成"会让同一张画布上的三次合成长得一模一样。
 */
function composeTitle(picked: CanvasCard[]): string {
  const head = picked.slice(0, 2).map(cardTitle).join(' → ');
  return picked.length > 2 ? `${head} → …（共 ${picked.length} 段）` : head;
}

/**
 * 画布底部的一键合成条。
 *
 * 合成产出的是一条**真任务**，因此这里不自己画进度，直接复用 TaskCard：
 * 排队、生成中、失败（含未扣费标记）、成功后的产物瓦片，全部与任务流里
 * 长得一样。合成失败该有的重试按钮、退款说明也就自动都有了。
 */
export function ComposeBar({ projectId, picks, cards, onClear, onExit }: ComposeBarProps) {
  const qc = useQueryClient();
  const { data: tasks } = useTasks();
  const [taskId, setTaskId] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const picked = picks.map((id) => cards.find((c) => c.id === id)).filter((c): c is CanvasCard => Boolean(c));
  // 没有产物的卡片拼不进去（后端会逐个报出来），提交前就先说清楚是哪几张，
  // 免得用户点了合成才知道自己选中了一张还在转圈的卡。
  const notReady = picked.filter((c) => !c.asset_id);
  const task = taskId ? tasks?.find((t) => t.id === taskId) : undefined;

  async function submit(): Promise<void> {
    setBusy(true);
    try {
      const res = await api.compose(projectId, picks, composeTitle(picked), crypto.randomUUID());
      setTaskId(res.task_id);
      await qc.invalidateQueries({ queryKey: qk.tasks });
      await qc.invalidateQueries({ queryKey: qk.me });
    } catch (err) {
      toast(err instanceof ApiError ? err.message : '合成提交失败，请稍后重试', 'danger');
    } finally {
      setBusy(false);
    }
  }

  async function cancel(t: Task): Promise<void> {
    try {
      await api.cancelTask(t.id);
      await qc.invalidateQueries({ queryKey: qk.tasks });
      toast('已取消，积分已退回');
    } catch {
      toast('取消失败，任务可能已经开始', 'danger');
    }
  }

  return (
    <div className="compose-bar">
      <div className="compose-head">
        <strong>🎬 合成成片</strong>
        <span className="sub">按点选顺序拼接 · 已选 {picks.length} 段</span>
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 8 }}>
          <button type="button" className="btn btn-sm btn-ghost" disabled={!picks.length} onClick={onClear}>
            清空
          </button>
          <button type="button" className="btn btn-sm btn-ghost" onClick={onExit}>
            退出合成
          </button>
        </div>
      </div>

      {task ? (
        <div className="compose-result">
          <TaskCard
            task={task}
            onCancel={(t) => void cancel(t)}
            onRetry={() => void submit()}
            onEditPrompt={() => setTaskId(null)}
            onDismiss={() => setTaskId(null)}
          />
          {task.status === 'succeeded' && (
            <button type="button" className="btn btn-sm" onClick={() => setTaskId(null)}>
              再合成一条
            </button>
          )}
        </div>
      ) : (
        <>
          <ol className="compose-picks">
            {picked.map((c, i) => (
              <li key={c.id} className={c.asset_id ? '' : 'not-ready'}>
                <span className="mono">{i + 1}</span> {cardTitle(c)}
                {!c.asset_id && <em> · 还没有产出</em>}
              </li>
            ))}
            {!picks.length && <li className="sub">在画布上依次点选要拼接的卡片</li>}
          </ol>
          {notReady.length > 0 && (
            <div className="sub" style={{ color: 'var(--warning)' }}>
              有 {notReady.length} 张卡片还没有产出，去掉它们才能合成
            </div>
          )}
          <button
            type="button"
            className="btn btn-primary"
            disabled={busy || picks.length < 2 || notReady.length > 0}
            onClick={() => void submit()}
          >
            {busy ? '提交中…' : `合成 ${picks.length} 段为一条视频`}
          </button>
        </>
      )}
    </div>
  );
}
