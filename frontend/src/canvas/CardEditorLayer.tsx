import type { CanvasCard, CharacterParams, ShotParams } from '@/api/types';
import type { Viewport } from './useViewport';
import { cardTitle, KIND_ICON } from './cardTitle';
import { CharacterEditor } from './CharacterCardBody';
import { ScriptEditor } from './ScriptCardBody';
import { ShotEditor } from './ShotCardBody';

interface CardEditorLayerProps {
  card: CanvasCard;
  viewport: Viewport;
  /** 这条创作线上的角色卡，镜头编辑器用它列出可勾选的出场角色 */
  characters: CanvasCard[];
  onCancel(): void;
  onSaveScript(card: CanvasCard, text: string): void;
  onSaveShot(card: CanvasCard, shot: ShotParams): void;
  onSaveCharacter(card: CanvasCard, c: CharacterParams): void;
}

/**
 * 就地编辑器所在的层。
 *
 * 它**不能**长在 `.canvas-world` 里：那一层有 `will-change: transform`，本身就是
 * 一个层叠上下文，里面的任何 z-index 都封顶在这一层自己，永远升不过外面的对话坞
 * （z 300）。编辑器一旦落在坞的矩形里，「保存」就点不着——在卡片上调 z-index
 * 解决不了，那条路是死的。
 *
 * 所以编辑器和卡片是两个兄弟 DOM 层（同 frontend-design.md §5.5 里工具条与卡片的
 * 关系），编辑器挂在 `--z-popover` 那一层，用卡片的屏幕坐标定位：
 *   屏幕 x = 世界 x × k + 视口平移 x
 * 再整体 `scale(k)`，视觉上就跟长在卡片上一模一样。平移缩放只改 viewport，
 * 这里每次渲染重算，编辑器自动跟着卡片走，不需要额外的同步机制。
 */
export function CardEditorLayer({
  card,
  viewport,
  characters,
  onCancel,
  onSaveScript,
  onSaveShot,
  onSaveCharacter,
}: CardEditorLayerProps) {
  return (
    <div className="canvas-popover-layer">
      <section
        className="canvas-card editing card-editor-float"
        aria-label={`编辑 ${cardTitle(card)}`}
        style={{
          left: card.x * viewport.k + viewport.x,
          top: card.y * viewport.k + viewport.y,
          width: card.w,
          transform: `scale(${viewport.k})`,
        }}
      >
        <div className="cap">
          {KIND_ICON[card.kind]} {cardTitle(card)}
        </div>
        {card.kind === 'script' ? (
          <ScriptEditor card={card} onSave={(text) => onSaveScript(card, text)} onCancel={onCancel} />
        ) : card.kind === 'character' ? (
          <CharacterEditor card={card} onSave={(c) => onSaveCharacter(card, c)} onCancel={onCancel} />
        ) : (
          <ShotEditor
            card={card}
            characters={characters}
            onSave={(shot) => onSaveShot(card, shot)}
            onCancel={onCancel}
          />
        )}
      </section>
    </div>
  );
}
