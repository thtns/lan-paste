/**
 * app.js — LAN Paste Web 前端入口（v2）
 *
 * 职责：
 * - 配置管理（localStorage）：服务器地址 / token / 设备名，首次访问显示引导卡片
 * - 主题：浅色 / 深色切换，默认跟随系统，记忆选择
 * - 上传：① 输入框文本 ② 任意位置 Ctrl/Cmd+V（文本或图片）③ 图片选择器
 * - 历史卡片流：按天分组 / 置顶（本地）/ 类型徽标 / 筛选 / 搜索 / 灯箱
 * - WebSocket 实时同步：new / deleted / hello，断线状态条 + 指数退避重连
 * - 防回环（PROTOCOL.md）：lastSeenHash / lastWrittenHash 双 hash 比对（文本）
 * - 图片：fetch blob → objectURL 缓存；复制走 ClipboardItem（仅安全上下文），失败引导灯箱
 */

import { createApi, ApiError, authLogin } from './api.js';
import { SyncSocket } from './ws.js';
import { ClipListView } from './ui.js';
import {
  sha256Hex, copyText, toast, randomId,
  fileToDataURL, dataURLToBase64, readImageDims, debounce,
} from './utils.js';

const CONFIG_KEY = 'lanpaste.config';
const DEVICE_KEY = 'lanpaste.device';
const THEME_KEY = 'lanpaste.theme';
const SETUP_DISMISS_KEY = 'lanpaste.setupDismissed';

const $ = (id) => document.getElementById(id);

const els = {
  setup: $('setupCard'),
  setupForm: $('setupForm'),
  serverInput: $('serverInput'),
  deviceInput: $('deviceInput'),
  setupError: $('setupError'),
  cancelSetup: $('cancelSetup'),
  usernameInput: $('usernameInput'),
  passwordInput: $('passwordInput'),
  registerCheck: $('registerCheck'),
  saveConnectBtn: $('saveConnectBtn'),
  mainUI: $('mainUI'),
  editor: $('editor'),
  syncBtn: $('syncBtn'),
  refreshBtn: $('refreshBtn'),
  settingsBtn: $('settingsBtn'),
  wizardBtn: $('wizardBtn'),
  themeBtn: $('themeBtn'),
  qrBtn: $('qrBtn'),
  connPill: $('connPill'),
  connText: $('connText'),
  statusBar: $('statusBar'),
  statusText: $('statusText'),
  deviceLabel: $('deviceLabel'),
  seg: $('seg'),
  searchInput: $('searchInput'),
  groups: $('groups'),
  emptyHint: $('emptyHint'),
  noMatch: $('noMatch'),
  listLoading: $('listLoading'),
  lightbox: $('lightbox'),
  lightboxImg: $('lightboxImg'),
  lightboxMeta: $('lightboxMeta'),
};

const state = {
  started: false,
  config: null,
  api: null,
  socket: null,
  view: null,
  theme: null,
  lastSeenHash: null,    // 最近一次由本页上传的内容 hash（文本）
  lastWrittenHash: null, // 最近一次写入系统剪贴板的内容 hash
  blobUrls: new Map(),    // clip.id -> objectURL 缓存
  blobPending: new Map(), // clip.id -> 正在加载的 Promise（并发渲染共用同一请求）
};

// ---------------------------------------------------------------------------
// 主题
// ---------------------------------------------------------------------------

function applyTheme(t) {
  state.theme = t;
  document.documentElement.setAttribute('data-theme', t);
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute('content', t === 'dark' ? '#0f1117' : '#4f46e5');
}

function initTheme() {
  let saved = null;
  try { saved = localStorage.getItem(THEME_KEY); } catch { /* ignore */ }
  if (saved !== 'light' && saved !== 'dark') {
    saved = (window.matchMedia && matchMedia('(prefers-color-scheme: dark)').matches)
      ? 'dark' : 'light';
  }
  applyTheme(saved);
}

