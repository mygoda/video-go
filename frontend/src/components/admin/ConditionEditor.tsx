import type { Condition, JsonPrimitive } from '@/schema/types';

const OPS: { value: Condition['op']; label: string }[] = [
  { value: 'eq', label: '等于' },
  { value: 'ne', label: '不等于' },
  { value: 'gt', label: '大于' },
  { value: 'lt', label: '小于' },
  { value: 'in', label: '属于' },
  { value: 'nin', label: '不属于' },
  { value: 'has_input', label: '输入槽有文件' },
  { value: 'and', label: '全部满足' },
  { value: 'or', label: '任一满足' },
  { value: 'not', label: '取反' },
];

/** 表单里一切都是字符串，写回 schema 前还原成 JSON 原始类型，否则条件永远比不中 */
function parsePrimitive(text: string): JsonPrimitive {
  if (text === 'true') return true;
  if (text === 'false') return false;
  if (text === 'null') return null;
  if (text !== '' && !Number.isNaN(Number(text))) return Number(text);
  return text;
}

function printPrimitive(value: JsonPrimitive): string {
  return value === null ? 'null' : String(value);
}

export const DEFAULT_CONDITION: Condition = { op: 'eq', key: '', value: '' };

/** 换 op 时尽量把已填的 key 带过去，别让管理员因为选错一次就重填 */
function convert(next: Condition['op'], current: Condition): Condition {
  const key = 'key' in current ? current.key : '';
  switch (next) {
    case 'eq':
    case 'ne':
      return { op: next, key, value: 'value' in current && !Array.isArray(current.value) ? current.value : '' };
    case 'gt':
    case 'lt':
      return { op: next, key, value: 'value' in current && typeof current.value === 'number' ? current.value : 0 };
    case 'in':
    case 'nin':
      return { op: next, key, value: 'value' in current && Array.isArray(current.value) ? current.value : [] };
    case 'has_input':
      return { op: next, slot: 'slot' in current ? current.slot : '' };
    case 'and':
    case 'or':
      return { op: next, of: 'of' in current ? (Array.isArray(current.of) ? current.of : [current.of]) : [] };
    case 'not':
      return { op: next, of: 'of' in current && !Array.isArray(current.of) ? current.of : DEFAULT_CONDITION };
  }
}

interface Props {
  value: Condition;
  onChange(next: Condition): void;
  idPrefix: string;
}

export function ConditionEditor({ value, onChange, idPrefix }: Props) {
  return (
    <div className="cond">
      <div className="cond-row">
        <label className="sr-only" htmlFor={`${idPrefix}-op`}>
          条件类型
        </label>
        <select
          id={`${idPrefix}-op`}
          className="select"
          value={value.op}
          onChange={(e) => onChange(convert(e.target.value as Condition['op'], value))}
        >
          {OPS.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>

        {'key' in value && (
          <>
            <label className="sr-only" htmlFor={`${idPrefix}-key`}>
              参数 key
            </label>
            <input
              id={`${idPrefix}-key`}
              className="input"
              placeholder="参数 key"
              value={value.key}
              onChange={(e) => onChange({ ...value, key: e.target.value })}
            />
          </>
        )}

        {'slot' in value && (
          <>
            <label className="sr-only" htmlFor={`${idPrefix}-slot`}>
              输入槽 key
            </label>
            <input
              id={`${idPrefix}-slot`}
              className="input"
              placeholder="输入槽 key"
              value={value.slot}
              onChange={(e) => onChange({ ...value, slot: e.target.value })}
            />
          </>
        )}

        {(value.op === 'eq' || value.op === 'ne') && (
          <>
            <label className="sr-only" htmlFor={`${idPrefix}-value`}>
              比较值
            </label>
            <input
              id={`${idPrefix}-value`}
              className="input"
              placeholder="值"
              value={printPrimitive(value.value)}
              onChange={(e) => onChange({ ...value, value: parsePrimitive(e.target.value) })}
            />
          </>
        )}

        {(value.op === 'gt' || value.op === 'lt') && (
          <>
            <label className="sr-only" htmlFor={`${idPrefix}-value`}>
              比较值
            </label>
            <input
              id={`${idPrefix}-value`}
              className="input"
              type="number"
              value={value.value}
              onChange={(e) => onChange({ ...value, value: Number(e.target.value) })}
            />
          </>
        )}

        {(value.op === 'in' || value.op === 'nin') && (
          <>
            <label className="sr-only" htmlFor={`${idPrefix}-value`}>
              候选值，逗号分隔
            </label>
            <input
              id={`${idPrefix}-value`}
              className="input"
              placeholder="逗号分隔"
              value={value.value.map(printPrimitive).join(',')}
              onChange={(e) =>
                onChange({
                  ...value,
                  value: e.target.value
                    .split(',')
                    .map((s) => s.trim())
                    .filter(Boolean)
                    .map(parsePrimitive),
                })
              }
            />
          </>
        )}
      </div>

      {(value.op === 'and' || value.op === 'or') && (
        <>
          {value.of.map((child, i) => (
            <ConditionEditor
              key={i}
              idPrefix={`${idPrefix}-${i}`}
              value={child}
              onChange={(next) =>
                onChange({ ...value, of: value.of.map((c, j) => (j === i ? next : c)) })
              }
            />
          ))}
          <div className="cond-row">
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => onChange({ ...value, of: [...value.of, DEFAULT_CONDITION] })}
            >
              添加子条件
            </button>
            {value.of.length > 0 && (
              <button
                type="button"
                className="btn btn-sm btn-ghost"
                onClick={() => onChange({ ...value, of: value.of.slice(0, -1) })}
              >
                删除最后一条
              </button>
            )}
          </div>
        </>
      )}

      {value.op === 'not' && (
        <ConditionEditor
          idPrefix={`${idPrefix}-not`}
          value={value.of}
          onChange={(next) => onChange({ op: 'not', of: next })}
        />
      )}
    </div>
  );
}
