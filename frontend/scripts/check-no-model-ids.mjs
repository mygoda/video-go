#!/usr/bin/env node
/**
 * 验收线守卫：前端源码里不允许出现具体模型 id / 供应商名。
 * 一旦出现，就说明有人写了 `if (model.id === 'xxx')` 这类分支，
 * 「后端接新模型、前端零改动」这条线当场就破了。
 *
 * src/mock/ 是模拟后端的数据，不算前端逻辑，故排除。
 */
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = join(fileURLToPath(new URL('.', import.meta.url)), '..');
const srcDir = join(root, 'src');
const EXCLUDED = [join(srcDir, 'mock')];

const FORBIDDEN = [
  /\bseedream\b/i,
  /\bhailuo\b/i,
  /\baurora-lite\b/i,
  /\bmidjourney\b/i,
  /\bstable[-_ ]?diffusion\b/i,
  /\bkling\b/i,
  /\bsora\b/i,
];

function walk(dir) {
  const out = [];
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (EXCLUDED.some((ex) => path.startsWith(ex))) continue;
    if (statSync(path).isDirectory()) out.push(...walk(path));
    else if (/\.(ts|tsx)$/.test(path)) out.push(path);
  }
  return out;
}

const offenders = [];
for (const file of walk(srcDir)) {
  const lines = readFileSync(file, 'utf8').split('\n');
  lines.forEach((line, i) => {
    for (const pattern of FORBIDDEN) {
      if (pattern.test(line)) offenders.push(`${relative(root, file)}:${i + 1}  ${line.trim()}`);
    }
  });
}

if (offenders.length) {
  console.error('发现硬编码的模型标识，schema 驱动被绕过了：');
  for (const line of offenders) console.error('  ' + line);
  process.exit(1);
}

console.log('ok: 前端源码无模型 id 硬编码');
