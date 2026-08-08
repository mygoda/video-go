import { useState } from 'react';
import type { JsonValue, ModelCapabilitySchema } from '@/schema/types';
import type { FormValues, InputValues } from '@/schema/evaluate';
import { groupParams } from '@/schema/form';
import type { FieldErrors } from '@/schema/validate';
import { Popover } from '../Popover';
import { ParamChip } from './ParamChip';
import { ParamEditor } from './ParamEditor';
import { ModelChip } from './ModelChip';

interface ParamChipBarProps {
  models: ModelCapabilitySchema[];
  model: ModelCapabilitySchema;
  values: FormValues;
  inputs: InputValues;
  errors: FieldErrors;
  migrated: string[];
  onSelectModel(model: ModelCapabilitySchema): void;
  onChange(key: string, value: JsonValue): void;
  children?: React.ReactNode;
}

/**
 * 参数芯片条。整条由 capability schema 渲染 —— 这里出现任何 `model.id === 'xxx'`
 * 都意味着「接新模型前端零改动」这条验收线破了。
 */
export function ParamChipBar({
  models,
  model,
  values,
  inputs,
  errors,
  migrated,
  onSelectModel,
  onChange,
  children,
}: ParamChipBarProps) {
  const [advOpen, setAdvOpen] = useState(false);
  const primary = groupParams(model, values, inputs, 'primary');
  const advanced = groupParams(model, values, inputs, 'advanced');
  const advancedHasError = advanced.some((s) => errors[s.key]);

  return (
    <div className="chipbar">
      <ModelChip models={models} current={model} onSelect={onSelectModel} />

      {primary.map((spec) => (
        <ParamChip
          key={spec.key}
          spec={spec}
          values={values}
          inputs={inputs}
          error={errors[spec.key]}
          migrated={migrated.includes(spec.key)}
          onChange={onChange}
        />
      ))}

      {advanced.length > 0 && (
        <Popover
          open={advOpen}
          onClose={() => setAdvOpen(false)}
          trigger={
            <button
              type="button"
              className={`chip icon-only${advancedHasError ? ' error' : ''}`}
              aria-label="高级参数"
              aria-haspopup="dialog"
              aria-expanded={advOpen}
              onClick={() => setAdvOpen((v) => !v)}
            >
              ⚙
            </button>
          }
        >
          <div className="adv-panel popover-scroll">
            <div className="popover-title">高级参数</div>
            {advanced.map((spec) => (
              <div key={spec.key} className={`adv-field${errors[spec.key] ? ' error' : ''}`}>
                <label htmlFor={`adv-${spec.key}`}>{spec.label}</label>
                <div id={`adv-${spec.key}`}>
                  <ParamEditor spec={spec} values={values} inputs={inputs} onChange={onChange} />
                </div>
                {errors[spec.key] && <p className="field-error">{errors[spec.key]}</p>}
              </div>
            ))}
          </div>
        </Popover>
      )}

      <div className="chipbar-right">{children}</div>
    </div>
  );
}