function toggleTheme() {
  const next = state.theme === 'dark' ? 'light' : 'dark';
  applyTheme(next);
  try { localStorage.setItem(THEME_KEY, next); } catch { /* ignore */ }
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

function loadConfig() {
  try {
    const raw = localStorage.getItem(CONFIG_KEY);
    const cfg = raw ? JSON.parse(raw) : null;
    if (cfg && cfg.server && cfg.token) return cfg;
  } catch { /* 损坏的配置视为未配置 */ }
  return null;
}

function getDeviceId() {
  let d = localStorage.getItem(DEVICE_KEY);
  if (!d) {
    d = 'web-' + randomId(5);
    localStorage.setItem(DEVICE_KEY, d);
  }
  return d;
}

/** 归一化服务器地址：自动补 http://、去路径与末尾斜杠；非法返回 null */
function normalizeServer(input) {
  let s = String(input || '').trim();
  if (!s) return null;
  if (!/^https?:\/\//i.test(s)) s = 'http://' + s;
  let u;
  try { u = new URL(s); } catch { return null; }
  if (!/^https?:$/.test(u.protocol) || !u.hostname) return null;
  return u.origin;
}

// ---------------------------------------------------------------------------
// 首次访问引导 / 设置卡片
// ---------------------------------------------------------------------------

function openSetup({ initial = false, error = '' } = {}) {
  const cfg = state.config || loadConfig();
  els.setup.hidden = false;
  els.mainUI.hidden = true;
  els.setupError.hidden = !error;
  els.setupError.textContent = error;
  els.cancelSetup.hidden = initial;
  els.serverInput.value = cfg ? cfg.server : location.origin;
  els.deviceInput.value = localStorage.getItem(DEVICE_KEY) || 'web-' + randomId(5);
  (cfg ? els.usernameInput : els.serverInput).focus();
}

function closeSetup() {
  if (!state.config) return; // 从未配置成功过，没有可返回的界面
  els.setup.hidden = true;
  if (state.started) els.mainUI.hidden = false;
}

async function onSetupSubmit(ev) {
  ev.preventDefault();
  const server = normalizeServer(els.serverInput.value);
  let device = els.deviceInput.value.trim() || localStorage.getItem(DEVICE_KEY) || 'web-' + randomId(5);
  if (device.length > 32) device = device.slice(0, 32);
  const fail = (msg) => {
    els.setupError.textContent = msg;
    els.setupError.hidden = false;
  };
  if (!server) {
    fail('服务器地址无效，示例：http://192.168.1.10:8080');
    return;
  }

  // 账号登录 / 注册（v1.2 多用户）
  const username = els.usernameInput.value.trim();
  const password = els.passwordInput.value;
  if (!username || !password) {
    fail('请填写用户名与密码');
    return;
  }
  if (password.length < 6) {
    fail('密码至少 6 位');
    return;
  }

  els.setupError.hidden = true;
  els.saveConnectBtn.disabled = true;
  els.saveConnectBtn.textContent = els.registerCheck.checked ? '注册并连接…' : '连接中…';
  try {
    const { token: deviceToken, user } = await authLogin(
      server, username, password, device, els.registerCheck.checked);
    localStorage.setItem(DEVICE_KEY, device);
    localStorage.setItem(CONFIG_KEY, JSON.stringify({
      server, token: deviceToken, device, user, savedAt: Date.now(),
    }));
    location.reload(); // 重建 api / ws / 视图，状态最干净
  } catch (err) {
    fail(err.message);
    els.saveConnectBtn.disabled = false;
    els.saveConnectBtn.textContent = '保存并连接';
  }
}

/**
 * 未完成部署向导时引导过去（服务端 /api/setup 报告 configured=false）。
 * 用户可在向导页「跳过」，用 SETUP_DISMISS_KEY 避免重复打扰（开发 / 内存模式）。
 * 401 = 服务端已配置（属正常，静默返回）；同时给未配置本机的用户一行提示。
 */
async function maybeRedirectToWizard() {
  try {
    const res = await fetch('/api/setup', { signal: AbortSignal.timeout ? AbortSignal.timeout(5000) : undefined });
    if (res.status === 401) {
      // 服务端已配置过，匿名访问被拒是预期行为；提示使用账号登录
      if (!state.config && !els.setup.hidden) {
        els.setupError.hidden = false;
        els.setupError.textContent =
          '服务端已就绪。输入用户名与密码登录即可（首次使用勾选「注册新用户」）。';
      }
      return;
    }
    if (!res.ok) return;
    const data = await res.json();
    let dismissed = false;
    try { dismissed = localStorage.getItem(SETUP_DISMISS_KEY) === '1'; } catch { /* ignore */ }
    if (data && data.configured === false && !dismissed) {
      location.replace('setup.html');
    }
  } catch { /* 服务端旧版本或网络问题：不影响正常使用 */ }
}

/**
 * 向导保存成功后 / 托盘「打开历史页面」会带着 ?token= 跳到主页面：
 * 本机没有配置，或存的令牌与 URL 里的不同（重新登录/换账号签发的新令牌），
 * 就用 URL 令牌覆盖直连；随后立即从地址栏抹掉 token。
 */
function consumeUrlToken() {
  let tok = null;
  try { tok = new URLSearchParams(location.search).get('token'); } catch { /* ignore */ }
  if (!tok) return;
  history.replaceState(null, '', location.pathname); // 先抹掉地址栏里的 token
  const existing = loadConfig();
  if (existing && existing.token === tok) return; // 同一令牌：按原配置走
  const device = (existing && existing.device) ||
    localStorage.getItem(DEVICE_KEY) || 'web-' + randomId(5);
  try {
    localStorage.setItem(DEVICE_KEY, device);
    localStorage.setItem(CONFIG_KEY, JSON.stringify({
      server: location.origin, token: tok, device, savedAt: Date.now(),
    }));
  } catch { /* 隐私模式等极端场景：退回手动填写 */ }
}

// ---------------------------------------------------------------------------
// 启动
// ---------------------------------------------------------------------------

function start(cfg) {
  state.config = cfg;
  state.api = createApi(cfg);
  state.view = new ClipListView(els.groups, { emptyEl: els.emptyHint, noMatchEl: els.noMatch }, {
    ownDevice: cfg.device,
    onCopy: handleCopy,
    onDelete: handleDelete,
    onZoom: openLightbox,
    loadImage: loadClipImage,
  });

  els.setup.hidden = true;
  els.mainUI.hidden = false;
  els.deviceLabel.textContent = cfg.user ? `${cfg.device} · ${cfg.user}` : cfg.device;
  els.deviceLabel.title = `服务器 ${cfg.server}`;
  // 服务端设置页入口：带上 token 免二次输入
  const srvLink = document.getElementById('srvSettingsLink');
  if (srvLink) srvLink.href = `settings.html?token=${encodeURIComponent(cfg.token)}`;

  state.started = true;

  state.socket = new SyncSocket({
    server: cfg.server,
    token: cfg.token,
    device: cfg.device,
    onEvent: handleWsEvent,
    onStatus: handleWsStatus,
  });
  state.socket.connect();

  els.editor.focus();
  refreshList();
}

// ---------------------------------------------------------------------------
// 图片加载（fetch blob → objectURL，缓存复用）
// ---------------------------------------------------------------------------

async function getBlobUrl(clip) {
  if (state.blobUrls.has(clip.id)) return state.blobUrls.get(clip.id);
  // 并发渲染同一张图时共用同一个加载 Promise，谁都拿得到结果
  // （旧实现用 Set 标记后直接返回 null，等待方会一直空着）
  const pending = state.blobPending.get(clip.id);
  if (pending) return pending;
  const task = (async () => {
    try {
      const blob = await state.api.getBlob(clip.id);
      const url = URL.createObjectURL(blob);
      state.blobUrls.set(clip.id, url);
      return url;
    } finally {
      state.blobPending.delete(clip.id);
    }
  })();
  state.blobPending.set(clip.id, task);
  return task;
}

function loadClipImage(clip, imgEl, boxEl) {
  getBlobUrl(clip).then((url) => {
    imgEl.src = url;
  }).catch(() => {
    boxEl.classList.add('failed');
    boxEl.textContent = '图片加载失败（已删除或网络异常）';
  });
}

// ---------------------------------------------------------------------------
// 上传（文本 / 图片；含防回环判断）
// ---------------------------------------------------------------------------

async function uploadText(text, { clearEditorOnSuccess = false } = {}) {
  try {
    const hash = await sha256Hex(text);
    if (hash === state.lastWrittenHash) {
      state.lastSeenHash = hash;
      toast('这是刚从服务器接收的内容，已跳过上传');
      return;
    }
    if (hash === state.lastSeenHash) {
      toast('与最近一次同步的内容相同，已跳过');
      return;
    }
    const { clip, created } = await state.api.uploadClip({ text, hash, device: state.config.device });
    state.lastSeenHash = (clip && clip.hash) || hash;
    state.view.upsert(clip);
    toast(created ? '已同步到服务器' : '与服务器最新一条相同，未重复创建');
    if (clearEditorOnSuccess) els.editor.value = '';
  } catch (err) {
    handleApiError(err, '同步失败');
  }
}

async function uploadImageFile(file) {
  if (!file.type.startsWith('image/')) {
    toast('仅支持图片文件', 'warn');
    return;
  }
  if (file.size > 16 * 1024 * 1024) {
    toast('图片超过 16MB 上限', 'warn');
    return;
  }
  toast('正在上传图片…');
  try {
    const [dataURL, dims] = await Promise.all([fileToDataURL(file), readImageDims(file)]);
    const { clip, created } = await state.api.uploadImage({
      data: dataURLToBase64(dataURL),
      device: state.config.device,
      width: dims.width,
      height: dims.height,
    });
    state.lastSeenHash = (clip && clip.hash) || null;
    state.view.upsert(clip);
    toast(created ? '图片已同步到服务器' : '同一张图片已存在，未重复创建');
  } catch (err) {
    handleApiError(err, '图片上传失败');
  }
}

function syncFromEditor() {
  const text = els.editor.value;
  if (!text.trim()) {
    toast('请先输入或粘贴要同步的内容', 'warn');
    return;
  }
  uploadText(text, { clearEditorOnSuccess: true });
}

// ---------------------------------------------------------------------------
// 历史列表操作
// ---------------------------------------------------------------------------

async function refreshList({ quiet = false } = {}) {
  if (!state.api) return;
  els.listLoading.hidden = false;
  try {
    const { clips } = await state.api.listClips(100);
    state.view.replaceAll(clips || []);
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) handleApiError(err);
    else if (!quiet) toast(`加载历史失败：${err.message}`, 'error');
  } finally {
    els.listLoading.hidden = true;
  }
}

async function handleCopy(clip) {
  try {
    if (clip.type === 'image') {
      await copyImage(clip);
      return;
    }
    const res = await copyText(clip.text || '');
    if (res === 'ok' || res === 'fallback') {
      state.lastWrittenHash = clip.hash || null; // 手动复制同样记为「已写入剪贴板」
      toast(res === 'ok' ? '已复制到剪贴板' : '已复制（兼容模式）');
    } else {
      state.view.expandAndSelect(clip.id);
      toast('自动复制失败，已选中全文，请按 Ctrl / ⌘ + C 手动复制', 'warn');
    }
  } catch (err) {
    toast(`复制失败：${err.message}`, 'error');
  }
}

/** 复制图片：ClipboardItem 仅安全上下文可用；失败则打开灯箱引导手动保存 */
async function copyImage(clip) {
  const secure = window.isSecureContext &&
    navigator.clipboard && typeof navigator.clipboard.write === 'function' &&
    typeof window.ClipboardItem === 'function';
  if (secure) {
    try {
      let url = state.blobUrls.get(clip.id);
      if (!url) url = await getBlobUrl(clip);
      const blob = await fetch(url).then((r) => r.blob());
      // PNG 之外的类型部分浏览器不接受，统一转存原始 MIME 尝试
      await navigator.clipboard.write([new ClipboardItem({ [blob.type || 'image/png']: blob })]);
      toast('图片已复制到剪贴板');
      return;
    } catch { /* 落到引导 */ }
  }
  openLightbox(clip);
  toast('当前环境无法直接复制图片，已打开大图：长按 / 右键另存', 'warn');
}

async function handleDelete(clip) {
  try {
    await state.api.deleteClip(clip.id);
    revokeBlobUrl(clip.id);
    state.view.remove(clip.id);
    toast('已删除');
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) {
      revokeBlobUrl(clip.id);
      state.view.remove(clip.id); // 其他设备已删除，本地同步移除
    } else {
      handleApiError(err, '删除失败');
    }
  }
}

