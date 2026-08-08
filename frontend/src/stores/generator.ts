import { create } from 'zustand';
import type { JsonValue, ModelCapabilitySchema } from '@/schema/types';
import type { FormValues, InputValues, UploadedFile } from '@/schema/evaluate';
import { defaultValues } from '@/schema/form';
import { adoptValues, migrateValues } from '@/schema/migrate';

export type Modality = 'image' | 'video';

interface ModalityForm {
  modelId: string | null;
  prompt: string;
  values: FormValues;
  inputs: InputValues;
  /** 切模型时被回落到 default 的 key，芯片高亮 1.5s 后由 clearMigrated 清掉 */
  migrated: string[];
}

const empty = (): ModalityForm => ({ modelId: null, prompt: '', values: {}, inputs: {}, migrated: [] });

interface GeneratorState {
  image: ModalityForm;
  video: ModalityForm;
  selectModel(modality: Modality, model: ModelCapabilitySchema): void;
  setPrompt(modality: Modality, prompt: string): void;
  setValue(modality: Modality, key: string, value: JsonValue): void;
  addFiles(modality: Modality, slot: string, files: UploadedFile[]): void;
  removeFile(modality: Modality, slot: string, uploadId: string): void;
  clearMigrated(modality: Modality): void;
  /** 「做同款」：把历史参数灌回表单，非法值走 default 回落 */
  adopt(modality: Modality, model: ModelCapabilitySchema, prompt: string, params: Record<string, unknown>): void;
}

export const useGeneratorStore = create<GeneratorState>((set) => {
  const patch = (modality: Modality, fn: (f: ModalityForm) => ModalityForm) =>
    set((s) => ({ [modality]: fn(s[modality]) }) as Pick<GeneratorState, Modality>);

  return {
    image: empty(),
    video: empty(),

    selectModel(modality, model) {
      patch(modality, (f) => {
        if (f.modelId === model.id) return f;
        // 首次选中不算迁移：没有「用户挑过的旧值」可保
        if (!f.modelId) return { ...f, modelId: model.id, values: defaultValues(model), migrated: [] };
        const { values, migrated } = migrateValues(model, f.values);
        return { ...f, modelId: model.id, values, migrated };
      });
    },

    setPrompt(modality, prompt) {
      patch(modality, (f) => ({ ...f, prompt }));
    },

    setValue(modality, key, value) {
      patch(modality, (f) => ({ ...f, values: { ...f.values, [key]: value } }));
    },

    addFiles(modality, slot, files) {
      patch(modality, (f) => ({ ...f, inputs: { ...f.inputs, [slot]: [...(f.inputs[slot] ?? []), ...files] } }));
    },

    removeFile(modality, slot, uploadId) {
      patch(modality, (f) => ({
        ...f,
        inputs: { ...f.inputs, [slot]: (f.inputs[slot] ?? []).filter((u) => u.upload_id !== uploadId) },
      }));
    },

    clearMigrated(modality) {
      patch(modality, (f) => (f.migrated.length ? { ...f, migrated: [] } : f));
    },

    adopt(modality, model, prompt, params) {
      patch(modality, (f) => ({
        ...f,
        modelId: model.id,
        prompt,
        values: adoptValues(model, params),
        migrated: [],
      }));
    },
  };
});

export function selectForm(modality: Modality) {
  return (s: GeneratorState): ModalityForm => s[modality];
}

export type { ModalityForm, InputValues, FormValues };
