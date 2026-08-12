import { useRef, useState } from 'react';
import type { CanvasCard } from '@/api/types';
import { api } from '@/api/endpoints';
import { toast } from '@/stores/toast';
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
  /** 批量出片在途 */
  renderBusy: boolean;
  /** 点一次「出片」会排上几镜：已有首帧、还没有成片的那些 */
  renderPending: number;
  /** 这一批出片要花多少积分。模型目录还没拉到时为 null */
  renderCost: number | null;
  /** 没有单独表过态的镜头，台词进不进出片 prompt */
  voiceDefault: boolean;
  onStoryboard(count: number, imageUploadId?: string): void;
  onFirstFrames(): void;
  onRenders(): void;
  onVoiceDefault(on: boolean): void;
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
 * 剧本写完不会自动拆分镜，分镜拆完不会自动出首帧，首帧出完不会自动出片，
 * 出完片也不会自己去合成。这是这块 UI 存在的全部理由：让「过程」成为一个
 * 能看见、能改、能重来的对象，而不是一条跑到底的流水线。
 */
export function FlowStatusBar({
  script,
  shots,
  busy,
  firstFrameBusy,
  firstFramePending,
  firstFrameCost,
  renderBusy,
  renderPending,
  renderCost,
  voiceDefault,
  onStoryboard,
  onFirstFrames,
  onRenders,
  onVoiceDefault,
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
            renderBusy={renderBusy}
            renderPending={renderPending}
            renderCost={renderCost}
            voiceDefault={voiceDefault}
            onFirstFrames={onFirstFrames}
            onRenders={onRenders}
            onVoiceDefault={onVoiceDefault}
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
  // 参考图：传了就走视觉模型看图拆分镜，让每一镜与图里的人物/场景/画风一致。
  const [ref, setRef] = useState<{ id: string; name: string; url: string } | null>(null);
  const [uploading, setUploading] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);
  const count = Number(text);
  const error = shotCountError(count);

  async function pickImage(e: React.ChangeEvent<HTMLInputElement>): Promise<void> {
    const file = e.target.files?.[0];
    e.target.value = ''; // 允许再次选同一个文件
    if (!file) return;
    if (!file.type.startsWith('image/')) {
      toast('参考图得是图片文件', 'danger');
      return;
    }
    setUploading(true);
    try {
      const up = await api.upload(file);
      setRef({ id: up.upload_id, name: file.name, url: up.preview_url });
    } catch {
      toast('参考图上传失败，请重试', 'danger');
    } finally {
      setUploading(false);
    }
  }

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
      <input ref={fileRef} type="file" accept="image/*" hidden onChange={pickImage} />
      {ref ? (
        <span className="flow-ref" title={ref.name}>
          <img src={ref.url} alt="" />
          参考图
          <button type="button" className="flow-ref-x" aria-label="移除参考图" onClick={() => setRef(null)}>
            ✕
          </button>
        </span>
      ) : (
        <button
          type="button"
          className="btn btn-sm"
          disabled={uploading}
          onClick={() => fileRef.current?.click()}
        >
          {uploading ? '上传中…' : '＋参考图'}
        </button>
      )}
      <button
        type="button"
        className="btn btn-sm btn-primary"
        disabled={busy || uploading || error !== null}
        onClick={() => onStoryboard(count, ref?.id)}
      >
        {busy ? '拆分镜中…' : ref ? '继续：看图拆分镜' : '继续：拆分镜'}
      </button>
      <span className={error ? 'hint danger' : 'hint'} id="flow-action-hint" role={error ? 'alert' : undefined}>
        {error ?? (ref ? '按参考图拆分镜，每镜与图保持一致；只出文字，不花钱' : '改完剧本再点，拆分镜只出文字，不花钱')}
      </span>
    </>
  );
}

interface ShotActionProps
  extends Pick<
    FlowStatusBarProps,
    | 'firstFrameBusy'
    | 'firstFramePending'
    | 'firstFrameCost'
    | 'renderBusy'
    | 'renderPending'
    | 'renderCost'
    | 'voiceDefault'
    | 'onFirstFrames'
    | 'onRenders'
    | 'onVoiceDefault'
    | 'onAddShot'
    | 'onRemoveShot'
    | 'onResizeShots'
  > {
  shotCount: number;
}

/**
 * 分镜、首帧、出片这三步：加镜 / 删镜 / 调镜数，批量出首帧，批量出片。
 *
 * 三步共用一组控件，因为它们作用在同一批镜头卡上：加完一镜，这条线就从
 * 「片都出完了」退回「还差一镜」，控件不该跟着换一套。
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
  renderBusy,
  renderPending,
  renderCost,
  voiceDefault,
  onFirstFrames,
  onRenders,
  onVoiceDefault,
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
      <button
        type="button"
        className="btn btn-sm"
        disabled={error !== null || target === shotCount}
        title={
          error !== null
            ? error
            : target === shotCount
              ? '镜头数已经是这么多了。「＋加镜」已经把镜头加好了；要一次改成别的数量，先在「调到」里填数字再点应用'
              : `把镜头数调整到 ${target}（多退少补）`
        }
        onClick={apply}
      >
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

      {renderCost !== null && renderPending > 0 && (
        <span className="cost">
          ⚡<span className="amount mono">{renderCost}</span> 积分
        </span>
      )}
      {/*
        人声开关放在出片按钮旁边而不是合成条上：它决定的是**这次生成**的成片里
        有没有人在说话，改了必须重出才生效。摆到合成那一步会让人以为拼接时还能
        反悔——而那时候声音早就编进每一段视频里了。
      */}
      <label className="flow-field" htmlFor="flow-voice-default">
        <input
          id="flow-voice-default"
          type="checkbox"
          checked={voiceDefault}
          disabled={renderBusy}
          onChange={(e) => onVoiceDefault(e.target.checked)}
        />
        台词念出来
      </label>
      <button
        type="button"
        className="btn btn-sm btn-primary"
        disabled={renderBusy || renderPending === 0}
        title={
          renderPending === 0
            ? firstFramePending > 0
              ? '还有镜头没出首帧。出片要拿首帧当第一画面，没图的镜头排不上队'
              : '每一镜都出片了；要重出就选中那一镜，点卡片上的 ▶'
            : `把这 ${renderPending} 镜排上出片，首帧图作为第一画面喂进去`
        }
        aria-describedby="flow-action-hint"
        onClick={onRenders}
      >
        {renderBusy ? '出片中…' : `继续：出片（${renderPending} 镜）`}
      </button>

      <span className={error ? 'hint danger' : 'hint'} id="flow-action-hint" role={error ? 'alert' : undefined}>
        {error ??
          (firstFramePending > 0
            ? full
              ? `已经到上限 ${MAX_SHOTS} 镜；出图前先把每一镜的描述写好，没写描述的镜头排不上队`
              : '先看图再出片：这一步只出图，出完停下来等你逐张确认'
            : renderPending > 0
              ? voiceDefault
                ? '出片按首帧生成，台词会念出来。一条一分多钟，出完停下来等你逐条看'
                : '出片按首帧生成，台词不念、只有环境音。单镜想单独开口，去那一镜的编辑器里改'
              : '每一镜都有成片了。不满意就选中那一镜，点卡片上的 ▶ 单独重出；要拼成一条片子，点顶栏的「🎬 合成」')}
      </span>
    </>
  );
}
