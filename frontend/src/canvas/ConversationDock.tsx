import { useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import type { CanvasCard, CanvasMessage } from '@/api/types';
import { cardTitle } from './cardTitle';
import { TextModelChip } from './TextModelChip';
import { api } from '@/api/endpoints';
import { ApiError } from '@/api/client';
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
  /**
   * 交出坞的根节点。坞是不透明的常驻浮层，卡片工具条落到它底下就一个按钮都
   * 点不到，画布页得量出这块矩形才能让工具条绕开——尺寸由外面量，理由同
   * useElementSize：坞是条件渲染的，收 ref 对象拿到的会是 null。
   */
  rootRef(node: HTMLElement | null): void;
  onRemoveRef(cardId: string): void;
}

/** 对话与画布同屏：产出落到画布这件事，看不见就等于没发生 */
export function ConversationDock({
  projectId,
  conversation,
  refCards,
  collapsed,
  onCollapsedChange,
  rootRef,
  onRemoveRef,
}: ConversationDockProps) {
  const [text, setText] = useState('');
  const [skillId, setSkillId] = useState<string | null>(null);
  const [skillOpen, setSkillOpen] = useState(false);
  // 这次写剧本点名哪个模型。null 一直是初始值，见 TextModelChip：
  // 不点名时请求里不带 model_id，后端的默认行为一字不变。
  const [modelId, setModelId] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const bodyRef = useRef<HTMLDivElement>(null);
  const { data: skills } = useSkills();
  const qc = useQueryClient();

  const skill = skills?.find((s) => s.id === skillId);

  /**
   * 消息列表把指针命中让给了底下的卡片（见 components.css 里 .dock-body 那一段），
   * 代价是浏览器也不再把滚轮派给它——滚轮会直接落到画布上，用户想往回翻聊天
   * 记录，翻走的却是整块画布。所以滚轮这一件事手动接回来。
   *
   * 挂在画布视口上、且走捕获阶段：画布自己的滚轮监听是冒泡阶段的，捕获先跑，
   * 才拦得住它。不挂在列表自己身上——它 pointer-events:none，收不到任何事件。
   */
  useEffect(() => {
    const body = bodyRef.current;
    const viewport = body?.closest('.canvas-viewport');
    if (!body || !viewport) return;
    const onWheel = (e: Event) => {
      const wheel = e as WheelEvent;
      const at = body.getBoundingClientRect();
      const inside =
        wheel.clientX >= at.left && wheel.clientX <= at.right && wheel.clientY >= at.top && wheel.clientY <= at.bottom;
      if (!inside || body.scrollHeight <= body.clientHeight) return;
      wheel.preventDefault();
      wheel.stopPropagation();
      body.scrollTop += wheel.deltaY;
    };
    viewport.addEventListener('wheel', onWheel, { capture: true, passive: false });
    return () => viewport.removeEventListener('wheel', onWheel, { capture: true });
  }, [collapsed]);

  async function send(): Promise<void> {
    const value = text.trim();
    if (!value || sending) return;
    setSending(true);
    try {
      await api.canvasChat(projectId, value, refCards.map((c) => c.id), skillId, modelId);
      setText('');
      await qc.invalidateQueries({ queryKey: qk.canvas(projectId) });
    } catch (err) {
      toast(err instanceof ApiError ? err.message : '发送失败，请稍后重试', 'danger');
    } finally {
      setSending(false);
    }
  }

  return (
    <aside className="dock" ref={rootRef}>
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
          <div className="dock-body" ref={bodyRef}>
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

            {/* 技能与模型各占一颗芯片，单独一行：360px 的坞里再塞两颗进输入框，
                输入框就只剩下一百来像素，写不下一句想法。
                两颗都往上开 —— 这一行离屏幕底边只有几十像素，往下开的弹层看不见。 */}
            <div className="dock-chips">
              <Popover
                open={skillOpen}
                onClose={() => setSkillOpen(false)}
                dropUp
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

              <TextModelChip modelId={modelId} onChange={setModelId} dropUp />
            </div>

            <div className="dock-input">
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
