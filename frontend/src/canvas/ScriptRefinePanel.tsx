import { useState } from 'react';
import type { CanvasCard } from '@/api/types';
import { cardTitle } from './cardTitle';
import { TextModelChip } from './TextModelChip';
import { refinePanelPosition, type Rect } from './toolbar';
import { useElementSize, type Size } from './useElementSize';

interface ScriptRefinePanelProps {
  card: CanvasCard;
  /** 卡片工具条在视口坐标系里的矩形，面板得躲开它，否则那 5 个按钮全点不到 */
  toolbar: Rect;
  viewportSize: Size;
  /** 改写在飞。一次调用 6 秒起步，没有这个态用户会以为按钮没反应，接着再点一次 */
  busy: boolean;
  onSubmit(instruction: string, modelId: string | null): void;
  onClose(): void;
}

/**
 * 按一句指令让模型改写一张剧本卡。
 *
 * 与就地编辑是两件事：那一件是用户自己动手改字，这一件是把"改成什么样"说给
 * 模型听。两条路都会在服务端留下改动前的那一版，所以改坏了都能从版本历史切回去。
 */
export function ScriptRefinePanel({ card, toolbar, viewportSize, busy, onSubmit, onClose }: ScriptRefinePanelProps) {
  const [instruction, setInstruction] = useState('');
  const [modelId, setModelId] = useState<string | null>(null);
  // 落点要看面板自己多高：正文行数、模型芯片展开与否都会改高度，写死一个值
  // 迟早对不上，让它自己量。
  const [panelEl, setPanelEl] = useState<HTMLDivElement | null>(null);
  const panelSize = useElementSize(panelEl);
  const { left, top } = refinePanelPosition(panelSize, toolbar, viewportSize);
  const fieldId = `refine-instruction-${card.id}`;

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
      <div className="popover-title">优化 · {cardTitle(card)}</div>
      <div style={{ padding: 'var(--space-2)' }}>
        <label className="sr-only" htmlFor={fieldId}>
          修改要求
        </label>
        <textarea
          id={fieldId}
          className="textarea"
          style={{ height: 92 }}
          placeholder="想改哪儿？例如：把男主角林远改成女主角林岚，其余情节一字不动"
          value={instruction}
          autoFocus
          disabled={busy}
          onChange={(e) => setInstruction(e.target.value)}
        />
        <p className="hint" style={{ marginTop: 'var(--space-2)' }}>
          只改你指出的地方，改动前的这一版会进版本历史，随时能切回去。模型改写要几秒。
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
            {busy ? '优化中…' : '优化'}
          </button>
        </span>
      </div>
    </div>
  );
}
