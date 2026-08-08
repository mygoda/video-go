import { useEffect, useMemo, useState } from 'react';
import { NavLink, useLocation, useNavigate } from 'react-router-dom';
import type { ModelCapabilitySchema } from '@/schema/types';
import type { Task } from '@/api/types';
import { useMe, useModels } from '@/api/queries';
import { useAuthStore } from '@/stores/auth';
import { useGeneratorStore, type Modality } from '@/stores/generator';
import { useSubmitTask } from '@/hooks/useSubmitTask';
import { costBreakdown, estimateSeconds, formatSeconds } from '@/schema/pricing';
import { validateForm, type FieldErrors } from '@/schema/validate';
import { ParamChipBar } from '@/components/controls/ParamChipBar';
import { InputSlotStrip } from '@/components/InputSlotStrip';
import { TaskFeed } from '@/components/task/TaskFeed';
import { toast } from '@/stores/toast';

interface AdoptState {
  adopt?: { model_id: string; prompt: string; params: Record<string, unknown> };
}

export function GeneratorPage({ modality }: { modality: Modality }) {
  const isAuthed = useAuthStore((s) => s.isAuthed);
  const navigate = useNavigate();
  const location = useLocation();

  const { data: models, isLoading } = useModels(modality);
  const { data: me } = useMe(isAuthed);
  const form = useGeneratorStore((s) => s[modality]);
  const { selectModel, setPrompt, setValue, addFiles, removeFile, clearMigrated, adopt } = useGeneratorStore();
  const submitTask = useSubmitTask();

  const [errors, setErrors] = useState<FieldErrors>({});
  const [submitting, setSubmitting] = useState(false);

  const model = useMemo(
    () => models?.find((m) => m.id === form.modelId) ?? models?.find((m) => m.enabled) ?? models?.[0],
    [models, form.modelId],
  );

  // 默认选中第一个可用模型，让表单一进页面就是可提交状态
  useEffect(() => {
    if (model && form.modelId !== model.id) selectModel(modality, model);
  }, [model, form.modelId, modality, selectModel]);

  // 「做同款」跳转带过来的参数：灌进表单后立刻清掉 state，免得刷新又灌一次
  useEffect(() => {
    const payload = (location.state as AdoptState | null)?.adopt;
    if (!payload || !models) return;
    const target = models.find((m) => m.id === payload.model_id);
    if (target) {
      adopt(modality, target, payload.prompt, payload.params);
      toast('已载入该作品的参数');
    } else {
      toast('原模型已下线，仅载入了提示词', 'danger');
      setPrompt(modality, payload.prompt);
    }
    navigate(location.pathname, { replace: true });
  }, [location.state, location.pathname, models, modality, adopt, setPrompt, navigate]);

  // 迁移高亮只是一次提示，1.5s 后自己退场
  useEffect(() => {
    if (!form.migrated.length) return;
    const timer = window.setTimeout(() => clearMigrated(modality), 1500);
    return () => window.clearTimeout(timer);
  }, [form.migrated, modality, clearMigrated]);

  if (isLoading || !model) {
    return <main className="generator"><div className="empty">模型列表加载中…</div></main>;
  }

  const cost = costBreakdown(model, form.values, form.inputs);
  const seconds = estimateSeconds(model, form.values);
  const notEnoughCredit = isAuthed && me !== undefined && me.credits < cost.total;
  const emptyPrompt = !form.prompt.trim();

  const disabledReason = !isAuthed
    ? '登录后即可生成'
    : emptyPrompt
      ? '先写一句提示词'
      : notEnoughCredit
        ? `积分不足，还差 ${cost.total - (me?.credits ?? 0)}`
        : null;

  async function onSubmit(): Promise<void> {
    if (!model) return;
    if (!isAuthed) {
      navigate(`/login?next=${encodeURIComponent(location.pathname)}`);
      return;
    }
    const found = validateForm(model, form.prompt, form.values, form.inputs);
    setErrors(found);
    if (Object.keys(found).length) {
      toast('有参数不合法，已在芯片上标出', 'danger');
      return;
    }

    setSubmitting(true);
    const res = await submitTask({ model, prompt: form.prompt, values: form.values, inputs: form.inputs });
    setSubmitting(false);

    if (res.ok) return;
    // 后端也判非法时，把 field_errors 回填到对应芯片上，而不是只弹一句话
    const fieldErrors: FieldErrors = {};
    for (const fe of res.error.field_errors ?? []) fieldErrors[fe.key] = fe.message;
    setErrors(fieldErrors);
    toast(res.error.message, 'danger');
  }

  function retryFrom(task: Task): void {
    setPrompt(modality, task.prompt);
    const target = models?.find((m) => m.id === task.model_id);
    if (target) adopt(modality, target, task.prompt, task.params);
    const fieldErrors: FieldErrors = {};
    for (const fe of task.error?.field_errors ?? []) fieldErrors[fe.key] = fe.message;
    setErrors(fieldErrors);
    window.scrollTo({ top: 0, behavior: 'smooth' });
  }

  function editPromptFrom(task: Task): void {
    setPrompt(modality, task.prompt);
    window.scrollTo({ top: 0, behavior: 'smooth' });
    document.getElementById('prompt-input')?.focus();
  }

  const costTitle = cost.applied.length
    ? `基础 ${cost.base}${cost.applied.map((a) => ` · ${a.label || a.op}${a.op === 'mul' ? '×' : '+'}${a.value}`).join('')}`
    : `基础 ${cost.base}`;

  return (
    <main className="generator">
      <div className="tabs-row">
        <div className="tabs" role="tablist">
          <NavLink className="tab" role="tab" to="/create/image" aria-selected={modality === 'image'}>
            图片
          </NavLink>
          <NavLink className="tab" role="tab" to="/create/video" aria-selected={modality === 'video'}>
            视频
          </NavLink>
        </div>
      </div>

      <div className="composer-wrap">
        <div className="composer">
          <div className="composer-body">
            {/* 传了参考图 = 图生图。前端不发 mode，路由由后端按 inputs 判定 */}
            <InputSlotStrip
              slots={model.inputs}
              inputs={form.inputs}
              errors={errors}
              onAdd={(slot, files) => addFiles(modality, slot, files)}
              onRemove={(slot, id) => removeFile(modality, slot, id)}
            />

            <label className="sr-only" htmlFor="prompt-input">
              提示词
            </label>
            <textarea
              id="prompt-input"
              className="prompt-input"
              rows={3}
              maxLength={model.limits.prompt_max_length}
              placeholder="描述你想要的画面，越具体越好"
              value={form.prompt}
              onChange={(e) => setPrompt(modality, e.target.value)}
            />
            {errors.prompt && (
              <p className="field-error" role="alert">
                {errors.prompt}
              </p>
            )}
          </div>

          <ParamChipBar
            models={models ?? []}
            model={model}
            values={form.values}
            inputs={form.inputs}
            errors={errors}
            migrated={form.migrated}
            onSelectModel={(m: ModelCapabilitySchema) => selectModel(modality, m)}
            onChange={(key, value) => setValue(modality, key, value)}
          >
            <span className="cost" title={costTitle}>
              ⚡<span className="amount mono">{cost.total}</span> 积分 · {formatSeconds(seconds)}
            </span>
            <button
              type="button"
              className="btn btn-primary btn-lg"
              onClick={() => void onSubmit()}
              disabled={submitting || Boolean(disabledReason)}
              title={disabledReason ?? undefined}
            >
              {submitting ? '提交中…' : '生成'}
            </button>
          </ParamChipBar>
        </div>
        {disabledReason && (
          <p className="hint" style={{ textAlign: 'right', marginTop: 'var(--space-2)' }}>
            {disabledReason}
          </p>
        )}
      </div>

      {isAuthed && <TaskFeed modality={modality} onRetry={retryFrom} onEditPrompt={editPromptFrom} />}
    </main>
  );
}