function revokeBlobUrl(id) {
  const url = state.blobUrls.get(id);
  if (url) {
    URL.revokeObjectURL(url);
    state.blobUrls.delete(id);
  }
}

// ---------------------------------------------------------------------------
// 灯箱
// ---------------------------------------------------------------------------

async function openLightbox(clip) {
  els.lightbox.hidden = false;
  els.lightboxImg.removeAttribute('src');
  els.lightboxMeta.textContent = [
    clip.width && clip.height ? `${clip.width}×${clip.height}` : '',
    `${clip.source_device || '未知设备'} · `,
  ].filter(Boolean).join(' ');
  try {
    let url = state.blobUrls.get(clip.id);
    if (!url) url = await getBlobUrl(clip);
    if (!els.lightbox.hidden) els.lightboxImg.src = url;
    els.lightboxMeta.textContent = [
      clip.width && clip.height ? `${clip.width}×${clip.height}` : '',
      clip.mime ? clip.mime.replace('image/', '').toUpperCase() : '',
      clip.source_device || '未知设备',
    ].filter(Boolean).join(' · ');
  } catch (err) {
    els.lightboxMeta.textContent = `图片加载失败：${err.message}`;
  }
}

function closeLightbox() {
  els.lightbox.hidden = true;
  els.lightboxImg.removeAttribute('src');
}

