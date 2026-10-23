/**
 * utils.js — 通用工具函数
 *
 * 内容：SHA-256（含非安全上下文下的纯 JS 兜底）、时间格式化、
 *       toast 提示、剪贴板复制（含 execCommand 降级）、随机 ID、设备徽标配色。
 *
 * 安全约定：本文件渲染一律使用 DOM API / textContent，禁止 innerHTML 拼接内容。
 */

// ---------------------------------------------------------------------------
// SHA-256
// ---------------------------------------------------------------------------

/** SHA-256 轮常量（前 64 个素数立方根小数部分的前 32 位） */
const SHA256_K = [
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
];

/**
 * 纯 JS 实现的 SHA-256，返回小写 hex。
 * 局域网 HTTP（非安全上下文）下 crypto.subtle 不可用，用它兜底，
 * 保证客户端计算的 hash 与服务端始终一致（防回环依赖 hash 精确匹配）。
 */
export function sha256HexPure(text) {
  const bytes = new TextEncoder().encode(text);
  const len = bytes.length;
  // 消息总长 = 填充到 64 字节对齐（附加 0x80 + 8 字节位长度）
  const totalLen = (((len + 9) + 63) >> 6) << 6;
  const block = new Uint8Array(totalLen);
  block.set(bytes);
  block[len] = 0x80;
  const dv = new DataView(block.buffer);
  // 64 位大端位长度（文本不可能超过 2^32 位，高 32 位用除法避免精度丢失）
  dv.setUint32(totalLen - 8, Math.floor(len / 0x20000000));
  dv.setUint32(totalLen - 4, ((len % 0x20000000) * 8) >>> 0);

  const h = [
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
    0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ];
  const w = new Uint32Array(64);

  for (let off = 0; off < totalLen; off += 64) {
    for (let i = 0; i < 16; i++) w[i] = dv.getUint32(off + i * 4);
    for (let i = 16; i < 64; i++) {
      const x = w[i - 15];
      const y = w[i - 2];
      const s0 = ((x >>> 7) | (x << 25)) ^ ((x >>> 18) | (x << 14)) ^ (x >>> 3);
      const s1 = ((y >>> 17) | (y << 15)) ^ ((y >>> 19) | (y << 13)) ^ (y >>> 10);
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) >>> 0;
    }
    let [a, b, c, d, e, f, g, hh] = h;
    for (let i = 0; i < 64; i++) {
      const S1 = ((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7));
      const ch = (e & f) ^ (~e & g);
      const t1 = (hh + S1 + ch + SHA256_K[i] + w[i]) >>> 0;
      const S0 = ((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10));
      const maj = (a & b) ^ (a & c) ^ (b & c);
      const t2 = (S0 + maj) >>> 0;
      hh = g; g = f; f = e;
      e = (d + t1) >>> 0;
      d = c; c = b; b = a;
      a = (t1 + t2) >>> 0;
    }
    h[0] = (h[0] + a) >>> 0; h[1] = (h[1] + b) >>> 0;
    h[2] = (h[2] + c) >>> 0; h[3] = (h[3] + d) >>> 0;
    h[4] = (h[4] + e) >>> 0; h[5] = (h[5] + f) >>> 0;
    h[6] = (h[6] + g) >>> 0; h[7] = (h[7] + hh) >>> 0;
  }
  let out = '';
  for (const x of h) out += x.toString(16).padStart(8, '0');
  return out;
}

/** 计算文本 SHA-256（小写 hex）。优先用原生 crypto.subtle，不可用时用纯 JS 实现。 */
export async function sha256Hex(text) {
  const subtle = globalThis.crypto && globalThis.crypto.subtle;
  if (subtle) {
    try {
      const buf = await subtle.digest('SHA-256', new TextEncoder().encode(text));
      return Array.from(new Uint8Array(buf), (b) => b.toString(16).padStart(2, '0')).join('');
    } catch { /* 忽略，走纯 JS 兜底 */ }
  }
  return sha256HexPure(text);
}

// ---------------------------------------------------------------------------
// 时间格式化（本地化，中文相对时间）
// ---------------------------------------------------------------------------

const pad2 = (n) => String(n).padStart(2, '0');

/** 相对时间：刚刚 / N 秒前 / N 分钟前 / N 小时前 / 昨天 HH:MM / N 天前 / 日期 */
export function formatRelativeTime(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  const diff = (Date.now() - d.getTime()) / 1000; // 秒，可能为负（时钟偏差）
  if (diff < 10) return '刚刚';
  if (diff < 60) return `${Math.floor(diff)} 秒前`;
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
  if (diff < 86400 * 2) return `昨天 ${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
  if (diff < 86400 * 7) return `${Math.floor(diff / 86400)} 天前`;
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
}

/** 绝对时间（本地时区）：2026-10-21 20:34:56 */
export function formatAbsoluteTime(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ` +
    `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`;
}

