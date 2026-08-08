import { useState } from 'react';
import type { ModelConfig, ModelConfigUpsert, ProbeResult, ProtocolFamily, Provider, VideoProtocol } from '@/api/types';
import type { ModelCapabilitySchema } from '@/schema/types';
import { CapabilityEditor } from './CapabilityEditor';
import { Sheet } from './Sheet';

const PROTOCOL_FAMILIES: { value: ProtocolFamily; label: string }[] = [
  { value: 'images', label: 'images（同步图片）' },
  { value: 'video', label: 'video（异步视频）' },
  { value: 'chat', label: 'chat（对话）' },
  { value: 'mock', label: 'mock（本地假接口）' },
];

const VIDEO_PROTOCOLS: { value: VideoProtocol; label: string }[] = [
  { value: 'ark', label: 'ark（火山方舟）' },
  { value: 'openai_video', label: 'openai_video' },
  { value: 'google_lro', label: 'google_lro' },
  { value: 'mock', label: 'mock' },
];

function blankCapability(modality: 'image' | 'video'): ModelCapabilitySchema {
  return {
    id: '',
    name: '',
    vendor: '',
    modality,
    enabled: true,
    order: 100,
    inputs: [],
    params: [],
    pricing: { currency: 'credit', base: 10, modifiers: [], rounding: 'ceil' },
    eta: { p50_seconds: 20, p90_seconds: 60 },
    limits: { max_concurrent_per_user: 3, prompt_max_length: 2000, queue_position_available: true },
  };
}

export function blankModel(providerId: string): ModelConfigUpsert {
  return {
    id: '',
    provider_id: providerId,
    upstream_model: '',
    protocol_family: 'images',
    video_protocol: null,
    capability: blankCapability('image'),
    poll_interval_seconds: null,
  };
}

interface Props {
  /** null = 新建 */
  model: ModelConfig | null;
  providers: Provider[];
  busy: boolean;
  probe: ProbeResult | null;
  probing: boolean;
  onProbe(): void;
  onClose(): void;
  onSubmit(payload: ModelConfigUpsert): void;
}

export function ModelSheet({ model, providers, busy, probe, probing, onProbe, onClose, onSubmit }: Props) {
  const [draft, setDraft] = useState<ModelConfigUpsert>(() =>
    model
      ? {
          id: model.id,
          provider_id: model.provider_id,
          upstream_model: model.upstream_model,
          protocol_family: model.protocol_family,
          video_protocol: model.video_protocol ?? null,
          capability: model.capability,
          request_mapping: model.request_mapping,
          poll_interval_seconds: model.poll_interval_seconds ?? null,
        }
      : blankModel(providers[0]?.id ?? ''),
  );
  const [error, setError] = useState<string | null>(null);

  /** capability.id 必须等于配置 id，否则生成器拿到的目录条目对不上任务提交 */
  function setId(id: string): void {
    setDraft({ ...draft, id, capability: { ...draft.capability, id } });
  }

  function setFamily(protocol_family: ProtocolFamily): void {
    const isVideo = protocol_family === 'video';
    setDraft({
      ...draft,
      protocol_family,
      video_protocol: isVideo ? (draft.video_protocol ?? 'ark') : null,
      poll_interval_seconds: isVideo ? (draft.poll_interval_seconds ?? 5) : null,
      capability: { ...draft.capability, modality: isVideo ? 'video' : 'image' },
    });
  }

  function submit(): void {
    if (!draft.id.trim()) return setError('模型 id 不能为空');
    if (!draft.provider_id) return setError('请选择供应商');
    if (!draft.upstream_model.trim()) return setError('上游模型名不能为空');
    if (draft.protocol_family === 'video' && !draft.video_protocol) {
      return setError('视频协议族必须选择 video_protocol');
    }
    if (!draft.capability.name.trim()) return setError('展示名不能为空');
    setError(null);
    onSubmit(draft);
  }

  return (
    <Sheet
      title={model ? `编辑模型 · ${model.capability.name}` : '新增模型'}
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
        <h4>接入</h4>
        <div className="form-grid">
          <div className="field">
            <label htmlFor="md-id">模型 id</label>
            <input
              id="md-id"
              className="input mono"
              placeholder="vendor-model-v1"
              value={draft.id}
              disabled={Boolean(model)}
              onChange={(e) => setId(e.target.value)}
            />
          </div>
          <div className="field">
            <label htmlFor="md-provider">供应商</label>
            <select
              id="md-provider"
              className="select"
              value={draft.provider_id}
              onChange={(e) => setDraft({ ...draft, provider_id: e.target.value })}
            >
              <option value="">请选择</option>
              {providers.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </select>
          </div>
          <div className="field">
            <label htmlFor="md-upstream">上游模型名</label>
            <input
              id="md-upstream"
              className="input mono"
              value={draft.upstream_model}
              onChange={(e) => setDraft({ ...draft, upstream_model: e.target.value })}
            />
          </div>
          <div className="field">
            <label htmlFor="md-family">协议族</label>
            <select
              id="md-family"
              className="select"
              value={draft.protocol_family}
              onChange={(e) => setFamily(e.target.value as ProtocolFamily)}
            >
              {PROTOCOL_FAMILIES.map((p) => (
                <option key={p.value} value={p.value}>
                  {p.label}
                </option>
              ))}
            </select>
          </div>
          {draft.protocol_family === 'video' && (
            <>
              <div className="field">
                <label htmlFor="md-video">video_protocol</label>
                <select
                  id="md-video"
                  className="select"
                  value={draft.video_protocol ?? ''}
                  onChange={(e) => setDraft({ ...draft, video_protocol: e.target.value as VideoProtocol })}
                >
                  {VIDEO_PROTOCOLS.map((v) => (
                    <option key={v.value} value={v.value}>
                      {v.label}
                    </option>
                  ))}
                </select>
              </div>
              <div className="field">
                <label htmlFor="md-poll">轮询间隔（秒）</label>
                <input
                  id="md-poll"
                  className="input"
                  type="number"
                  min={1}
                  value={draft.poll_interval_seconds ?? 5}
                  onChange={(e) => setDraft({ ...draft, poll_interval_seconds: Number(e.target.value) })}
                />
              </div>
            </>
          )}
        </div>
      </div>

      <CapabilityEditor
        value={draft.capability}
        onChange={(capability) => setDraft({ ...draft, capability })}
      />

      {model && (
        <div className="editor-section">
          <h4>
            连通性自检
            <span className="spacer" />
            <button type="button" className="btn btn-sm" onClick={onProbe} disabled={probing}>
              {probing ? '探测中…' : '试跑（dry run）'}
            </button>
          </h4>
          <p className="hint" style={{ marginTop: 'calc(-1 * var(--space-2))' }}>
            只渲染将要发出的请求，不真实调用上游、不产生费用。渲染结果里的凭证位置显示的是环境变量名。
          </p>
          {probe && (
            <>
              <div className="check-row">
                <span className={`tag ${probe.ok ? 'tone-success' : 'tone-danger'}`}>
                  {probe.ok ? '映射可用' : '映射有问题'}
                </span>
                {probe.error && <span className="hint">{probe.error.message}</span>}
              </div>
              <pre
                className="mono"
                style={{
                  margin: 0,
                  padding: 'var(--space-3)',
                  background: 'var(--bg-sunken)',
                  borderRadius: 'var(--radius-md)',
                  fontSize: 'var(--text-2xs)',
                  overflowX: 'auto',
                }}
              >
                {JSON.stringify(probe.rendered_request ?? {}, null, 2)}
              </pre>
            </>
          )}
        </div>
      )}
    </Sheet>
  );
}
