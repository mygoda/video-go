import type { CanvasCard, ShotParams } from '@/api/types';
import type { UploadedFile } from '@/schema/evaluate';
import type { ModelCapabilitySchema } from '@/schema/types';
import { api } from '@/api/endpoints';
import { hasFirstFrame } from './firstFrame';
import { readShot } from './shot';

/** 首帧图挂进去的那个输入槽。与目录里 seedance 的 InputSlotSpec.key 对齐 */
export const FIRST_FRAME_SLOT = 'first_frame';

/**
 * 同时在飞的出片请求上限。
 *
 * 与出图那道闸门同形状但数字不同，理由也不同：出图那边卡的是上游限流，
 * 这边还多一层——一条视频是分钟级、按秒计价的任务，闸门放宽就等于用户点一下
 * 立刻冻结十几倍积分。所以哪怕目录声明得再大，这里也不会同时放过 4 条。
 *
 * 真正生效的上限取 min(这个数, model.limits.max_concurrent_per_user)：
 * 目录里两个出片模型声明的并发一个是 1、一个是 4，照抄任何一个常数都会错。
 */
export const RENDER_MAX_CONCURRENCY = 4;

export function renderConcurrency(model: ModelCapabilitySchema): number {
  return Math.max(1, Math.min(RENDER_MAX_CONCURRENCY, model.limits.max_concurrent_per_user));
}

/**
 * 出片用哪个模型：目录里 p50 最短的那个。
 *
 * 不写死 id（check-no-model-ids.mjs 会拦），也不照抄出图那边的 `[0]` ——
 * 目录顺序是陈列顺序，不表示「适合出片」，取第一个就是把选型交给运营改 order。
 * 判据用 eta 是因为这一步的价值口径就是时间：用户已经确认过首帧，他要的是
 * 尽快看到这一镜动起来，而出片是这条链路上唯一以分钟计的等待。
 */
export function pickRenderModel(models: ModelCapabilitySchema[] | undefined): ModelCapabilitySchema | null {
  if (!models?.length) return null;
  return models.reduce((best, m) => (m.eta.p50_seconds < best.eta.p50_seconds ? m : best));
}

/**
 * 出片 prompt 同时用画面描述与台词 —— 与出图那一步相反。
 *
 * 出图不许带台词（模型会把话画成画面里的字）；出片必须带，seedance 是音画同步
 * 生成，台词进 prompt 才有对上口型的人声，字幕也取同一个字段。所以这两个
 * prompt 是两件事，不能共用一个拼法。
 *
 * 台词单独起一行并加提示词，不是直接接在描述后面：连排的话模型会把它当成
 * 画面内容的一部分去构图，出来是一块写着字的招牌。
 */
export function renderPrompt(shot: ShotParams): string {
  const description = shot.description.trim();
  const dialogue = shot.dialogue.trim();
  return [description, dialogue ? `台词：「${dialogue}」` : ''].filter(Boolean).join('\n');
}

/** 这一镜有没有成片。流程条推进与批量排队都以它为判据 */
export function hasVideo(card: CanvasCard): boolean {
  return (card.asset_id ?? '') !== '';
}

/**
 * 还排得上队的镜头：**必须已经有首帧**，还没有成片，且拼得出 prompt。
 *
 * 没首帧的一律不排，也不顺手替它出一张 —— 首帧是用户逐张看过、确认过的东西，
 * 批量出片时悄悄补一张没人看过的图，等于把那个确认点跳过去了。
 */
export function shotsAwaitingRender(shots: CanvasCard[]): CanvasCard[] {
  return shots.filter((c) => hasFirstFrame(c) && !hasVideo(c) && renderPrompt(readShot(c)) !== '');
}

function extensionOf(mime: string): string {
  if (mime === 'image/png') return 'png';
  if (mime === 'image/webp') return 'webp';
  return 'jpg';
}

/**
 * 把已经落成 asset 的首帧图变成一个能填进输入槽的 upload。
 *
 * ## 为什么要绕这一圈
 *
 * `domain.Task.Inputs` 的注释说值可以是 upload_id 或已有 asset_id，执行器那一层
 * 确实两种都认（`executor.resolveOne` 先查 assets 再回落到提升 upload）。但
 * **提交接口不认**：`httpapi.assertInputUploads` 只查 uploads 表，传 asset_id
 * 会在提交那一刻拿到 400「上传对象 … 不存在或已过期」。首帧是上一步出好的
 * asset，手上没有对应的 upload_id，只能把字节取回来重登记一次。
 *
 * ## 代价，写在这里免得后面有人以为是免费的
 *
 * 执行器会把这份 upload 再提升成一件**新** asset，于是同一张首帧在存储里存了
 * 两份、各自计入配额，血缘的 input 边指向副本而不是卡片上挂着的那张原图。
 * 正解在后端：出片需要一条像 compose 那样直接吃 asset_id 的校验路径。
 */
export async function firstFrameInput(assetId: string): Promise<UploadedFile> {
  const asset = await api.asset(assetId);
  const res = await fetch(asset.original);
  if (!res.ok) throw new Error(`首帧图读不出来（HTTP ${res.status}），这一镜先跳过`);
  const blob = await res.blob();
  const name = `first-frame-${assetId}.${extensionOf(blob.type)}`;
  const upload = await api.uploadFile(blob, name, FIRST_FRAME_SLOT);
  return { upload_id: upload.upload_id, preview_url: upload.preview_url, name };
}
