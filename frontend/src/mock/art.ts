/** 占位产物图。mock 后端没有真实模型，用确定性的渐变 SVG 顶替，配色取自 mockup.css 的 .art */
const PALETTES = [
  ['#2A2140', '#3B2A5C', '#12233D'],
  ['#3D2A1E', '#6B4226', '#24160E'],
  ['#10303A', '#1E5F63', '#0C1E24'],
  ['#3A1030', '#6A1D4A', '#1B0A16'],
];

function hash(seed: string): number {
  let h = 2166136261;
  for (let i = 0; i < seed.length; i++) {
    h ^= seed.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return Math.abs(h);
}

export function artUrl(seed: string, width: number, height: number): string {
  const [a, b, c] = PALETTES[hash(seed) % PALETTES.length];
  const angle = hash(seed + 'a') % 90;
  const svg =
    `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}" viewBox="0 0 ${width} ${height}">` +
    `<defs><linearGradient id="g" gradientTransform="rotate(${angle})">` +
    `<stop offset="0" stop-color="${a}"/><stop offset="0.5" stop-color="${b}"/><stop offset="1" stop-color="${c}"/>` +
    `</linearGradient></defs><rect width="${width}" height="${height}" fill="url(#g)"/></svg>`;
  return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`;
}
