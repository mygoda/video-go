import { useState } from 'react';
import type { ModelCapabilitySchema } from '@/schema/types';
import { Popover } from '../Popover';

interface ModelChipProps {
  models: ModelCapabilitySchema[];
  current: ModelCapabilitySchema | undefined;
  onSelect(model: ModelCapabilitySchema): void;
}

export function ModelChip({ models, current, onSelect }: ModelChipProps) {
  const [open, setOpen] = useState(false);

  const trigger = (
    <button type="button" className="chip model" aria-haspopup="listbox" aria-expanded={open} onClick={() => setOpen((v) => !v)}>
      <span className="thumb" />
      <span className="v">{current?.name ?? '选择模型'}</span>
      <span className="caret" aria-hidden="true">
        ▾
      </span>
    </button>
  );

  return (
    <Popover open={open} onClose={() => setOpen(false)} trigger={trigger}>
      <div className="popover-title">模型</div>
      <div role="listbox" className="popover-scroll" style={{ width: 268 }}>
        {models.map((m) => (
          <button
            key={m.id}
            type="button"
            role="option"
            className="option model-option"
            aria-selected={m.id === current?.id}
            disabled={!m.enabled}
            title={m.enabled ? undefined : m.disabled_reason}
            onClick={() => {
              onSelect(m);
              setOpen(false);
            }}
          >
            <span className="row1">
              <span>{m.name}</span>
              {m.id === current?.id && (
                <span className="check" style={{ marginLeft: 'auto' }} aria-hidden="true">
                  ✓
                </span>
              )}
            </span>
            {(m.description || !m.enabled) && <span className="desc">{m.enabled ? m.description : m.disabled_reason}</span>}
          </button>
        ))}
      </div>
    </Popover>
  );
}