// ---------------------------------------------------------------------------
// WebSocket 事件（含防回环）
// ---------------------------------------------------------------------------

function handleWsEvent(msg) {
  if (msg.event === 'hello') {
    els.connPill.title = `当前在线设备 ${msg.device_count ?? '-'} 台`;
    refreshList({ quiet: true }); // 重连后补拉，弥补断线期间错过的事件
    return;
  }
  if (msg.event === 'new' && msg.clip && msg.clip.id != null) {
    handleRemoteNew(msg.clip);
  } else if (msg.event === 'deleted' && msg.id != null) {
    revokeBlobUrl(msg.id);
    state.view.remove(msg.id);
  }
}

async function handleRemoteNew(clip) {
  // 先合并进列表：已存在（同 id）则原地更新，避免与本页刚上传的记录重复显示
  state.view.upsert(clip);
  // 防回环：与本页刚上传的内容相同 → 只更新列表，不动剪贴板、不提示
  if (clip.hash && clip.hash === state.lastSeenHash) return;
  if (clip.hash && clip.hash === state.lastWrittenHash) return;

  const from = clip.source_device || '未知设备';
  if (clip.type === 'image') {
    toast(`收到来自「${from}」的图片`);
    return;
  }

  // 文本：尽力自动写入系统剪贴板（仅安全上下文且页面聚焦时成功；失败则提示手动复制）
  if (globalThis.navigator && navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(clip.text || '');
      state.lastWrittenHash = clip.hash; // 防回环第 3 步
      toast(`已自动复制来自「${from}」的内容`, 'ok');
      return;
    } catch { /* 无焦点 / 无权限 → 落到提示 */ }
  }
  toast(`收到来自「${from}」的新内容`);
}

