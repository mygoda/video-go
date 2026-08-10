import { useCallback, useMemo, useRef, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { useQueryClient } from '@tanstack/react-query';
import type { CanvasCard, CanvasOp, CanvasState, ShotParams } from '@/api/types';
import { api } from '@/api/endpoints';
import { ApiError } from '@/api/client';
import { qk, useCanvas, useMe, useModels, useProjects } from '@/api/queries';
import { useSubmitTask } from '@/hooks/useSubmitTask';
import { defaultValues } from '@/schema/form';
import { estimateCost } from '@/schema/pricing';
import type { ModelCapabilitySchema } from '@/schema/types';
import { useAuthStore } from '@/stores/auth';
import { toast } from '@/stores/toast';
import { useViewport } from '@/canvas/useViewport';
import { useCanvasSync } from '@/canvas/useCanvasSync';
import { CanvasCardView } from '@/canvas/CanvasCardView';
import { CardEditorLayer } from '@/canvas/CardEditorLayer';
import { ConversationDock } from '@/canvas/ConversationDock';
import { CardRerunPanel } from '@/canvas/CardRerunPanel';
import { ComposeBar } from '@/canvas/ComposeBar';
import { FlowStatusBar } from '@/canvas/FlowStatusBar';
import { ScriptVersionsPanel } from '@/canvas/ScriptVersionsPanel';
import { activeScript, MAX_SHOTS } from '@/canvas/flow';
import {
  firstFrameConcurrency,
  firstFramePrompt,
  hasFirstFrame,
  runWithGate,
  shotsAwaitingFirstFrame,
  waitForTask,
} from '@/canvas/firstFrame';
import {
  FIRST_FRAME_SLOT,
  firstFrameInput,
  hasVideo,
  pickRenderModel,
  renderConcurrency,
  renderPrompt,
  shotsAwaitingRender,
} from '@/canvas/render';
import type { ScriptVersion } from '@/canvas/script';
import { readShot, relayoutShots, shotParams, shotSlot, shotsOf, SHOT_CARD_H, SHOT_CARD_W } from '@/canvas/shot';

interface DragState {
  pointerId: number;
  cardId: string;
  startX: number;
  startY: number;
  originX: number;
  originY: number;
}

function bounds(cards: CanvasCard[]): { minX: number; minY: number; maxX: number; maxY: number } | null {
  if (!cards.length) return null;
  return cards.reduce(
    (acc, c) => ({
      minX: Math.min(acc.minX, c.x),
      minY: Math.min(acc.minY, c.y),
      maxX: Math.max(acc.maxX, c.x + c.w),
      maxY: Math.max(acc.maxY, c.y + c.h),
    }),
    { minX: Infinity, minY: Infinity, maxX: -Infinity, maxY: -Infinity },
  );
}

export function CanvasPage() {
  const { projectId = '' } = useParams<{ projectId: string }>();
  const isAuthed = useAuthStore((s) => s.isAuthed);
  const { data: me } = useMe(isAuthed);
  const { data: projects } = useProjects();
  const { data: canvas, isLoading } = useCanvas(projectId);
  // 出首帧用图片目录里的第一个模型。目录接口只返回 enabled 且 public 的模型，
  // 所以这里不需要——也不允许——把模型 id 写死在前端（check-no-model-ids.mjs 会拦）。
  const { data: imageModels } = useModels('image');
  const firstFrameModel = imageModels?.[0] ?? null;
  // 出片走视频目录。这里不能照抄上面的 [0]：目录顺序是陈列顺序，
  // 判据见 pickRenderModel —— 这一步的等待以分钟计，取 p50 最短的那个。
  const { data: videoModels } = useModels('video');
  const renderModel = useMemo(() => pickRenderModel(videoModels), [videoModels]);
  const submitTask = useSubmitTask();
  const qc = useQueryClient();

  const [viewportEl, setViewportEl] = useState<HTMLDivElement | null>(null);
  const { viewport, panning, zoomBy, fit, toWorld, handlers } = useViewport(projectId, viewportEl);
  const { enqueue, flush, saveState } = useCanvasSync(projectId);

  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [rerunOpen, setRerunOpen] = useState(false);
  // 就地编辑同一时刻只有一张卡：两张卡同时开着编辑器，用户改完一张去点另一张
  // 时会以为两边都保存了，实际上只提交了后点的那张。
  const [editingId, setEditingId] = useState<string | null>(null);
  const [versionsOpen, setVersionsOpen] = useState(false);
  const [droppedRefs, setDroppedRefs] = useState<string[]>([]);
  // 合成模式：点选卡片不再是"选中"，而是往片段序列里追加。数组顺序即成片顺序，
  // 所以它是 string[] 而不是 Set——用户点选的先后是这个功能唯一的排序依据。
  const [composing, setComposing] = useState(false);
  const [picks, setPicks] = useState<string[]>([]);
  // 拆分镜是同步接口，一次只放一个在飞：连点两下会拆出两组镜头卡，
  // 用户看到的是同一个剧本下莫名其妙多了一倍的镜头。
  const [storyboarding, setStoryboarding] = useState(false);
  // 批量出首帧在途。它只挡「再点一次批量」，不挡单张重出——那两件事作用在
  // 不同的镜头上，互相拦住只会让用户以为界面卡了。
  const [firstFrameBusy, setFirstFrameBusy] = useState(false);
  // 正在出首帧的那几镜。按卡片 id 记而不是记一个总数：骨架屏要精确显示在
  // 那几张卡上，而闸门下同时在飞的永远只是其中两张。
  const [framesInFlight, setFramesInFlight] = useState<ReadonlySet<string>>(new Set());
  // 批量出片在途。与 firstFrameBusy 分开两个 flag：这两批作用在不同的镜头上
  // （没图的 vs 有图没片的），互相拦住只会让用户以为界面卡了。
  const [renderBusy, setRenderBusy] = useState(false);
  const [rendersInFlight, setRendersInFlight] = useState<ReadonlySet<string>>(new Set());
  const [dockCollapsed, setDockCollapsed] = useState(false);
  // 拖卡片的中间位置只落在这里，松手才写进缓存并排队上行
  const dragRef = useRef<DragState | null>(null);
  const [dragPos, setDragPos] = useState<{ id: string; x: number; y: number } | null>(null);

  const project = projects?.find((p) => p.id === projectId);
  const cards = canvas?.cards ?? [];
  const selected = cards.find((c) => c.id === selectedId) ?? null;
  // 只有剧本与镜头有就地编辑器；卡片被删或换了类型时 editingId 可能指向一张
  // 已经不该开编辑器的卡，这里一并兜住。
  const editingCard = cards.find((c) => c.id === editingId && (c.kind === 'script' || c.kind === 'shot')) ?? null;
  const lineageIds = new Set(selected?.refs ?? []);

  // 流程条盯着的那一条创作线：选中的剧本（或选中镜头回溯到的剧本），
  // 没选就是最新的那份。加镜 / 删镜 / 调镜数都作用在它名下。
  const flowScript = useMemo(() => activeScript(cards, selectedId), [cards, selectedId]);
  const flowShots = useMemo(() => (flowScript ? shotsOf(cards, flowScript.id) : []), [cards, flowScript]);

  // 点一次「出首帧」会排上几镜。已经有图的、没写描述的都不算——前者重出要
  // 一张一张来，后者提交上去只会换回一次 400。
  const pendingFrames = useMemo(() => shotsAwaitingFirstFrame(flowShots).length, [flowShots]);
  // 这一批的报价。单张按模型默认参数算，乘以镜数：镜头越多这一步越贵，
  // 数字得在点下去之前就摆在按钮旁边。
  const firstFrameCost = useMemo(
    () => (firstFrameModel ? estimateCost(firstFrameModel, defaultValues(firstFrameModel), {}) * pendingFrames : null),
    [firstFrameModel, pendingFrames],
  );

  // 点一次「出片」会排上几镜：已经有首帧、还没有成片的那些。没首帧的一律不算，
  // 也不顺手替它出一张——那张图用户没看过，等于把首帧那个确认点跳过去了。
  const pendingRenders = useMemo(() => shotsAwaitingRender(flowShots).length, [flowShots]);
  // 报价按「每一镜都挂着首帧」算：排上队的镜头个个都有图，而目录里的加价条件
  // 可能就看输入槽有没有填，传空对象会报低。
  const renderCost = useMemo(
    () =>
      renderModel
        ? estimateCost(renderModel, defaultValues(renderModel), { [FIRST_FRAME_SLOT]: [''] }) * pendingRenders
        : null,
    [renderModel, pendingRenders],
  );

  const refCards = useMemo(
    () => (selected && !droppedRefs.includes(selected.id) ? [selected] : []),
    [selected, droppedRefs],
  );

  const onCardDragStart = useCallback((e: React.PointerEvent, card: CanvasCard) => {
    e.stopPropagation();
    dragRef.current = {
      pointerId: e.pointerId,
      cardId: card.id,
      startX: e.clientX,
      startY: e.clientY,
      originX: card.x,
      originY: card.y,
    };
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
  }, []);

  const onCardDragMove = useCallback(
    (e: React.PointerEvent) => {
      const drag = dragRef.current;
      if (!drag || drag.pointerId !== e.pointerId) return;
      e.stopPropagation();
      setDragPos({
        id: drag.cardId,
        x: drag.originX + (e.clientX - drag.startX) / viewport.k,
        y: drag.originY + (e.clientY - drag.startY) / viewport.k,
      });
    },
    [viewport.k],
  );

  const onCardDragEnd = useCallback(
    (e: React.PointerEvent) => {
      const drag = dragRef.current;
      if (!drag || drag.pointerId !== e.pointerId) return;
      e.stopPropagation();
      dragRef.current = null;
      const pos = dragPos;
      setDragPos(null);
      if (!pos) return;
      const x = Math.round(pos.x);
      const y = Math.round(pos.y);
      const ops: CanvasOp[] = [{ type: 'card.move', id: drag.cardId, x, y }];
      // 用户手动挪过的卡片退出自动重排（后端 storyboardCards 就是这么约定的）。
      // 少了这一条，改一次镜号就会把用户特意摆开的那张镜头卡拽回格子里。
      const moved = cards.find((c) => c.id === drag.cardId);
      if (moved?.auto_placed) ops.push({ type: 'card.update', id: drag.cardId, patch: { auto_placed: false } });
      enqueue(ops, (prev) => ({
        ...prev,
        cards: prev.cards.map((c) => (c.id === drag.cardId ? { ...c, x, y, auto_placed: false } : c)),
      }));
    },
    [cards, dragPos, enqueue],
  );

  function addTextCard(): void {
    const rect = viewportEl?.getBoundingClientRect();
    const at = rect ? toWorld(rect.left + rect.width / 2, rect.top + 160) : { x: 0, y: 0 };
    const card: CanvasCard = {
      id: `c_${Date.now().toString(36)}`,
      kind: 'text',
      x: Math.round(at.x),
      y: Math.round(at.y),
      w: 220,
      h: 120,
      z: cards.length + 1,
      // 不带 title：标题由 cardTitle() 按 kind 兜底，用户没起过名的卡不该被前端
      // 先斩后奏地写死一个名字（后端补上标题后会随卡片带回来）。
      text: '双击编辑…',
      refs: [],
      history: [],
      auto_placed: false,
      created_at: new Date().toISOString(),
    };
    enqueue([{ type: 'card.create', card }], (prev) => ({ ...prev, cards: [...prev.cards, card] }));
    setSelectedId(card.id);
  }

  function deleteSelected(): void {
    if (!selected) return;
    const id = selected.id;
    setSelectedId(null);
    setEditingId((cur) => (cur === id ? null : cur));
    enqueue([{ type: 'card.delete', id }], (prev) => ({ ...prev, cards: prev.cards.filter((c) => c.id !== id) }));
  }

  /** 剧本卡：正文空着建出来，直接进编辑态，用户不用再找哪儿能写。 */
  function addScriptCard(): void {
    const rect = viewportEl?.getBoundingClientRect();
    const at = rect ? toWorld(rect.left + rect.width / 2, rect.top + 160) : { x: 0, y: 0 };
    const card: CanvasCard = {
      id: `c_${Date.now().toString(36)}`,
      kind: 'script',
      x: Math.round(at.x),
      y: Math.round(at.y),
      w: 320,
      h: 200,
      z: cards.length + 1,
      text: '',
      refs: [],
      history: [],
      auto_placed: false,
      created_at: new Date().toISOString(),
    };
    enqueue([{ type: 'card.create', card }], (prev) => ({ ...prev, cards: [...prev.cards, card] }));
    setSelectedId(card.id);
    setEditingId(card.id);
  }

  /**
   * 在一张剧本卡名下追加 n 个空镜头：refs 指回剧本（血缘就是这么记的，不建边表），
   * 镜号顺着已有的最大镜号往下排，落点直接取该序号的格子。
   *
   * 一次 enqueue 建完而不是循环调用：调镜数时会一口气补好几镜，一镜一个
   * op 批次会让画布连着抖好几下，中途失败还会只落一半。
   */
  function appendShots(script: CanvasCard, n: number): CanvasCard[] {
    const siblings = shotsOf(cards, script.id);
    const maxNo = siblings.reduce((max, s) => Math.max(max, readShot(s).shot_no), 0);
    const stamp = Date.now().toString(36);
    const created = Array.from({ length: n }, (_, i): CanvasCard => {
      const at = shotSlot(script, siblings.length + i);
      return {
        id: `c_${stamp}${i ? `_${i}` : ''}`,
        kind: 'shot',
        x: at.x,
        y: at.y,
        w: SHOT_CARD_W,
        h: SHOT_CARD_H,
        z: cards.length + 1 + i,
        params: shotParams({
          shot_no: maxNo + 1 + i,
          description: '',
          dialogue: '',
          duration_sec: 0,
          camera: '',
          shot_size: '',
          first_frame_asset_id: '',
        }),
        refs: [script.id],
        history: [],
        auto_placed: true,
        created_at: new Date().toISOString(),
      };
    });
    enqueue(
      created.map((card) => ({ type: 'card.create' as const, card })),
      (prev) => ({ ...prev, cards: [...prev.cards, ...created] }),
    );
    return created;
  }

  /**
   * 删掉末尾 n 镜。
   *
   * 删末尾而不是删选中的那一镜：镜号因此始终是连续的 1..N，用户不用回头
   * 补编号。要删中间某一镜走顶栏的「删除卡片」——那是明确指着一张卡说的话。
   */
  function removeShots(script: CanvasCard, n: number): void {
    const victims = shotsOf(cards, script.id).slice(-n);
    if (!victims.length) return;
    const ids = new Set(victims.map((c) => c.id));
    setSelectedId((cur) => (cur && ids.has(cur) ? null : cur));
    setEditingId((cur) => (cur && ids.has(cur) ? null : cur));
    enqueue(
      victims.map((c) => ({ type: 'card.delete' as const, id: c.id })),
      (prev) => ({ ...prev, cards: prev.cards.filter((c) => !ids.has(c.id)) }),
    );
  }

  function addShotCard(): void {
    if (!selected || selected.kind !== 'script' || shotsOf(cards, selected.id).length >= MAX_SHOTS) return;
    const [card] = appendShots(selected, 1);
    setSelectedId(card.id);
    setEditingId(card.id);
  }

  /**
   * 把一张剧本卡下的镜头凑够 target 个。多退少补，都由用户点了才发生——
   * 这一步同样不会自己往下走。
   */
  function resizeShots(target: number): void {
    if (!flowScript) return;
    const diff = target - flowShots.length;
    if (diff > 0) appendShots(flowScript, diff);
    else if (diff < 0) removeShots(flowScript, -diff);
  }

  /**
   * 拆分镜：这一步是**用户点出来的**，剧本写完不会自己走到这里。
   *
   * 发之前必须把队列冲干净：用户很可能刚改完剧本正文，那次 patch 还压在
   * 500ms 的防抖里，后端读到的会是改之前的旧稿，拆出来的镜头对不上他看见的字。
   */
  async function runStoryboard(count: number): Promise<void> {
    if (!flowScript || storyboarding) return;
    setStoryboarding(true);
    try {
      await flush();
      await api.storyboard(projectId, flowScript.id, count);
      await qc.invalidateQueries({ queryKey: qk.canvas(projectId) });
    } catch (err) {
      toast(err instanceof ApiError ? err.message : '拆分镜失败，请稍后重试', 'danger');
    } finally {
      setStoryboarding(false);
    }
  }

  /**
   * 把出好的首帧挂回镜头卡。
   *
   * 卡片从缓存里现取而不是用闭包里那张：一批图要跑好几分钟，用户完全可能在
   * 等图的时候把某一镜的描述改了。拿旧快照拼 params 会把他刚写的字覆盖掉，
   * 而 card.update 的 params 是整列覆盖，覆盖了就找不回来。
   */
  function linkFirstFrame(cardId: string, assetId: string): void {
    const current = qc.getQueryData<CanvasState>(qk.canvas(projectId))?.cards.find((c) => c.id === cardId);
    if (!current) return;
    const params = shotParams({ ...readShot(current), first_frame_asset_id: assetId });
    enqueue([{ type: 'card.update', id: cardId, patch: { params } }], (prev) => ({
      ...prev,
      cards: prev.cards.map((c) => (c.id === cardId ? { ...c, params } : c)),
    }));
  }

  /**
   * 出一镜的首帧：提交、等它走到终态、把产物挂回这张卡。
   *
   * **提交时不带 cardId**：后端拿到 card_id 会在任务成功时把产物直接写进
   * canvas_cards.asset_id，而出片那一步要往同一个字段写视频——首帧当场就没了，
   * 卡片的 history 还会把图和视频混成一串。首帧是镜头的属性，归 params 管，
   * 所以这一条关联由前端在任务成功后自己写。
   *
   * 代价说清楚：这中间关掉页面，图还在资产库里，但这一镜不会自动关联上，
   * 得重出一次。
   */
  async function generateFirstFrame(model: ModelCapabilitySchema, card: CanvasCard): Promise<void> {
    setFramesInFlight((prev) => new Set(prev).add(card.id));
    try {
      const res = await submitTask({
        model,
        prompt: firstFramePrompt(readShot(card)),
        values: defaultValues(model),
        inputs: {},
        canvasId: projectId,
      });
      if (!res.ok) {
        toast(res.error.message, 'danger');
        return;
      }
      const task = await waitForTask(res.taskId);
      const asset = task.assets?.[0];
      if (task.status !== 'succeeded' || !asset) {
        toast(task.error?.message ?? `第 ${readShot(card).shot_no} 镜的首帧没出来`, 'danger');
        return;
      }
      linkFirstFrame(card.id, asset.id);
    } finally {
      setFramesInFlight((prev) => {
        const next = new Set(prev);
        next.delete(card.id);
        return next;
      });
    }
  }

  /**
   * 批量出首帧：这一步也是**用户点出来的**，分镜拆完不会自己走到这里，
   * 出完也不会自己往下走到出片——形状与 runStoryboard 一致，没有任何
   * useEffect 会调到它。
   *
   * 并发交给 runWithGate 卡住：上游按并发限流，一口气把 12 镜全推上去会整批
   * 拿到 429，而重试还是 429。排队才是解。
   */
  async function runFirstFrames(): Promise<void> {
    if (!firstFrameModel || firstFrameBusy) return;
    // 用户很可能刚改完某一镜的描述，那次 patch 还压在防抖里；不冲干净就会
    // 拿改之前的旧描述去出图。
    await flush();
    const targets = shotsAwaitingFirstFrame(flowShots);
    if (!targets.length) return;
    setFirstFrameBusy(true);
    try {
      await runWithGate(targets, firstFrameConcurrency(firstFrameModel), (card) =>
        generateFirstFrame(firstFrameModel, card),
      );
      await flush();
      await qc.invalidateQueries({ queryKey: qk.canvas(projectId) });
    } finally {
      setFirstFrameBusy(false);
    }
  }

  /** 单张重出：只动这一镜，另外几镜的首帧原样留着。 */
  async function rerunFirstFrame(card: CanvasCard): Promise<void> {
    if (!firstFrameModel || framesInFlight.has(card.id)) return;
    await flush();
    if (!firstFramePrompt(readShot(card))) {
      toast('这一镜还没写镜头描述，先写一句再出图', 'danger');
      return;
    }
    await generateFirstFrame(firstFrameModel, card);
    await flush();
    await qc.invalidateQueries({ queryKey: qk.canvas(projectId) });
  }

  /**
   * 出一镜的成片：把首帧填进输入槽，提交、等它走到终态。
   *
   * **提交时带 cardId**，与首帧那一步刻意相反：视频就是这张镜头卡的产物，
   * 后端在任务成功时写 canvas_cards.asset_id（store/mysql 的 SetCardResult 只动
   * 这一列），params.first_frame_asset_id 不在它的射程里，两件产物各占各的字段。
   *
   * 一镜出不来只记这一镜：整批的其余镜头照跑。所以这里全程 toast、不抛——
   * 抛出去会让 runWithGate 的那条 worker 连带把后面排着的镜头一起丢掉。
   */
  async function generateVideo(model: ModelCapabilitySchema, card: CanvasCard): Promise<void> {
    const shot = readShot(card);
    setRendersInFlight((prev) => new Set(prev).add(card.id));
    try {
      const input = await firstFrameInput(shot.first_frame_asset_id);
      const res = await submitTask({
        model,
        prompt: renderPrompt(shot),
        values: defaultValues(model),
        inputs: { [FIRST_FRAME_SLOT]: [input] },
        canvasId: projectId,
        cardId: card.id,
      });
      if (!res.ok) {
        toast(res.error.message, 'danger');
        return;
      }
      const task = await waitForTask(res.taskId);
      if (task.status !== 'succeeded') {
        toast(task.error?.message ?? `第 ${shot.shot_no} 镜没出片`, 'danger');
      }
    } catch (err) {
      toast(err instanceof Error ? err.message : `第 ${shot.shot_no} 镜没出片`, 'danger');
    } finally {
      setRendersInFlight((prev) => {
        const next = new Set(prev);
        next.delete(card.id);
        return next;
      });
    }
  }

  /**
   * 批量出片：同样是**用户点出来的**。首帧出完不会自己走到这里，出完片也不会
   * 自己去合成——形状与 runFirstFrames / runStoryboard 一致，没有任何 useEffect
   * 或任务回调能调到它。
   *
   * 闸门的上限取 min(前端上限, 目录里这个模型的 max_concurrent_per_user)：
   * 一条视频是分钟级、按秒计价的任务，放宽闸门就等于用户点一下立刻冻结十几倍积分。
   */
  async function runRenders(): Promise<void> {
    if (!renderModel || renderBusy) return;
    // 用户很可能刚改完某一镜的台词，那次 patch 还压在防抖里；不冲干净
    // 出来的片子念的就是改之前的那句。
    await flush();
    const targets = shotsAwaitingRender(flowShots);
    if (!targets.length) return;
    setRenderBusy(true);
    try {
      await runWithGate(targets, renderConcurrency(renderModel), (card) => generateVideo(renderModel, card));
      await qc.invalidateQueries({ queryKey: qk.canvas(projectId) });
    } finally {
      setRenderBusy(false);
    }
  }

  /** 单条重出：只动这一镜，另外几镜的成片原样留着。 */
  async function rerunRender(card: CanvasCard): Promise<void> {
    if (!renderModel || rendersInFlight.has(card.id)) return;
    await flush();
    if (!hasFirstFrame(card)) {
      toast('这一镜还没有首帧，出片要拿首帧当第一画面', 'danger');
      return;
    }
    if (!renderPrompt(readShot(card))) {
      toast('这一镜既没有描述也没有台词，先写一句再出片', 'danger');
      return;
    }
    await generateVideo(renderModel, card);
    await qc.invalidateQueries({ queryKey: qk.canvas(projectId) });
  }

  /**
   * 剧本正文的保存只发 text（必要时带上回退的标题）。版本是服务端在这次 patch
   * 里自己追加的，前端多发一个 params 就会被 400 顶回来（store/mysql 的
   * errScriptParamsNotPatchable）。也正因为版本在服务端生成，写完必须立刻把
   * 队列冲掉再重取画布，否则版本历史里看不到刚存的这一版。
   */
  async function patchScript(card: CanvasCard, patch: Partial<CanvasCard>): Promise<void> {
    enqueue([{ type: 'card.update', id: card.id, patch }], (prev) => ({
      ...prev,
      cards: prev.cards.map((c) => (c.id === card.id ? { ...c, ...patch } : c)),
    }));
    await flush();
    await qc.invalidateQueries({ queryKey: qk.canvas(projectId) });
  }

  function saveScript(card: CanvasCard, text: string): void {
    setEditingId(null);
    if ((card.text ?? '') === text) return;
    void patchScript(card, { text });
  }

  /** 回退：把那一版的正文（和它当时的标题，如果有）写回当前，这次回退本身也会被服务端留成一版。 */
  function restoreScript(card: CanvasCard, version: ScriptVersion): void {
    if ((card.text ?? '') === version.text) return;
    const patch: Partial<CanvasCard> = { text: version.text };
    if (version.title && version.title !== card.title) patch.title = version.title;
    void patchScript(card, patch);
  }

  /** 改完镜头就按镜号重排一次：镜号是分镜的顺序，画布上的位置必须跟着它走。 */
  function saveShot(card: CanvasCard, shot: ShotParams): void {
    setEditingId(null);
    const params = shotParams(shot);
    const next = cards.map((c) => (c.id === card.id ? { ...c, params } : c));
    const moves = card.refs.length ? relayoutShots(next, card.refs[0]) : [];
    const ops: CanvasOp[] = [
      { type: 'card.update', id: card.id, patch: { params } },
      ...moves.map((m) => ({ type: 'card.move' as const, id: m.id, x: m.x, y: m.y })),
    ];
    enqueue(ops, (prev) => ({
      ...prev,
      cards: prev.cards.map((c) => {
        const base = c.id === card.id ? { ...c, params } : c;
        const move = moves.find((m) => m.id === c.id);
        return move ? { ...base, x: move.x, y: move.y } : base;
      }),
    }));
  }

  // 合成模式下点卡片是加/减片段；再点一次同一张就把它取消，序号自动前移。
  function onCardClick(id: string): void {
    if (!composing) {
      setSelectedId(id);
      return;
    }
    setPicks((prev) => (prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id]));
  }

  if (isLoading || !canvas) {
    return <div className="empty">画布加载中…</div>;
  }

  const toolbarLeft = selected ? selected.x * viewport.k + viewport.x + selected.w * viewport.k - 96 : 0;
  const toolbarTop = selected ? selected.y * viewport.k + viewport.y - 38 : 0;

  return (
    <div className="canvas-screen">
      {/* 画布用更窄的顶栏：这里视口比导航值钱 */}
      <header className="canvas-topbar">
        <Link className="btn btn-sm btn-ghost" to="/projects">
          ← 返回
        </Link>
        <span className="proj-name">{project?.name ?? '画布'}</span>
        <span className="rev mono">
          {saveState === 'pending' ? '保存中…' : saveState === 'error' ? '已重新同步' : '已保存'} · rev {canvas.revision}
        </span>
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 8, alignItems: 'center' }}>
          <span className="credit-badge">
            ⚡ <span className="mono">{(me?.credits ?? 0).toLocaleString()}</span>
          </span>
          <button type="button" className="btn btn-sm" onClick={addTextCard}>
            ＋ 便签
          </button>
          <button type="button" className="btn btn-sm" onClick={addScriptCard}>
            ＋ 剧本
          </button>
          <button
            type="button"
            className="btn btn-sm"
            disabled={selected?.kind !== 'script' || flowShots.length >= MAX_SHOTS}
            title={
              selected?.kind !== 'script'
                ? '先选中一张剧本卡'
                : flowShots.length >= MAX_SHOTS
                  ? `镜头数最多 ${MAX_SHOTS}`
                  : '在这张剧本下加一个镜头'
            }
            onClick={addShotCard}
          >
            ＋ 镜头
          </button>
          <button
            type="button"
            className={`btn btn-sm${composing ? ' btn-primary' : ''}`}
            onClick={() => {
              setComposing((on) => !on);
              setSelectedId(null);
              setPicks([]);
            }}
          >
            🎬 合成
          </button>
          <button type="button" className="btn btn-sm" disabled={!selected} onClick={deleteSelected}>
            删除卡片
          </button>
        </div>
      </header>

      <FlowStatusBar
        script={flowScript}
        shots={flowShots}
        busy={storyboarding}
        firstFrameBusy={firstFrameBusy}
        firstFramePending={pendingFrames}
        firstFrameCost={firstFrameCost}
        renderBusy={renderBusy}
        renderPending={pendingRenders}
        renderCost={renderCost}
        onStoryboard={(count) => void runStoryboard(count)}
        onFirstFrames={() => void runFirstFrames()}
        onRenders={() => void runRenders()}
        onAddShot={() => flowScript && appendShots(flowScript, 1)}
        onRemoveShot={() => flowScript && removeShots(flowScript, 1)}
        onResizeShots={resizeShots}
      />

      <div
        ref={setViewportEl}
        className={`canvas-viewport${panning ? ' panning' : ''}`}
        style={{ backgroundSize: `${24 * viewport.k}px ${24 * viewport.k}px`, backgroundPosition: `${viewport.x}px ${viewport.y}px` }}
        onPointerDown={(e) => {
          // 只有落在空白处才算平移。落在卡片或浮层控件上时绝不能 setPointerCapture ——
          // 指针一旦被视口捕获，按钮就收不到 pointerup，click 永远不会派发。
          const onBackground =
            e.target === e.currentTarget || (e.target as HTMLElement).classList.contains('canvas-world');
          if (!onBackground) return;
          setSelectedId(null);
          handlers.onPointerDown(e);
        }}
        onPointerMove={(e) => {
          onCardDragMove(e);
          if (!dragRef.current) handlers.onPointerMove(e);
        }}
        onPointerUp={(e) => {
          onCardDragEnd(e);
          handlers.onPointerUp(e);
        }}
        onPointerCancel={handlers.onPointerCancel}
      >
        {/* 所有卡片放进这一个 transform 容器，平移缩放只重排这一层 */}
        <div
          className="canvas-world"
          style={{ transform: `translate(${viewport.x}px, ${viewport.y}px) scale(${viewport.k})` }}
        >
          {cards.map((card) => {
            const live = dragPos?.id === card.id ? { ...card, x: dragPos.x, y: dragPos.y } : card;
            return (
              <CanvasCardView
                key={card.id}
                card={live}
                k={viewport.k}
                visible
                selected={card.id === selectedId}
                lineage={lineageIds.has(card.id)}
                dimmed={Boolean(selectedId) && card.id !== selectedId && !lineageIds.has(card.id)}
                pickIndex={picks.indexOf(card.id) + 1}
                firstFramePending={framesInFlight.has(card.id)}
                renderPending={rendersInFlight.has(card.id)}
                onSelect={onCardClick}
                onDragStart={onCardDragStart}
                onEditStart={setEditingId}
              />
            );
          })}
        </div>

        <div className="canvas-overlay">
          {selected && (
            <div className="card-toolbar" style={{ left: toolbarLeft, top: toolbarTop }}>
              {(selected.kind === 'script' || selected.kind === 'shot') && (
                <button
                  type="button"
                  className="icon-btn"
                  title="编辑正文（也可以双击卡片）"
                  aria-label="编辑正文"
                  onClick={() => setEditingId(selected.id)}
                >
                  ✎
                </button>
              )}
              {selected.kind === 'script' && (
                <button
                  type="button"
                  className="icon-btn"
                  title="版本历史"
                  aria-label="版本历史"
                  aria-pressed={versionsOpen}
                  onClick={() => setVersionsOpen((on) => !on)}
                >
                  ⟲
                </button>
              )}
              <button
                type="button"
                className="icon-btn"
                title={selected.kind === 'shot' ? '重出这一镜的首帧' : '重跑（片段重拍）'}
                aria-label={selected.kind === 'shot' ? '重出首帧' : '重跑'}
                // 重跑要拿模型重出一件产物：图片 / 视频卡是换一张产物，镜头卡是
                // 换一张首帧。剧本的正文是用户写的，改它走就地编辑。
                disabled={
                  selected.kind === 'shot'
                    ? !firstFrameModel || framesInFlight.has(selected.id)
                    : selected.kind !== 'image' && selected.kind !== 'video'
                }
                onClick={() => {
                  if (selected.kind === 'shot') void rerunFirstFrame(selected);
                  else setRerunOpen(true);
                }}
              >
                ↻
              </button>
              {/* 出片单独一个按钮，不跟 ↻ 挤在一起：一镜有两件产物，
                  「换一张首帧」和「重出这条片子」是两个价钱、两个等待长度。 */}
              {selected.kind === 'shot' && (
                <button
                  type="button"
                  className="icon-btn"
                  title={
                    !hasFirstFrame(selected)
                      ? '先出首帧：出片要拿首帧当第一画面'
                      : hasVideo(selected)
                        ? '重出这一镜的成片（首帧不动）'
                        : '出这一镜的成片'
                  }
                  aria-label={hasVideo(selected) ? '重出成片' : '出成片'}
                  disabled={!renderModel || !hasFirstFrame(selected) || rendersInFlight.has(selected.id)}
                  onClick={() => void rerunRender(selected)}
                >
                  ▶
                </button>
              )}
              <button
                type="button"
                className="icon-btn"
                title="引用为下次输入"
                aria-label="引用为下次输入"
                onClick={() => setDroppedRefs((prev) => prev.filter((id) => id !== selected.id))}
              >
                ⊹
              </button>
            </div>
          )}

          <div className="viewport-controls">
            <button type="button" className="vc-btn" aria-label="缩小" onClick={() => zoomBy(1 / 1.2)}>
              ⊖
            </button>
            <span className="mono" style={{ minWidth: 38, textAlign: 'center' }}>
              {Math.round(viewport.k * 100)}%
            </span>
            <button type="button" className="vc-btn" aria-label="放大" onClick={() => zoomBy(1.2)}>
              ⊕
            </button>
            <span style={{ width: 1, height: 16, background: 'var(--border-subtle)' }} />
            <button type="button" className="vc-btn" aria-label="适应全部内容" onClick={() => fit(bounds(cards))}>
              ⛶
            </button>
          </div>

          {versionsOpen && selected?.kind === 'script' && (
            <ScriptVersionsPanel
              card={selected}
              onRestore={(version) => restoreScript(selected, version)}
              onClose={() => setVersionsOpen(false)}
            />
          )}

          {rerunOpen && selected && (
            <CardRerunPanel
              projectId={projectId}
              card={selected}
              onClose={() => setRerunOpen(false)}
              onSubmitted={(taskId) => {
                // 重跑期间这张卡原位显示骨架屏，靠 task_id 关联
                qc.setQueryData<CanvasState>(qk.canvas(projectId), (prev) =>
                  prev
                    ? { ...prev, cards: prev.cards.map((c) => (c.id === selected.id ? { ...c, task_id: taskId } : c)) }
                    : prev,
                );
              }}
            />
          )}

          {/* 合成条与对话坞都占底部，同时出现会互相压住；合成是个短暂的模式，
              退出就回到对话。 */}
          {composing ? (
            <ComposeBar
              projectId={projectId}
              picks={picks}
              cards={cards}
              onClear={() => setPicks([])}
              onExit={() => {
                setComposing(false);
                setPicks([]);
              }}
            />
          ) : (
            <ConversationDock
              projectId={projectId}
              conversation={canvas.conversation}
              refCards={refCards}
              collapsed={dockCollapsed}
              onCollapsedChange={setDockCollapsed}
              onRemoveRef={(id) => setDroppedRefs((prev) => [...prev, id])}
            />
          )}
        </div>

        {/* 编辑器单独一层，压在坞（z 300）之上。它不能进 .canvas-world —— 那一层
            是层叠上下文，里面的 z-index 封顶在它自己身上。详见 CardEditorLayer。 */}
        {editingCard && (
          <CardEditorLayer
            card={editingCard}
            viewport={viewport}
            onCancel={() => setEditingId(null)}
            onSaveScript={saveScript}
            onSaveShot={saveShot}
          />
        )}
      </div>
    </div>
  );
}
