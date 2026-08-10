import type { CanvasCard, ShotParams, Task } from '@/api/types';
import type { ModelCapabilitySchema } from '@/schema/types';
import { api } from '@/api/endpoints';
import { readShot } from './shot';

/**
 * 同时在飞的出图请求上限。
 *
 * 这是**闸门**，不是重试策略：上游按并发限流，一口气把一条线的镜头全推上去
 * 会整批拿到 429 rate_limited，而 429 重试还是 429 —— 排队才是解，退避只是
 * 把同一批请求换个时刻再撞一次。
 *
 * 模型自己声明的 max_concurrent_per_user 更小时以它为准，但无论它声明多大，
 * 这里都不会放过 2 个。
 */
export const FIRST_FRAME_MAX_CONCURRENCY = 2;

export function firstFrameConcurrency(model: ModelCapabilitySchema): number {
  return Math.max(1, Math.min(FIRST_FRAME_MAX_CONCURRENCY, model.limits.max_concurrent_per_user));
}

/**
 * 出图 prompt 只取画面描述。
 *
 * **台词绝不能拼进来**：出图模型会把 prompt 里的话直接画成画面里的文字，
 * 首帧上就会浮出一行字。台词是出片那一步的输入（Seedance 靠它生成同步人声），
 * 两个 prompt 用途不同，共用一个拼法必然有一边是错的。
 */
export function firstFramePrompt(shot: ShotParams): string {
  return shot.description.trim();
}

/** 这一镜有没有首帧产物。流程条推进与批量排队都以它为判据 */
export function hasFirstFrame(card: CanvasCard): boolean {
  return readShot(card).first_frame_asset_id !== '';
}

/**
 * 还排得上队的镜头：已经有首帧的跳过，没写镜头描述的也跳过 ——
 * 空 prompt 提交上去只会换回一次 400，白占一个闸门名额。
 */
export function shotsAwaitingFirstFrame(shots: CanvasCard[]): CanvasCard[] {
  return shots.filter((c) => !hasFirstFrame(c) && firstFramePrompt(readShot(c)) !== '');
}

/**
 * 带闸门的批量执行：任意时刻在跑的 run 不超过 limit 个，其余排队。
 *
 * 用固定数量的 worker 轮流从游标取任务，而不是 chunk 成一批批的 Promise.all——
 * 分批的话每一批都要等最慢的那个结束，闸门实际利用率会掉到一半。
 */
export async function runWithGate<T>(
  items: T[],
  limit: number,
  run: (item: T) => Promise<void>,
): Promise<void> {
  let cursor = 0;
  const workers = Array.from({ length: Math.min(limit, items.length) }, async () => {
    while (cursor < items.length) {
      const item = items[cursor];
      cursor += 1;
      await run(item);
    }
  });
  await Promise.all(workers);
}

const POLL_MS = 2000;

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, ms));
}

/**
 * 轮询到任务走进终态。
 *
 * 闸门要卡的是"在飞的请求数"，所以这一步必须等到任务真的结束才放行下一个 ——
 * 提交返回 202 只说明它排上了队，那时候上游还没开始算。
 *
 * 不走 SSE：那条通道断线会降级、会漏事件，靠它决定闸门什么时候放行，
 * 漏一条就是永远卡住。轮询是这里唯一不会卡死的形态。
 */
export async function waitForTask(taskId: string): Promise<Task> {
  for (;;) {
    await sleep(POLL_MS);
    const task = await api.task(taskId);
    if (task.status !== 'queued' && task.status !== 'running') return task;
  }
}
