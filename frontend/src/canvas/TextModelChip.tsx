import { useState } from 'react';
import { useModels } from '@/api/queries';
import { Popover } from '@/components/Popover';

interface TextModelChipProps {
  /** null = 没点名。写剧本与优化两处共用同一个语义 */
  modelId: string | null;
  onChange(modelId: string | null): void;
  disabled?: boolean;
  /** 芯片贴着屏幕底边时（对话坞）弹层得往上开，否则整个列表落到视口外 */
  dropUp?: boolean;
}

/**
 * 写剧本用哪个模型。
 *
 * 「默认」是一个真的选项，而且是初始值：不点名时请求里根本没有 model_id 这个键，
 * 后端按自己那套优先级选。前端绝不能"默认选中列表第一个再传上去"——那等于
 * 悄悄把默认模型换成了目录顺序第一的那个，而目录顺序是管理员随时能排的。
 *
 * 列表顺序直接用后端给的：GET /api/models 已按 order、同序按 name 定序下发，
 * 前端再排一遍只会多出一处会和后端走散的规则。
 */
export function TextModelChip({ modelId, onChange, disabled, dropUp }: TextModelChipProps) {
  const [open, setOpen] = useState(false);
  const { data: models } = useModels('text');
  const current = models?.find((m) => m.id === modelId);

  return (
    <Popover
      open={open}
      onClose={() => setOpen(false)}
      dropUp={dropUp}
      trigger={
        <button
          type="button"
          className="chip btn-sm"
          style={{ height: 24, padding: '0 8px', maxWidth: 150 }}
          aria-haspopup="listbox"
          aria-expanded={open}
          disabled={disabled}
          title={current ? `写剧本用 ${current.name}` : '写剧本用哪个模型'}
          onClick={() => setOpen((v) => !v)}
        >
          <span className="v" style={{ overflow: 'hidden', textOverflow: 'ellipsis' }}>
            {current?.name ?? '默认模型'}
          </span>
          <span className="caret" aria-hidden="true">
            ▾
          </span>
        </button>
      }
    >
      <div className="popover-title">写剧本的模型</div>
      <div role="listbox" className="popover-scroll" style={{ width: 240 }}>
        <button
          type="button"
          role="option"
          className="option model-option"
          aria-selected={modelId === null}
          onClick={() => {
            onChange(null);
            setOpen(false);
          }}
        >
          <span className="row1">默认模型</span>
          <span className="desc">不点名，由服务端选</span>
        </button>
        {models?.map((m) => (
          <button
            key={m.id}
            type="button"
            role="option"
            className="option model-option"
            aria-selected={m.id === modelId}
            disabled={!m.enabled}
            title={m.enabled ? undefined : m.disabled_reason}
            onClick={() => {
              onChange(m.id);
              setOpen(false);
            }}
          >
            <span className="row1">{m.name}</span>
            {(m.description || !m.enabled) && (
              <span className="desc">{m.enabled ? m.description : m.disabled_reason}</span>
            )}
          </button>
        ))}
      </div>
    </Popover>
  );
}
