import { useMemo, useState } from 'react';
import type { CanvasCard } from '@/api/types';
import { cardTitle } from './cardTitle';
import type { FormValues } from '@/schema/evaluate';
import { useModels } from '@/api/queries';
import { adoptValues } from '@/schema/migrate';
import { costBreakdown } from '@/schema/pricing';
import { ParamChipBar } from '@/components/controls/ParamChipBar';
import { useSubmitTask } from '@/hooks/useSubmitTask';
import { toast } from '@/stores/toast';

interface CardRerunPanelProps {
  projectId: string;
  card: CanvasCard;
  onClose(): void;
  onSubmitted(taskId: string): void;
}

const NO_INPUTS = {};

/**
 * 单卡重跑。参数编辑复用生成器那套芯片 —— 画布里再实现一套控件，
 * 就等于给「schema 驱动」开了第二个后门。
 */
export function CardRerunPanel({ projectId, card, onClose, onSubmitted }: CardRerunPanelProps) {
  const modality = card.kind === 'video' ? 'video' : 'image';
  const { data: models } = useModels(modality);
  const submitTask = useSubmitTask();

  const initialModel = useMemo(
    () => models?.find((m) => m.id === card.model_id) ?? models?.find((m) => m.enabled) ?? models?.[0],
    [models, card.model_id],
  );
  const [modelId, setModelId] = useState<string | null>(null);
  const model = models?.find((m) => m.id === modelId) ?? initialModel;

  const [prompt, setPrompt] = useState(card.prompt ?? '');
  const [values, setValues] = useState<FormValues | null>(null);
  const [busy, setBusy] = useState(false);

  if (!model) return null;
  const effective = values ?? adoptValues(model, card.params ?? {});
  const cost = costBreakdown(model, effective, NO_INPUTS);

  async function rerun(): Promise<void> {
    if (!model) return;
    setBusy(true);
    const res = await submitTask({
      model,
      prompt,
      values: effective,
      inputs: NO_INPUTS,
      canvasId: projectId,
      cardId: card.id,
    });
    setBusy(false);
    if (res.ok) {
      onSubmitted(res.taskId);
      toast('已开始重跑，产物会落回这张卡');
      onClose();
    } else {
      toast(res.error.message, 'danger');
    }
  }

  return (
    <div className="popover" style={{ position: 'absolute', left: 'var(--space-4)', top: 'var(--space-4)', width: 520 }}>
      <div className="popover-title">重跑 · {cardTitle(card)}</div>
      <div style={{ padding: 'var(--space-2)' }}>
        <label className="sr-only" htmlFor="rerun-prompt">
          提示词
        </label>
        <textarea
          id="rerun-prompt"
          className="textarea"
          value={prompt}
          maxLength={model.limits.prompt_max_length}
          onChange={(e) => setPrompt(e.target.value)}
        />
      </div>

      <ParamChipBar
        models={models ?? []}
        model={model}
        values={effective}
        inputs={NO_INPUTS}
        errors={{}}
        migrated={[]}
        onSelectModel={(m) => {
          setModelId(m.id);
          setValues(adoptValues(m, effective));
        }}
        onChange={(key, value) => setValues({ ...effective, [key]: value })}
      >
        <span className="cost">
          ⚡<span className="amount mono">{cost.total}</span> 积分
        </span>
        <button type="button" className="btn btn-sm" onClick={onClose}>
          取消
        </button>
        <button type="button" className="btn btn-sm btn-primary" disabled={busy || !prompt.trim()} onClick={() => void rerun()}>
          {busy ? '提交中…' : '重跑'}
        </button>
      </ParamChipBar>
    </div>
  );
}