function handleWsStatus(status, info = {}) {
  const { attempt = 0, delayMs = 0 } = info;
  els.connPill.hidden = false;
  if (status === 'online') {
    setPill('online', '已连接');
    els.statusBar.hidden = true;
    return;
  }
  if (status === 'connecting') {
    setPill('connecting', '连接中…');
    if (attempt > 0) {
      els.statusBar.hidden = false;
      els.statusText.textContent = `正在连接服务器（第 ${attempt} 次尝试）…`;
    }
    return;
  }
  // offline / scheduled
  setPill('offline', '已断开');
  els.statusBar.hidden = false;
  els.statusText.textContent = status === 'scheduled'
    ? `实时连接已断开，${Math.max(1, Math.round(delayMs / 1000))} 秒后自动重连（第 ${attempt} 次尝试）…`
    : '实时连接已断开，准备重连…';
}

function setPill(cls, text) {
  els.connPill.className = `pill pill-${cls}`;
  els.connText.textContent = text;
}

// ---------------------------------------------------------------------------
// 事件绑定
// ---------------------------------------------------------------------------

function onDocumentPaste(ev) {
  if (!state.started) return;
  // 设置表单内的粘贴（比如往 token 输入框里贴 token）不拦截
  const target = ev.target;
  if (target instanceof Node && els.setup.contains(target)) return;

  const cd = ev.clipboardData;
  if (!cd) return;

  // 图片优先：有图片文件且无文本时上传图片
  const imageFile = Array.from(cd.items || [])
    .find((it) => it && it.kind === 'file' && it.type && it.type.startsWith('image/'));
  const text = cd.getData('text/plain') || '';

  if (imageFile && !text) {
    ev.preventDefault();
    const file = imageFile.getAsFile();
    if (file) uploadImageFile(file);
    return;
  }
  if (!text) {
    const hasFile = Array.from(cd.items || []).some((it) => it && it.kind === 'file');
    if (hasFile) {
      const inField = target instanceof HTMLElement &&
        (target.tagName === 'TEXTAREA' || target.tagName === 'INPUT' || target.isContentEditable);
      if (!inField) {
        ev.preventDefault();
        toast('仅支持同步文本与图片内容', 'warn');
      }
    }
    return;
  }
  // 无论焦点在哪（包括输入框），Ctrl/Cmd+V 一律直接上传，不落入输入框
  ev.preventDefault();
  uploadText(text);
}

