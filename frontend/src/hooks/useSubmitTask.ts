import { useQueryClient } from '@tanstack/react-query';
import type { ModelCapabilitySchema } from '@/schema/types';
import type { FormValues, InputValues } from '@/schema/evaluate';
import type { Task, TaskError } from '@/api/types';
import { submitParams } from '@/schema/form';
import { estimateCost } from '@/schema/pricing';
import { api } from '@/api/endpoints';
import { ApiError } from '@/api/client';
import { qk } from '@/api/queries';
import { uuid } from '@/lib/uuid';

export interface SubmitArgs {
  model: ModelCapabilitySchema;
  prompt: string;
  values: FormValues;
  inputs: InputValues;
  canvasId?: string;
  cardId?: string;
}

export type SubmitResult = { ok: true; taskId: string } | { ok: false; error: TaskError };

function newClientToken(): string {
  return uuid();
}

export function useSubmitTask() {
  const qc = useQueryClient();

  return async function submit(args: SubmitArgs): Promise<SubmitResult> {
    const { model, prompt, values, inputs } = args;
    const clientToken = newClientToken();
    const params = submitParams(model, values, inputs);
    const inputIds = Object.fromEntries(
      Object.entries(inputs).map(([k, files]) => [k, files.map((f) => f.upload_id)]),
    );

    // 乐观卡片：点下生成就该看见东西，网络往返不该是空窗
    const optimistic: Task = {
      id: clientToken,
      client_token: clientToken,
      status: 'queued',
      model_id: model.id,
      modality: model.modality,
      prompt,
      params,
      inputs: inputIds,
      estimated_cost: estimateCost(model, values, inputIds),
      eta_seconds: null,
      queue_position: null,
      progress: null,
      assets: [],
      error: null,
      created_at: new Date().toISOString(),
      canvas_id: args.canvasId,
      local: 'submitting',
    };
    qc.setQueryData<Task[]>(qk.tasks, (prev) => [optimistic, ...(prev ?? [])]);

    try {
      const res = await api.createTask({
        model_id: model.id,
        prompt,
        inputs: inputIds,
        params,
        client_token: clientToken,
        canvas_id: args.canvasId,
        card_id: args.cardId,
      });
      qc.setQueryData<Task[]>(qk.tasks, (prev) =>
        prev?.map((t) =>
          t.client_token === clientToken
            ? {
                ...t,
                id: res.task_id,
                status: res.status,
                // 以后端返回的 estimated_cost 为准，本地估算只是提交前的展示
                estimated_cost: res.estimated_cost,
                queue_position: res.queue_position,
                eta_seconds: res.eta_seconds,
                local: undefined,
              }
            : t,
        ),
      );
      await qc.invalidateQueries({ queryKey: qk.me });
      return { ok: true, taskId: res.task_id };
    } catch (err) {
      qc.setQueryData<Task[]>(qk.tasks, (prev) => prev?.filter((t) => t.client_token !== clientToken));
      const error: TaskError =
        err instanceof ApiError
          ? err.asTaskError()
          : { code: 'internal_error', message: '网络异常，请稍后重试', retryable: true, charged: false };
      return { ok: false, error };
    }
  };
}
