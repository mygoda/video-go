import type { CanvasCard } from '@/api/types';
import { readVersions, type ScriptVersion } from './script';

interface ScriptVersionsPanelProps {
  card: CanvasCard;
  onRestore(version: ScriptVersion): void;
  onClose(): void;
}

function when(at: string): string {
  const t = Date.parse(at);
  return Number.isNaN(t) ? '时间未知' : new Date(t).toLocaleString();
}

function excerpt(text: string): string {
  const flat = text.trim().replace(/\s+/g, ' ');
  return flat.length > 90 ? `${flat.slice(0, 90)}…` : flat || '（空）';
}

/**
 * 剧本版本列表。最新的在上面，点「回退」把那一版的正文放回当前。
 *
 * 回退也是一次正文修改，服务端照样给它留一版，所以回错了还能再回来 ——
 * 版本历史里最怕的就是"回退把现在这一版吃掉了"。
 */
export function ScriptVersionsPanel({ card, onRestore, onClose }: ScriptVersionsPanelProps) {
  const versions = readVersions(card);

  return (
    <div className="popover" style={{ position: 'absolute', right: 'var(--space-4)', top: 'var(--space-4)', width: 380 }}>
      <div className="popover-title">版本历史 · 共 {versions.length + 1} 版</div>
      <div className="popover-scroll">
        <div className="version-row current">
          <div className="version-meta">
            <strong>当前</strong>
          </div>
          <div className="version-text">{excerpt(card.text ?? '')}</div>
        </div>
        {versions
          .slice()
          .reverse()
          .map((v) => (
            <div className="version-row" key={`${v.rev}-${v.at}`}>
              <div className="version-meta">
                <span className="mono">第 {v.rev} 版</span>
                <span className="hint">{when(v.at)}</span>
                <button type="button" className="btn btn-sm" onClick={() => onRestore(v)}>
                  回退
                </button>
              </div>
              <div className="version-text">{excerpt(v.text)}</div>
            </div>
          ))}
        {versions.length === 0 && (
          <div className="hint" style={{ padding: 'var(--space-2)' }}>
            还没有改过，保存一次就会留下上一版。
          </div>
        )}
      </div>
      <div className="popover-foot">
        <button type="button" className="btn btn-sm" onClick={onClose}>
          关闭
        </button>
      </div>
    </div>
  );
}
