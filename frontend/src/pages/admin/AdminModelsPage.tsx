import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ApiError } from '@/api/client';
import { adminApi } from '@/api/endpoints';
import { invalidateModelCatalog, qk, useAdminModels, useAdminProviders } from '@/api/queries';
import type { ModelConfig, ModelConfigUpsert, ProbeResult, Provider, ProviderUpsert } from '@/api/types';
import { ModelSheet } from '@/components/admin/ModelSheet';
import { ProviderSheet } from '@/components/admin/ProviderSheet';
import { formatRelative } from '@/components/admin/format';
import { toast } from '@/stores/toast';

/** 凭证只回显掩码：环境变量名保留首尾，中间打码，够管理员认出是哪一条又不泄露命名细节 */
function maskRef(ref: string): string {
  if (ref.length <= 4) return '••••';
  return `${ref.slice(0, 2)}••••${ref.slice(-2)}`;
}

function errorText(err: unknown): string {
  return err instanceof ApiError ? err.message : '操作失败，请稍后重试';
}

type ProviderEditing = { mode: 'create' } | { mode: 'edit'; provider: Provider };
type ModelEditing = { mode: 'create' } | { mode: 'edit'; model: ModelConfig };

export function AdminModelsPage() {
  const qc = useQueryClient();
  const { data: providers, isPending: providersPending } = useAdminProviders();
  const { data: models, isPending: modelsPending } = useAdminModels();

  const [providerEditing, setProviderEditing] = useState<ProviderEditing | null>(null);
  const [modelEditing, setModelEditing] = useState<ModelEditing | null>(null);
  const [probe, setProbe] = useState<ProbeResult | null>(null);

  const saveProvider = useMutation({
    mutationFn: (payload: ProviderUpsert) =>
      providerEditing?.mode === 'edit'
        ? adminApi.updateProvider(providerEditing.provider.id, payload)
        : adminApi.createProvider(payload),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: qk.adminProviders });
      setProviderEditing(null);
      toast('供应商已保存');
    },
    onError: (err) => toast(errorText(err), 'danger'),
  });

  const removeProvider = useMutation({
    mutationFn: (id: string) => adminApi.deleteProvider(id),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: qk.adminProviders });
      toast('供应商已删除');
    },
    onError: (err) => toast(errorText(err), 'danger'),
  });

  const saveModel = useMutation({
    mutationFn: (payload: ModelConfigUpsert) =>
      modelEditing?.mode === 'edit'
        ? adminApi.updateModel(modelEditing.model.id, payload)
        : adminApi.createModel(payload),
    onSuccess: () => {
      // 生成器的模型下拉立刻跟着变，这正是这个页面存在的理由
      invalidateModelCatalog(qc);
      setModelEditing(null);
      setProbe(null);
      toast('模型已保存，生成器的模型列表已同步');
    },
    onError: (err) => toast(errorText(err), 'danger'),
  });

  const removeModel = useMutation({
    mutationFn: (id: string) => adminApi.deleteModel(id),
    onSuccess: () => {
      invalidateModelCatalog(qc);
      toast('模型已删除');
    },
    onError: (err) => toast(errorText(err), 'danger'),
  });

  const runProbe = useMutation({
    mutationFn: (id: string) => adminApi.probeModel(id, true),
    onSuccess: (result) => setProbe(result),
    onError: (err) => toast(errorText(err), 'danger'),
  });

  const providerName = (id: string) => providers?.find((p) => p.id === id)?.name ?? id;

  return (
    <>
      <section>
        <div className="admin-toolbar">
          <h3 style={{ margin: 0 }}>模型配置</h3>
          <span className="hint">{models?.length ?? 0} 条</span>
          <span className="spacer" />
          <button
            type="button"
            className="btn btn-primary"
            disabled={!providers?.length}
            onClick={() => {
              setProbe(null);
              setModelEditing({ mode: 'create' });
            }}
          >
            新增模型
          </button>
        </div>

        {modelsPending ? (
          <p className="hint">加载中…</p>
        ) : models?.length ? (
          <table className="table">
            <thead>
              <tr>
                <th>模型</th>
                <th>供应商</th>
                <th>协议</th>
                <th>计费</th>
                <th>状态</th>
                <th>更新</th>
                <th className="right">操作</th>
              </tr>
            </thead>
            <tbody>
              {models.map((m) => (
                <tr key={m.id}>
                  <td className="strong">
                    {m.capability.name}
                    <div className="hint mono" style={{ fontSize: 'var(--text-2xs)' }}>
                      {m.id}
                    </div>
                  </td>
                  <td>{providerName(m.provider_id)}</td>
                  <td>
                    <span className="tag">{m.protocol_family}</span>
                    {m.video_protocol && <span className="tag tone-accent">{m.video_protocol}</span>}
                  </td>
                  <td className="mono">{m.capability.pricing.base}</td>
                  <td>
                    {m.capability.enabled ? (
                      <span className="tag tone-success">已启用</span>
                    ) : (
                      <span className="tag tone-muted">已停用</span>
                    )}
                  </td>
                  <td>{formatRelative(m.updated_at)}</td>
                  <td>
                    <div className="row-actions">
                      <button
                        type="button"
                        className="btn btn-sm"
                        onClick={() => {
                          setProbe(null);
                          setModelEditing({ mode: 'edit', model: m });
                        }}
                      >
                        编辑
                      </button>
                      <button
                        type="button"
                        className="btn btn-sm btn-ghost"
                        onClick={() => {
                          if (window.confirm(`删除模型「${m.capability.name}」？生成器里会立刻消失。`)) {
                            removeModel.mutate(m.id);
                          }
                        }}
                      >
                        删除
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          <div className="empty">
            <div className="title">还没有模型配置</div>
            <p>先添加一个供应商，再新增模型；保存后生成器的模型下拉会立刻出现它。</p>
          </div>
        )}
      </section>

      <section style={{ marginTop: 'var(--space-8)' }}>
        <div className="admin-toolbar">
          <h3 style={{ margin: 0 }}>供应商</h3>
          <span className="hint">凭证只保存环境变量名，密钥不入库</span>
          <span className="spacer" />
          <button type="button" className="btn" onClick={() => setProviderEditing({ mode: 'create' })}>
            新增供应商
          </button>
        </div>

        {providersPending ? (
          <p className="hint">加载中…</p>
        ) : providers?.length ? (
          <table className="table">
            <thead>
              <tr>
                <th>名称</th>
                <th>base_url</th>
                <th>凭证变量</th>
                <th>并发 / 超时</th>
                <th>状态</th>
                <th className="right">操作</th>
              </tr>
            </thead>
            <tbody>
              {providers.map((p) => (
                <tr key={p.id}>
                  <td className="strong">
                    {p.name}
                    <div className="hint mono" style={{ fontSize: 'var(--text-2xs)' }}>
                      {p.id}
                    </div>
                  </td>
                  <td className="mono">{p.base_url}</td>
                  <td>
                    <span className="mask">{maskRef(p.credential_ref)}</span>{' '}
                    {p.credential_present ? (
                      <span className="tag tone-success">已配置</span>
                    ) : (
                      <span className="tag tone-warning">缺失</span>
                    )}
                  </td>
                  <td className="mono">
                    {p.max_concurrency ?? '—'} / {p.timeout_ms ? `${Math.round(p.timeout_ms / 1000)}s` : '—'}
                  </td>
                  <td>
                    {p.enabled ? (
                      <span className="tag tone-success">已启用</span>
                    ) : (
                      <span className="tag tone-muted">已停用</span>
                    )}
                  </td>
                  <td>
                    <div className="row-actions">
                      <button
                        type="button"
                        className="btn btn-sm"
                        onClick={() => setProviderEditing({ mode: 'edit', provider: p })}
                      >
                        编辑
                      </button>
                      <button
                        type="button"
                        className="btn btn-sm btn-ghost"
                        onClick={() => {
                          if (window.confirm(`删除供应商「${p.name}」？`)) removeProvider.mutate(p.id);
                        }}
                      >
                        删除
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          <div className="empty">
            <div className="title">还没有供应商</div>
            <p>模型必须挂在供应商下，先建一个。</p>
          </div>
        )}
      </section>

      {providerEditing && (
        <ProviderSheet
          key={providerEditing.mode === 'edit' ? providerEditing.provider.id : 'new'}
          provider={providerEditing.mode === 'edit' ? providerEditing.provider : null}
          busy={saveProvider.isPending}
          onClose={() => setProviderEditing(null)}
          onSubmit={(payload) => saveProvider.mutate(payload)}
        />
      )}

      {modelEditing && (
        <ModelSheet
          key={modelEditing.mode === 'edit' ? modelEditing.model.id : 'new'}
          model={modelEditing.mode === 'edit' ? modelEditing.model : null}
          providers={providers ?? []}
          busy={saveModel.isPending}
          probe={probe}
          probing={runProbe.isPending}
          onProbe={() => {
            if (modelEditing.mode === 'edit') runProbe.mutate(modelEditing.model.id);
          }}
          onClose={() => {
            setModelEditing(null);
            setProbe(null);
          }}
          onSubmit={(payload) => saveModel.mutate(payload)}
        />
      )}
    </>
  );
}
