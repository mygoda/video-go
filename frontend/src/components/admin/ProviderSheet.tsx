import { useState } from 'react';
import type { AuthStyle, Provider, ProviderUpsert } from '@/api/types';
import { Sheet } from './Sheet';

const AUTH_STYLES: { value: AuthStyle; label: string }[] = [
  { value: 'bearer', label: 'Bearer Token' },
  { value: 'header', label: '自定义请求头' },
  { value: 'query', label: 'Query 参数' },
];

export function blankProvider(): ProviderUpsert {
  return {
    id: '',
    name: '',
    base_url: '',
    credential_ref: '',
    auth_style: 'bearer',
    enabled: true,
    timeout_ms: 60_000,
    max_concurrency: 4,
  };
}

interface Props {
  /** null = 新建 */
  provider: Provider | null;
  busy: boolean;
  onClose(): void;
  onSubmit(payload: ProviderUpsert): void;
}

export function ProviderSheet({ provider, busy, onClose, onSubmit }: Props) {
  const [draft, setDraft] = useState<ProviderUpsert>(() =>
    provider
      ? {
          id: provider.id,
          name: provider.name,
          base_url: provider.base_url,
          credential_ref: provider.credential_ref,
          auth_style: provider.auth_style ?? 'bearer',
          auth_header_name: provider.auth_header_name,
          enabled: provider.enabled,
          timeout_ms: provider.timeout_ms,
          max_concurrency: provider.max_concurrency,
        }
      : blankProvider(),
  );
  const [error, setError] = useState<string | null>(null);

  function submit(): void {
    if (!draft.id.trim()) return setError('供应商 id 不能为空');
    if (!draft.name.trim()) return setError('名称不能为空');
    if (!draft.base_url.trim()) return setError('base_url 不能为空');
    if (!draft.credential_ref.trim()) return setError('凭证环境变量名不能为空');
    if (draft.auth_style === 'header' && !draft.auth_header_name?.trim()) {
      return setError('自定义请求头方式必须填请求头名称');
    }
    setError(null);
    onSubmit(draft);
  }

  return (
    <Sheet
      title={provider ? `编辑供应商 · ${provider.name}` : '新增供应商'}
      onClose={onClose}
      footer={
        <>
          {error && (
            <span className="hint danger" role="alert" style={{ marginRight: 'auto' }}>
              {error}
            </span>
          )}
          <button type="button" className="btn btn-ghost" onClick={onClose}>
            取消
          </button>
          <button type="button" className="btn btn-primary" onClick={submit} disabled={busy}>
            {busy ? '保存中…' : '保存'}
          </button>
        </>
      }
    >
      <div className="editor-section">
        <h4>基本信息</h4>
        <div className="form-grid">
          <div className="field">
            <label htmlFor="pv-id">供应商 id</label>
            <input
              id="pv-id"
              className="input"
              placeholder="pv_example"
              value={draft.id}
              disabled={Boolean(provider)}
              onChange={(e) => setDraft({ ...draft, id: e.target.value })}
            />
          </div>
          <div className="field">
            <label htmlFor="pv-name">名称</label>
            <input
              id="pv-name"
              className="input"
              value={draft.name}
              onChange={(e) => setDraft({ ...draft, name: e.target.value })}
            />
          </div>
          <div className="field span-2">
            <label htmlFor="pv-url">base_url</label>
            <input
              id="pv-url"
              className="input"
              placeholder="https://example.com/v1"
              value={draft.base_url}
              onChange={(e) => setDraft({ ...draft, base_url: e.target.value })}
            />
          </div>
        </div>
      </div>

      <div className="editor-section">
        <h4>凭证</h4>
        {/* 这里填的永远是 env 变量名。密钥本体只在后端进程的环境变量里，不入库、不回传、不回显 */}
        <p className="hint" style={{ marginTop: 'calc(-1 * var(--space-2))' }}>
          填写<strong>环境变量名</strong>，不要粘贴密钥本身。密钥只从后端进程的环境变量读取，
          不入库、不经过浏览器。
        </p>
        <div className="form-grid">
          <div className="field">
            <label htmlFor="pv-cred">凭证环境变量名</label>
            <input
              id="pv-cred"
              className="input mono"
              placeholder="EXAMPLE_API_KEY"
              value={draft.credential_ref}
              onChange={(e) => setDraft({ ...draft, credential_ref: e.target.value })}
            />
          </div>
          <div className="field">
            <label htmlFor="pv-auth">鉴权方式</label>
            <select
              id="pv-auth"
              className="select"
              value={draft.auth_style ?? 'bearer'}
              onChange={(e) => setDraft({ ...draft, auth_style: e.target.value as AuthStyle })}
            >
              {AUTH_STYLES.map((a) => (
                <option key={a.value} value={a.value}>
                  {a.label}
                </option>
              ))}
            </select>
          </div>
          {draft.auth_style === 'header' && (
            <div className="field span-2">
              <label htmlFor="pv-header">请求头名称</label>
              <input
                id="pv-header"
                className="input mono"
                placeholder="X-Api-Key"
                value={draft.auth_header_name ?? ''}
                onChange={(e) => setDraft({ ...draft, auth_header_name: e.target.value })}
              />
            </div>
          )}
        </div>
        {provider && (
          <p className="hint">
            当前状态：
            {provider.credential_present ? (
              <span className="tag tone-success">环境变量已配置</span>
            ) : (
              <span className="tag tone-warning">环境变量缺失，调用会失败</span>
            )}
          </p>
        )}
      </div>

      <div className="editor-section">
        <h4>调用限制</h4>
        <div className="form-grid">
          <div className="field">
            <label htmlFor="pv-timeout">超时（毫秒）</label>
            <input
              id="pv-timeout"
              className="input"
              type="number"
              min={1000}
              value={draft.timeout_ms ?? 0}
              onChange={(e) => setDraft({ ...draft, timeout_ms: Number(e.target.value) })}
            />
          </div>
          <div className="field">
            <label htmlFor="pv-conc">最大并发</label>
            <input
              id="pv-conc"
              className="input"
              type="number"
              min={1}
              value={draft.max_concurrency ?? 0}
              onChange={(e) => setDraft({ ...draft, max_concurrency: Number(e.target.value) })}
            />
          </div>
        </div>
        <div className="check-row">
          <input
            id="pv-enabled"
            type="checkbox"
            checked={draft.enabled}
            onChange={(e) => setDraft({ ...draft, enabled: e.target.checked })}
          />
          <label htmlFor="pv-enabled">启用（关闭后其下所有模型都不再派发）</label>
        </div>
      </div>
    </Sheet>
  );
}
