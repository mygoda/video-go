import { useNavigate } from 'react-router-dom';
import { useQueryClient } from '@tanstack/react-query';
import type { Asset, CanvasCard } from '@/api/types';
import { api } from '@/api/endpoints';
import { qk } from '@/api/queries';
import { toast } from '@/stores/toast';

export interface AdoptPayload {
  model_id: string;
  prompt: string;
  params: Record<string, unknown>;
}

/** 送入画布时的落点：按已有卡片数排成网格，避免叠在原点 */
export function autoPlace(index: number): { x: number; y: number } {
  const perRow = 4;
  return { x: (index % perRow) * 320, y: Math.floor(index / perRow) * 280 };
}

export function useAssetActions() {
  const navigate = useNavigate();
  const qc = useQueryClient();

  function download(asset: Asset): void {
    const a = document.createElement('a');
    a.href = asset.original;
    a.download = `${asset.id}.${asset.type === 'video' ? 'mp4' : 'png'}`;
    a.click();
  }

  /** 「做同款」：带着来源参数跳回对应 modality 的生成器，由 GeneratorPage 灌进表单 */
  function adopt(asset: Asset): void {
    const modality = asset.type === 'video' ? 'video' : 'image';
    navigate(`/create/${modality}`, {
      state: {
        adopt: { model_id: asset.source.model_id, prompt: asset.source.prompt, params: asset.source.params },
      },
    });
  }

  async function sendToCanvas(asset: Asset): Promise<void> {
    try {
      let projects = await api.projects();
      if (!projects.length) {
        const created = await api.createProject('未命名画布');
        projects = [created];
      }
      const project = projects[0];
      const state = await api.canvas(project.id);
      const card: CanvasCard = {
        id: `c_${Date.now().toString(36)}`,
        kind: asset.type,
        ...autoPlace(state.cards.length),
        w: 280,
        // 尺寸未知时按 4:3 给个占位高度，别把卡片算成 0 高
        h: asset.width && asset.height ? Math.round((280 * asset.height) / asset.width) : 210,
        z: state.cards.length + 1,
        asset_id: asset.id,
        title: asset.source.model_name,
        model_id: asset.source.model_id,
        prompt: asset.source.prompt,
        params: asset.source.params,
        refs: [],
        history: [],
        auto_placed: true,
        created_at: new Date().toISOString(),
      };
      await api.patchCanvas(project.id, state.revision, [{ type: 'card.create', card }]);
      await qc.invalidateQueries({ queryKey: qk.canvas(project.id) });
      await qc.invalidateQueries({ queryKey: qk.projects });
      toast(`已送入画布《${project.name}》`);
    } catch {
      toast('送入画布失败，请稍后重试', 'danger');
    }
  }

  async function remove(asset: Asset): Promise<void> {
    try {
      await api.deleteAsset(asset.id);
      await qc.invalidateQueries({ queryKey: ['assets'] });
      await qc.invalidateQueries({ queryKey: qk.tasks });
      toast('已删除');
    } catch {
      toast('删除失败', 'danger');
    }
  }

  return { download, adopt, sendToCanvas, remove };
}
