import { useEffect, useRef, useState } from 'react';
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
  // 两个都默认关：拼接这一步不该悄悄改动素材。默认静音会把上一步好不容易生成的
  // 同步人声整条抹掉，默认挂字幕则会给一堆没台词的片段拼上一条空轨。
  const [mute, setMute] = useState(false);
  const [subtitles, setSubtitles] = useState(false);
  const barRef = useRef<HTMLDivElement>(null);

  const picked = picks.map((id) => cards.find((c) => c.id === id)).filter((c): c is CanvasCard => Boolean(c));
  // 没有产物的卡片拼不进去（后端会逐个报出来），提交前就先说清楚是哪几张，
  // 免得用户点了合成才知道自己选中了一张还在转圈的卡。
  const notReady = picked.filter((c) => !c.asset_id);
  const task = taskId ? tasks?.find((t) => t.id === taskId) : undefined;

  /**
   * 滚轮手动接回来，理由与坞同源但落点不同：坞让出指针的是内层 .dock-body、滚的
   * 也是它；合成条没有内层滚动体，`overflow-y:auto` 就在让出指针的那个容器自己
   * 身上。所以判据不能按「事件目标在不在条内」分，得整块矩形一律接管——
   * useViewport 的 wheel 监听对落在画布视口里的滚轮无条件 `preventDefault()` 去平移
   * 画布，浏览器的原生滚动早就被它吃掉了，条上哪一处都指望不上。
   *
   * 挂在画布视口上、走捕获阶段：视口自己的监听是冒泡阶段的，捕获先跑才拦得住它。
   */
  useEffect(() => {
    const bar = barRef.current;
    const viewport = bar?.closest('.canvas-viewport');
    if (!bar || !viewport) return;
    const onWheel = (e: Event) => {
      const wheel = e as WheelEvent;
      const at = bar.getBoundingClientRect();
      const inside =
        wheel.clientX >= at.left && wheel.clientX <= at.right && wheel.clientY >= at.top && wheel.clientY <= at.bottom;
      if (!inside || bar.scrollHeight <= bar.clientHeight) return;
      wheel.preventDefault();
      wheel.stopPropagation();
      bar.scrollTop += wheel.deltaY;
    };
    viewport.addEventListener('wheel', onWheel, { capture: true, passive: false });
    return () => viewport.removeEventListener('wheel', onWheel, { capture: true });
  }, []);

  async function submit(): Promise<void> {
    setBusy(true);
    try {
      const res = await api.compose(projectId, picks, composeTitle(picked), crypto.randomUUID(), {
        mute,
        subtitles,
      });
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
    <div className="compose-bar" ref={barRef}>
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
          {/*
            这两个开关只作用于**拼接这一次**，不重出任何一段：静音是把已经编进
            各段视频里的声音在拼接时丢掉（-an），字幕是按各镜台词与各段实际时长
            新排一条软字幕轨挂上去。想让某一镜的成片本身没有人声，得回上一步
            关掉那一镜的人声再重出——那是生成时就定死的事。
          */}
          <div className="compose-options">
            <label className="flow-field" htmlFor="compose-mute">
              <input
                id="compose-mute"
                type="checkbox"
                checked={mute}
                disabled={busy}
                onChange={(e) => setMute(e.target.checked)}
              />
              去掉声音
            </label>
            <label className="flow-field" htmlFor="compose-subtitles">
              <input
                id="compose-subtitles"
                type="checkbox"
                checked={subtitles}
                disabled={busy}
                onChange={(e) => setSubtitles(e.target.checked)}
              />
              挂字幕轨
            </label>
            <span className="hint">
              {subtitles
                ? '字幕直接取各镜卡片上的台词原文，按各段实际时长排时间轴；是可关闭的软字幕轨，不烧进画面'
                : '字幕取各镜台词原文，不做语音转写'}
            </span>
          </div>
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