function onKeyDown(ev) {
  if (!state.started) return;
  if ((ev.metaKey || ev.ctrlKey) && ev.key === 'Enter') {
    ev.preventDefault();
    syncFromEditor();
  }
  if (ev.key === 'Escape' && !els.lightbox.hidden) {
    closeLightbox();
  }
}

function bindEvents() {
  els.setupForm.addEventListener('submit', onSetupSubmit);
  els.cancelSetup.addEventListener('click', closeSetup);
  els.settingsBtn.addEventListener('click', () => openSetup({ initial: !state.config }));
  els.wizardBtn.addEventListener('click', () => { location.href = 'setup.html'; });
  els.qrBtn.addEventListener('click', () => {
    if (!state.config) {
      toast('请先完成连接配置', 'warn');
      return;
    }
    // 跳转扫码接入页：带 token 时二维码编码 origin/?token=…，手机扫码自动配置
    window.open(`/download?token=${encodeURIComponent(state.config.token)}`, '_blank');
  });
  els.themeBtn.addEventListener('click', toggleTheme);
  els.syncBtn.addEventListener('click', () => { if (state.started) syncFromEditor(); });
  els.refreshBtn.addEventListener('click', () => { if (state.started) refreshList(); });

  els.seg.addEventListener('click', (ev) => {
    const btn = ev.target.closest('button[data-f]');
    if (!btn) return;
    for (const b of els.seg.querySelectorAll('button')) b.classList.toggle('active', b === btn);
    if (state.view) state.view.setFilter(btn.dataset.f);
  });
  els.searchInput.addEventListener('input', debounce(function () {
    if (state.view) state.view.setQuery(this.value);
  }, 150));

  els.lightbox.addEventListener('click', closeLightbox);

  document.addEventListener('keydown', onKeyDown);
  document.addEventListener('paste', onDocumentPaste, true); // 捕获阶段，先于输入框默认行为
}

function registerServiceWorker() {
  if (!('serviceWorker' in navigator)) return; // HTTP 局域网（非安全上下文）下不可用
  navigator.serviceWorker.register('./sw.js').catch(() => { /* 注册失败不影响使用 */ });
}

// ---------------------------------------------------------------------------
// 入口
// ---------------------------------------------------------------------------

function init() {
  initTheme();
  bindEvents();
  consumeUrlToken(); // 向导带回的 ?token= → 自动写入配置
  const cfg = loadConfig();
  if (cfg) start(cfg);
  else openSetup({ initial: true });
  registerServiceWorker();
  maybeRedirectToWizard(); // 服务端未完成部署向导时引导过去（已跳过则不动）
  setInterval(() => { if (state.view) state.view.refreshTimes(); }, 30000);
}

init();
