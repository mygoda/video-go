import { useState } from 'react';
import type { CanvasCard } from '@/api/types';
import type { FlowStepState } from './flow';
import { currentStep, DEFAULT_SHOTS, flowSteps, MAX_SHOTS, shotCountError } from './flow';

const STATE_TEXT: Record<FlowStepState, string> = {
  done: '已完成',
  current: '进行中',
  locked: '未解锁',
};

interface FlowStatusBarProps {
  script: CanvasCard | null;
  shots: CanvasCard[];
  /** 拆分镜在途。这一步是同步接口，一次只能有一个在飞 */
  busy: boolean;
  /** 批量出首帧在途 */
  firstFrameBusy: boolean;
  /** 点一次「出首帧」会排上几镜：没有首帧、且写了镜头描述的那些 */
  firstFramePending: number;
  /** 这一批要花多少积分。模型目录还没拉到时为 null */
  firstFrameCost: number | null;
  onStoryboard(count: number): void;
  onFirstFrames(): void;
  onAddShot(): void;
  onRemoveShot(): void;
  onResizeShots(count: number): void;
}

/**
 * 创作流程条：这条链路一共几步、现在停在哪一步、下一步要点哪里。
 *
 * 放在画布上方而不是底部：底部那一槽已经被对话坞与合成条占满，而「走到哪儿了」
 * 必须一直看得见，不能被别的浮层轮流盖掉。
 *
 * **每一步都停在这里等用户点。** 界面上没有任何一处会自己把流程往下推——
 * 剧本写完不会自动拆分镜，分镜拆完不会自动出首帧，首帧出完不会自动出片。
 * 这是这块 UI 存在的全部理由：让「过程」成为一个能看见、能改、能重来的对象，
 * 而不是一条跑到底的流水线。
 */
export function FlowStatusBar({
  script,
  shots,
  busy,
  firstFrameBusy,
  firstFramePending,
  firstFrameCost,
  onStoryboard,
  onFirstFrames,
  onAddShot,
  onRemoveShot,
  onResizeShots,
}: FlowStatusBarProps) {
  const steps = flowSteps(script, shots);
  const step = currentStep(script, shots);

  return (
    <nav className="flow-bar" aria-label="创作流程">
      <ol className="flow-steps">
        {steps.map((s, i) => (
          <li key={s.id} className={`flow-step ${s.state}`} aria-current={s.state === 'current' ? 'step' : undefined}>
            <span className="flow-dot" aria-hidden="true">
              {s.state === 'done' ? '✓' : i + 1}
            </span>
            {s.label}
            <span className="sr-only">（{STATE_TEXT[s.state]}）</span>
          </li>
        ))}
      </ol>

      <div className="flow-action">
        {step === 'idea' ? (
          <span className="hint">在右下角说一句你想拍什么，它会先写成一份剧本 —— 每一步都停下来等你点，系统不会自己往下跑</span>
        ) : step === 'script' ? (
          <StoryboardAction busy={busy} onStoryboard={onStoryboard} />
        ) : (
          <ShotAction
            shotCount={shots.length}
            firstFrameBusy={firstFrameBusy}
            firstFramePending={firstFramePending}
            firstFrameCost={firstFrameCost}
            onFirstFrames={onFirstFrames}
            onAddShot={onAddShot}
            onRemoveShot={onRemoveShot}
            onResizeShots={onResizeShots}
          />
        )}
      </div>
    </nav>
  );
}

/** 剧本这一步的停顿点：先决定拆几个镜头，再由用户点「继续」。 */
function StoryboardAction({ busy, onStoryboard }: Pick<FlowStatusBarProps, 'busy' | 'onStoryboard'>) {
  const [text, setText] = useState(String(DEFAULT_SHOTS));
  const count = Number(text);
  const error = shotCountError(count);

  return (
    <>
      <label className="flow-field" htmlFor="flow-shot-count">
        拆成
        <input
          id="flow-shot-count"
          className="num-input"
          type="number"
          min={1}
          max={MAX_SHOTS}
          step={1}
          value={text}
          aria-invalid={error !== null}
          aria-describedby="flow-action-hint"
          onChange={(e) => setText(e.target.value)}
        />
        个镜头
      </label>
      <button
        type="button"
        className="btn btn-sm btn-primary"
        disabled={busy || error !== null}
        onClick={() => onStoryboard(count)}
      >
        {busy ? '拆分镜中…' : '继续：拆分镜'}
      </button>
      <span className={error ? 'hint danger' : 'hint'} id="flow-action-hint" role={error ? 'alert' : undefined}>
        {error ?? '改完剧本再点，拆分镜只出文字，不花钱'}
      </span>
    </>
  );
}