/** 字节数可读化：24 字节 / 1.2 KB / 3.4 MB */
export function formatBytes(n) {
  const v = Number(n);
  if (!Number.isFinite(v) || v < 0) return '';
  if (v < 1024) return `${v} 字节`;
  if (v < 1024 * 1024) return `${(v / 1024).toFixed(1)} KB`;
  return `${(v / 1024 / 1024).toFixed(1)} MB`;
}

// ---------------------------------------------------------------------------
// Toast
// ---------------------------------------------------------------------------

/**
 * 全局 toast 提示。type: info(默认) / ok / warn / error
 * 消息一律 textContent 写入（消息里可能包含设备名等外部字符串）。
 */
export function toast(message, type = 'info') {
  const box = document.getElementById('toasts');
  if (!box) return;
  const el = document.createElement('div');
  el.className = `toast toast-${type}`;
  el.textContent = message;
  box.appendChild(el);
  requestAnimationFrame(() => el.classList.add('show'));
  setTimeout(() => {
    el.classList.remove('show');
    setTimeout(() => el.remove(), 350);
  }, 2600);
}

// ---------------------------------------------------------------------------
// 剪贴板复制（含非安全上下文降级）
// ---------------------------------------------------------------------------

/**
 * 复制文本到系统剪贴板。
 * 返回值：
 *   'ok'       — navigator.clipboard.writeText 成功
 *   'fallback' — 降级 document.execCommand('copy') 成功
 *   'failed'   — 两种方式都失败（调用方应引导用户手动选择复制）
 */
export async function copyText(text) {
  const value = String(text == null ? '' : text);
  if (globalThis.navigator && navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(value);
      return 'ok';
    } catch { /* 权限被拒或页面失焦 → 降级 */ }
  }
  // 降级方案：临时 textarea + execCommand('copy')（HTTP 局域网下 navigator.clipboard 不存在）
  const ta = document.createElement('textarea');
  ta.value = value;
  ta.setAttribute('readonly', '');
  ta.style.cssText = 'position:fixed;left:-9999px;top:0;opacity:0;';
  document.body.appendChild(ta);
  const prevFocus = document.activeElement;
  ta.focus();
  ta.select();
  ta.setSelectionRange(0, value.length);
  let ok = false;
  try { ok = document.execCommand('copy'); } catch { ok = false; }
  ta.remove();
  if (prevFocus && typeof prevFocus.focus === 'function') prevFocus.focus();
  return ok ? 'fallback' : 'failed';
}

// ---------------------------------------------------------------------------
// 杂项
// ---------------------------------------------------------------------------

/** 随机 ID（小写字母数字，去掉易混淆字符） */
export function randomId(n = 5) {
  const chars = 'abcdefghjkmnpqrstuvwxyz23456789';
  let out = '';
  if (globalThis.crypto && crypto.getRandomValues) {
    const buf = new Uint32Array(n);
    crypto.getRandomValues(buf);
    for (let i = 0; i < n; i++) out += chars[buf[i] % chars.length];
  } else {
    for (let i = 0; i < n; i++) out += chars[Math.floor(Math.random() * chars.length)];
  }
  return out;
}

/** 由设备名生成稳定的色相（0-359），用于来源徽标配色 */
export function deviceHue(name) {
  const s = String(name || '');
  let h = 0;
  for (const ch of s) h = (h * 31 + ch.codePointAt(0)) >>> 0;
  return h % 360;
}

// ---------------------------------------------------------------------------
// 图片辅助（v1.1 图片同步）
// ---------------------------------------------------------------------------

/** File/Blob → dataURL（FileReader Promise 封装） */
export function fileToDataURL(file) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(String(r.result));
    r.onerror = () => reject(new Error('读取图片失败'));
    r.readAsDataURL(file);
  });
}

/** dataURL → 纯 base64（去掉 data:image/png;base64, 前缀） */
export function dataURLToBase64(dataURL) {
  const i = dataURL.indexOf(';base64,');
  return i >= 0 ? dataURL.slice(i + ';base64,'.length) : dataURL;
}

/**
 * 读取图片像素尺寸。createImageBitmap（安全上下文）优先，退回 Image + objectURL。
 * 两条路都失败时返回 { width: 0, height: 0 }（服务端会自行解码 png/jpeg/gif）。
 */
export async function readImageDims(file) {
  try {
    if (typeof createImageBitmap === 'function') {
      const bmp = await createImageBitmap(file);
      const dims = { width: bmp.width, height: bmp.height };
      bmp.close && bmp.close();
      return dims;
    }
  } catch { /* 走 Image 兜底 */ }
  return new Promise((resolve) => {
    const url = URL.createObjectURL(file);
    const img = new Image();
    img.onload = () => {
      resolve({ width: img.naturalWidth, height: img.naturalHeight });
      URL.revokeObjectURL(url);
    };
    img.onerror = () => {
      resolve({ width: 0, height: 0 });
      URL.revokeObjectURL(url);
    };
    img.src = url;
  });
}

/** 防抖（搜索框输入用） */
export function debounce(fn, ms) {
  let timer = null;
  return (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), ms);
  };
}
