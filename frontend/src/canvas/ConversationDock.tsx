import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import type { CanvasCard, CanvasMessage } from '@/api/types';
import { cardTitle } from './cardTitle';
import { api } from '@/api/endpoints';
import { qk, useSkills } from '@/api/queries';
import { Popover } from '@/components/Popover';
import { toast } from '@/stores/toast';

interface ConversationDockProps {
  projectId: string;
  conversation: CanvasMessage[];
  refCards: CanvasCard[];
  /**
   * 折叠态由画布页持有：就地编辑的编辑器和这个坞抢的是同一个右下角，
   * 开编辑器时得由外面把坞收起来。
   */
  collapsed: boolean;
  onCollapsedChange(next: boolean): void;
  onRemoveRef(cardId: string): void;
}

/** 对话与画布同屏：产出落到画布这件事，看不见就等于没发生 */
export function ConversationDock({
  projectId,
  conversation,
  refCards,
  collapsed,
  onCollapsedChange,
  onRemoveRef,
}: ConversationDockProps) {
  const [text, setText] = useState('');
  const [skillId, setSkillId] = useState<string | null>(null);
  const [skillOpen, setSkillOpen] = useState(false);
  const [sending, setSending] = useState(false);
  const { data: skills } = useSkills();
  const qc = useQueryClient();

  const skill = skills?.find((s) => s.id === skillId);

  async function send(): Promise<void> {
    const value = text.trim();
    if (!value || sending) return;
    setSending(true);
    try {
      await api.canvasChat(projectId, value, refCards.map((c) => c.id), skillId);
      setText('');
      await qc.invalidateQueries({ queryKey: qk.canvas(projectId) });
    } catch {
      toast('发送失败，请稍后重试', 'danger');
    } finally {
      setSending(false);
    }
  }

  return (
    <aside className="dock">
      <div className="dock-head">
        💬 对话
        <span style={{ marginLeft: 'auto', display: 'flex', gap: 6 }}>
          <button
            type="button"
            className="vc-btn"
            aria-label={collapsed ? '展开对话' : '折叠对话'}
            aria-expanded={!collapsed}
            onClick={() => onCollapsedChange(!collapsed)}
          >
            {collapsed ? '▴' : '▾'}
          </button>
        </span>
      </div>

      {!collapsed && (
        <>
          <div className="dock-body">
            {conversation.map((m) => (
              <div className={`msg${m.role === 'assistant' ? ' ai' : ''}`} key={m.id}>
                <span className="who">{m.role === 'assistant' ? 'AI' : '你'}</span>
                <span className="text">{m.content}</span>
              </div>
            ))}
            {!conversation.length && <p className="hint">选中画布上的卡片，它们会自动进入引用区。</p>}
          </div>

          <div className="dock-foot">
            {/* 引用 = AutoLink：选中卡片即入引用区，画布上不画任何连线 */}
            {refCards.length > 0 && (
              <div className="ref-strip">
                {refCards.map((c) => (
                  <span className="ref-chip" key={c.id}>
                    {cardTitle(c)}
                    <button
                      type="button"
                      aria-label={`取消引用 ${cardTitle(c)}`}
                      style={{ background: 'none', border: 'none', color: 'inherit', padding: 0 }}
                      onClick={() => onRemoveRef(c.id)}
                    >
                      ×
                    </button>
                  </span>
                ))}
              </div>
            )}

            <div className="dock-input">
              <Popover
                open={skillOpen}
                onClose={() => setSkillOpen(false)}
                trigger={
                  <button
                    type="button"
                    className="chip btn-sm"
                    style={{ height: 24, padding: '0 8px' }}
                    aria-haspopup="listbox"
                    aria-expanded={skillOpen}
                    onClick={() => setSkillOpen((v) => !v)}
                  >
                    <span className="v">{skill?.name ?? '通用'}</span>
                    <span className="caret" aria-hidden="true">
                      ▾
                    </span>
                  </button>
                }
              >
                <div className="popover-title">技能</div>
                <div role="listbox">
                  <button
                    type="button"
                    role="option"
                    className="option"
                    aria-selected={skillId === null}
                    onClick={() => {
                      setSkillId(null);
                      setSkillOpen(false);
                    }}
                  >
                    通用
                  </button>
                  {skills?.map((s) => (
                    <button
                      key={s.id}
                      type="button"
                      role="option"
                      className="option"
                      aria-selected={skillId === s.id}
                      onClick={() => {
                        setSkillId(s.id);
                        setSkillOpen(false);
                      }}
                    >
                      {s.name}
                    </button>
                  ))}
                </div>
              </Popover>

              <label className="sr-only" htmlFor="dock-input">
                对话输入
              </label>
              <input
                id="dock-input"
                placeholder="描述你想要的……"
                value={text}
                onChange={(e) => setText(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey) {
                    e.preventDefault();
                    void send();
                  }
                }}
              />
              <button
                type="button"
                className="btn btn-sm btn-primary"
                style={{ width: 28, padding: 0 }}
                aria-label="发送"
                disabled={sending || !text.trim()}
                onClick={() => void send()}
              >
                ↑
              </button>
            </div>
          </div>
        </>
      )}
    </aside>
  );
}
