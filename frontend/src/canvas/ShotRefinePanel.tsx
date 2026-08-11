import { useState } from 'react';
import type { CanvasCard } from '@/api/types';
import { cardTitle } from './cardTitle';
import { TextModelChip } from './TextModelChip';
import { refinePanelPosition } from './toolbar';
import { useElementSize, type Rect, type Size } from './useElementSize';

interface ShotRefinePanelProps {
  card: CanvasCard;
  /** 卡片工具条在视口坐标系里的矩形，面板得躲开它 */
  toolbar: Rect;
  viewportSize: Size;
  /** 改写在飞。一次调用几秒，没有这个态用户会以为按钮没反应 */
  busy: boolean;
  onSubmit(instruction: string, modelId: string | null): void;
  onClose(): void;
}

/**
 * 按一句指令让模型同时改写这一镜的画面描述与台词。与剧本卡的 ScriptRefinePanel
 * 同构，只是作用在镜头卡上；镜头正文不留版本历史（版本只对成片生效）。
 */
export function ShotRefinePanel({ card, toolbar, viewportSize, busy, onSubmit, onClose }: ShotRefinePanelProps) {
  const [instruction, setInstruction] = useState('');
  const [modelId, setModelId] = useState<string | null>(null);
  const [panelEl, setPanelEl] = useState<HTMLDivElement | null>(null);
  const panelSize = useElementSize(panelEl);
  const { left, top } = refinePanelPosition(panelSize, toolbar, viewportSize);
  const fieldId = `shot-refine-instruction-${card.id}`;

  function submit(): void {
    const value = instruction.trim();
    if (!value || busy) return;
    onSubmit(value, modelId);
  }

  return (
    <div
      ref={setPanelEl}
      className="popover"
      style={{ position: 'absolute', left, top, width: 420 }}
      onKeyDown={(e) => {
        if (e.key === 'Escape' && !busy) onClose();
        if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) submit();
      }}
    >
      <div className="popover-title">润色 · {cardTitle(card)}</div>
      <div style={{ padding: 'var(--space-2)' }}>
        <label className="sr-only" htmlFor={fieldId}>
          修改要求
        </label>
        <textarea
          id={fieldId}
          className="textarea"
          style={{ height: 92 }}
          placeholder="想改哪儿？例如：台词更冷硬一些，画面加一层雨雾，其余不动"
          value={instruction}
          autoFocus
          disabled={busy}
          onChange={(e) => setInstruction(e.target.value)}
        />
        <p className="hint" style={{ marginTop: 'var(--space-2)' }}>
          同时改这一镜的画面描述与台词，只改你指出的地方。模型改写要几秒。
        </p>
      </div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)', padding: 'var(--space-2)' }}>
        <TextModelChip modelId={modelId} onChange={setModelId} disabled={busy} />
        <span style={{ marginLeft: 'auto', display: 'flex', gap: 'var(--space-2)' }}>
          <button type="button" className="btn btn-sm" disabled={busy} onClick={onClose}>
            取消
          </button>
          <button
            type="button"
            className="btn btn-sm btn-primary"
            disabled={busy || !instruction.trim()}
            onClick={submit}
          >
            {busy ? '润色中…' : '润色'}
          </button>
        </span>
      </div>
    </div>
  );
}