interface ShotActionProps
  extends Pick<
    FlowStatusBarProps,
    'firstFrameBusy' | 'firstFramePending' | 'firstFrameCost' | 'onFirstFrames' | 'onAddShot' | 'onRemoveShot' | 'onResizeShots'
  > {
  shotCount: number;
}

/**
 * 分镜与首帧这一步：加镜 / 删镜 / 调镜数，以及批量出首帧。
 *
 * 两步共用一组控件，因为它们作用在同一批镜头卡上：加完一镜，这条线就从
 * 「首帧都出好了」退回「还差一镜」，控件不该跟着换一套。
 *
 * 「调到 N」的输入框平时跟着实际镜头数走，用户一动才转成他自己的草稿
 * （draft 非 null）；应用完再交还。少了这一步，点完加镜后输入框还停在旧数字，
 * 再点「应用」就会把刚加的那一镜删回去。
 */
function ShotAction({
  shotCount,
  firstFrameBusy,
  firstFramePending,
  firstFrameCost,
  onFirstFrames,
  onAddShot,
  onRemoveShot,
  onResizeShots,
}: ShotActionProps) {
  const [draft, setDraft] = useState<string | null>(null);
  const text = draft ?? String(shotCount);
  const target = Number(text);
  const error = shotCountError(target);
  const full = shotCount >= MAX_SHOTS;

  function apply(): void {
    if (error || target === shotCount) return;
    onResizeShots(target);
    setDraft(null);
  }

  return (
    <>
      <span className="flow-count mono">
        镜头 {shotCount} / {MAX_SHOTS}
      </span>
      <button
        type="button"
        className="btn btn-sm"
        disabled={shotCount <= 1}
        title={shotCount <= 1 ? '只剩一镜了，再删就没有分镜了' : '删掉最后一镜'}
        onClick={onRemoveShot}
      >
        − 删镜
      </button>
      <button
        type="button"
        className="btn btn-sm"
        disabled={full}
        title={full ? `镜头数最多 ${MAX_SHOTS}` : '在末尾加一个空镜头'}
        onClick={onAddShot}
      >
        ＋ 加镜
      </button>
      <label className="flow-field" htmlFor="flow-shot-target">
        调到
        <input
          id="flow-shot-target"
          className="num-input"
          type="number"
          min={1}
          max={MAX_SHOTS}
          step={1}
          value={text}
          aria-invalid={error !== null}
          aria-describedby="flow-action-hint"
          onChange={(e) => setDraft(e.target.value)}
        />
        镜
      </label>
      <button type="button" className="btn btn-sm" disabled={error !== null || target === shotCount} onClick={apply}>
        应用
      </button>

      {firstFrameCost !== null && firstFramePending > 0 && (
        <span className="cost">
          ⚡<span className="amount mono">{firstFrameCost}</span> 积分
        </span>
      )}
      <button
        type="button"
        className="btn btn-sm btn-primary"
        disabled={firstFrameBusy || firstFramePending === 0}
        title={
          firstFramePending === 0
            ? '每一镜都有首帧了；要换一张就选中那一镜，点卡片上的 ↻'
            : `把这 ${firstFramePending} 镜排上出图，同时最多 2 张在跑`
        }
        aria-describedby="flow-action-hint"
        onClick={onFirstFrames}
      >
        {firstFrameBusy ? '出首帧中…' : `继续：出首帧（${firstFramePending} 镜）`}
      </button>

      <span className={error ? 'hint danger' : 'hint'} id="flow-action-hint" role={error ? 'alert' : undefined}>
        {error ??
          (firstFramePending > 0
            ? full
              ? `已经到上限 ${MAX_SHOTS} 镜；出图前先把每一镜的描述写好，没写描述的镜头排不上队`
              : '先看图再出片：这一步只出图，出完停下来等你逐张确认'
            : '首帧都出好了。不满意就选中那一镜，点卡片上的 ↻ 单独重出；下一步（出片）还没放行')}
      </span>
    </>
  );
}
