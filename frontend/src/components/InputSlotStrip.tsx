import { useRef, useState } from 'react';
import type { InputSlotSpec } from '@/schema/types';
import type { InputValues, UploadedFile } from '@/schema/evaluate';
import { validateFile } from '@/schema/validate';
import { api } from '@/api/endpoints';

interface InputSlotStripProps {
  slots: InputSlotSpec[];
  inputs: InputValues;
  errors: Record<string, string>;
  onAdd(slot: string, files: UploadedFile[]): void;
  onRemove(slot: string, uploadId: string): void;
}

function readAsDataUrl(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(new Error('读取文件失败'));
    reader.readAsDataURL(file);
  });
}

function Slot({
  slot,
  files,
  error,
  onAdd,
  onRemove,
}: {
  slot: InputSlotSpec;
  files: UploadedFile[];
  error?: string;
  onAdd(files: UploadedFile[]): void;
  onRemove(uploadId: string): void;
}) {
  const inputRef = useRef<HTMLInputElement>(null);
  const [busy, setBusy] = useState(false);
  const [localError, setLocalError] = useState<string | null>(null);
  const canAdd = files.length < slot.max_count;

  async function handleFiles(list: FileList | null): Promise<void> {
    if (!list?.length) return;
    setLocalError(null);
    setBusy(true);
    try {
      const room = slot.max_count - files.length;
      const uploaded: UploadedFile[] = [];
      for (const file of Array.from(list).slice(0, room)) {
        const problem = await validateFile(slot, file);
        if (problem) {
          setLocalError(`${file.name}：${problem}`);
          continue;
        }
        const res = await api.upload(await readAsDataUrl(file), file.name);
        uploaded.push({ upload_id: res.upload_id, preview_url: res.preview_url, name: file.name });
      }
      if (uploaded.length) onAdd(uploaded);
    } catch {
      setLocalError('上传失败，请重试');
    } finally {
      setBusy(false);
      if (inputRef.current) inputRef.current.value = '';
    }
  }

  const message = error ?? localError;

  return (
    <div>
      <div className="slot-strip">
        {files.map((f) => (
          <div className="upload-slot filled" key={f.upload_id}>
            <img src={f.preview_url} alt={f.name} />
            <button
              type="button"
              className="slot-remove"
              aria-label={`移除${slot.label} ${f.name}`}
              onClick={() => onRemove(f.upload_id)}
            >
              ×
            </button>
          </div>
        ))}

        {canAdd && (
          <>
            {/* 真 input + label：拖拽区好看但键盘用户过不去，这里两者都要 */}
            <input
              ref={inputRef}
              id={`slot-${slot.key}`}
              className="sr-only"
              type="file"
              accept={slot.accept.join(',')}
              multiple={slot.max_count > 1}
              disabled={busy}
              onChange={(e) => void handleFiles(e.target.files)}
            />
            <label className="upload-slot" htmlFor={`slot-${slot.key}`} title={slot.hint}>
              {busy ? '…' : '＋'}
              <span className="sr-only">上传{slot.label}</span>
            </label>
          </>
        )}

        <span className="slot-label">
          {slot.label}
          {slot.required ? ' · 必填' : ''} · 最多 {slot.max_count} 个
        </span>
      </div>
      {message && (
        <p className="field-error" role="alert">
          {message}
        </p>
      )}
    </div>
  );
}

export function InputSlotStrip({ slots, inputs, errors, onAdd, onRemove }: InputSlotStripProps) {
  if (!slots.length) return null;
  return (
    <>
      {slots.map((slot) => (
        <Slot
          key={slot.key}
          slot={slot}
          files={inputs[slot.key] ?? []}
          error={errors[slot.key]}
          onAdd={(files) => onAdd(slot.key, files)}
          onRemove={(id) => onRemove(slot.key, id)}
        />
      ))}
    </>
  );
}
