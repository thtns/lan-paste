/**
 * ui.js — 历史列表渲染（v2：卡片流 + 按天分组 + 置顶 + 筛选/搜索 + 图片）
 *
 * ClipListView 持有全部条目（id → clip），任何状态变化（增删改、筛选、搜索、
 * 置顶、展开）都整表重绘。条目上限 200（协议 limit），全量重绘成本可忽略。
 *
 * 安全约定：所有用户文本一律 textContent 写入，不使用 innerHTML 拼接；
 * 图片 src 只允许来自 fetch 的 objectURL（Blob URL），不直接拼服务端地址。
 */

import { formatRelativeTime, formatAbsoluteTime, formatBytes, deviceHue } from './utils.js';

const PINS_KEY = 'lanpaste.pins';

function h(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

function svgIcon(paths, cls) {
  const NS = 'http://www.w3.org/2000/svg';
  const el = document.createElementNS(NS, 'svg');
  el.setAttribute('viewBox', '0 0 24 24');
  el.setAttribute('fill', 'none');
  el.setAttribute('stroke', 'currentColor');
  el.setAttribute('stroke-width', '2');
  el.setAttribute('stroke-linecap', 'round');
  el.setAttribute('stroke-linejoin', 'round');
  el.setAttribute('aria-hidden', 'true');
  const p = document.createElementNS(NS, 'path');
  p.setAttribute('d', paths);
  el.appendChild(p);
  if (cls) el.classList.add(cls);
  return el;
}

const ICONS = {
  pin: 'M12 17v5M9 10.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V6h1a2 2 0 0 0 0-4H8a2 2 0 0 0 0 4h1z',
  trash: 'M3 6h18M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2',
  image: 'M3 3h18v18H3zM3 3l18 18',
  globe: 'M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20zM2 12h20M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z',
};

/** 文本内容的展示类型（纯展示，不影响存储） */
function textKind(text) {
  const t = String(text || '').trim();
  if (/^https?:\/\/\S+$/i.test(t) ||
      (/^https?:\/\/[^\s]+\s[—-]/.test(t) && t.length < 500)) return 'link';
  if (/^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$/.test(t)) return 'color';
  const lines = t.split('\n');
  if (/\b(function|const |let |var |import |export |def |class |package |docker |curl |git |SELECT |INSERT )/.test(t) ||
      (lines.length > 2 && /[{};=]/.test(t) && !/[。，、！？]/.test(t))) return 'code';
  return 'text';
}

const KIND_LABEL = { text: '文本', code: '代码', link: '链接', color: '颜色', image: '图片' };

function dayLabel(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '更早';
  const start = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const diff = (start(new Date()) - start(d)) / 86400000;
  if (diff < 1) return '今天';
  if (diff < 2) return '昨天';
  return `${d.getMonth() + 1}月${d.getDate()}日`;
}

export class ClipListView {
  /**
   * @param {HTMLElement} groupsEl 分组容器
   * @param {{emptyEl: HTMLElement, noMatchEl: HTMLElement}} els 空状态元素
   * @param {{ownDevice: string,
   *          onCopy?: (clip: object) => void,
   *          onDelete?: (clip: object) => void,
   *          onZoom?: (clip: object) => void,
   *          loadImage?: (clip: object, imgEl: HTMLImageElement, boxEl: HTMLElement) => void}} opts
   */
  constructor(groupsEl, els, opts) {
    this.groupsEl = groupsEl;
    this.emptyEl = els.emptyEl;
    this.noMatchEl = els.noMatchEl;
    this.ownDevice = opts.ownDevice;
    this.onCopy = opts.onCopy;
    this.onDelete = opts.onDelete;
    this.onZoom = opts.onZoom;
    this.loadImage = opts.loadImage;

    this.clips = new Map();   // id -> clip
    this.filter = 'all';      // all | text | image
    this.query = '';
    this.pins = this.loadPins();
    this.expanded = new Set(); // 保持整表重绘后的展开状态
  }

  // ---- 置顶（本地展示层，不进协议） ----

  loadPins() {
    try {
      const raw = JSON.parse(localStorage.getItem(PINS_KEY) || '[]');
      return new Set(Array.isArray(raw) ? raw.map(Number) : []);
    } catch { return new Set(); }
  }

  savePins() {
    try { localStorage.setItem(PINS_KEY, JSON.stringify([...this.pins])); } catch { /* 隐私模式等 */ }
  }

  isPinned(id) { return this.pins.has(Number(id)); }

  togglePin(clip) {
    const id = Number(clip.id);
    if (this.pins.has(id)) this.pins.delete(id);
    else this.pins.add(id);
    this.savePins();
    this.render();
  }

  // ---- 数据入口（与 app.js 的契约） ----

  replaceAll(clips) {
    this.clips.clear();
    const sorted = [...(clips || [])].sort((a, b) =>
      String(b.created_at || '').localeCompare(String(a.created_at || '')));
    for (const c of sorted) this.clips.set(c.id, c);
    this.render();
  }

  upsert(clip) {
    if (!clip || clip.id == null) return;
    this.clips.set(clip.id, clip);
    this.render();
  }

  remove(id) {
    this.clips.delete(id);
    this.pins.delete(Number(id));
    this.savePins();
    this.render();
  }

  count() { return this.clips.size; }

  /** 30s 定时刷新相对时间（不整表重绘，保持图片与展开状态） */
  refreshTimes() {
    for (const el of this.groupsEl.querySelectorAll('.clip-time')) {
      const idStr = el.closest('.clip')?.dataset.id;
      if (idStr == null) continue;
      const clip = this.clips.get(Number(idStr));
      if (!clip) continue;
      el.textContent = formatRelativeTime(clip.created_at);
      el.title = formatAbsoluteTime(clip.created_at);
    }
  }

  // ---- 筛选 ----

  visible() {
    const q = this.query.trim().toLowerCase();
    const out = [];
    for (const clip of this.clips.values()) {
      if (this.filter === 'text' && clip.type !== 'text') continue;
      if (this.filter === 'image' && clip.type !== 'image') continue;
      if (q) {
        const hay = `${clip.text || ''} ${clip.source_device || ''} ${clip.mime || ''}`.toLowerCase();
        if (!hay.includes(q)) continue;
      }
      out.push(clip);
    }
    return out.sort((a, b) =>
      String(b.created_at || '').localeCompare(String(a.created_at || '')));
  }

  setFilter(f) { this.filter = f; this.render(); }
  setQuery(q) { this.query = q; this.render(); }

  updateCounts() {
    let text = 0, image = 0;
    for (const c of this.clips.values()) {
      if (c.type === 'image') image++;
      else text++;
    }
    const set = (id, n) => {
      const el = document.getElementById(id);
      if (el) el.textContent = String(n);
    };
    set('cAll', this.clips.size);
    set('cText', text);
    set('cImg', image);
  }

  // ---- 渲染 ----

  render() {
    this.updateCounts();
    this.groupsEl.textContent = '';

    const list = this.visible();
    const pins = list.filter((c) => this.isPinned(c.id));
    const rest = list.filter((c) => !this.isPinned(c.id));

    if (this.clips.size === 0) {
      this.emptyEl.hidden = false;
      this.noMatchEl.hidden = true;
      return;
    }
    this.emptyEl.hidden = true;
    this.noMatchEl.hidden = list.length > 0;

    if (pins.length) {
      const head = h('div', 'day pin');
      head.appendChild(svgIcon(ICONS.pin));
      head.appendChild(document.createTextNode('置顶'));
      this.groupsEl.appendChild(head);
      const grid = h('div', 'grid');
      for (const clip of pins) grid.appendChild(this.buildEl(clip));
      this.groupsEl.appendChild(grid);
    }

    let lastDay = null;
    let grid = null;
    for (const clip of rest) {
      const d = dayLabel(clip.created_at);
      if (d !== lastDay) {
        const head = h('div', 'day');
        head.dataset.role = 'day';
        head.textContent = d;
        this.groupsEl.appendChild(head);
        grid = h('div', 'grid');
        this.groupsEl.appendChild(grid);
        lastDay = d;
      }
      grid.appendChild(this.buildEl(clip));
    }
  }

  buildEl(clip) {
    const li = h('article', 'clip');
    li.dataset.id = String(clip.id);

    // ---- 头部：类型徽标 + 设备 + 时间 ----
    const head = h('div', 'clip-head');
    const kind = clip.type === 'image' ? 'image' : textKind(clip.text);
    head.appendChild(h('span', `badge t-${kind}`, KIND_LABEL[kind]));

    const dev = h('span', 'clip-device');
    const dot = document.createElement('i');
    dot.style.setProperty('--h', String(deviceHue(clip.source_device || '')));
    dev.appendChild(dot);
    const isOwn = clip.source_device === this.ownDevice;
    dev.appendChild(document.createTextNode(isOwn
      ? `${clip.source_device || '未知设备'}（本机）`
      : (clip.source_device || '未知设备')));
    if (isOwn) dev.appendChild(h('span', 'self-tag', '本机'));
    head.appendChild(dev);

    const time = h('span', 'clip-time', formatRelativeTime(clip.created_at));
    time.title = formatAbsoluteTime(clip.created_at);
    head.appendChild(time);
    li.appendChild(head);

    // ---- 内容 ----
    if (clip.type === 'image') {
      li.appendChild(this.buildImageBody(clip));
    } else {
      li.appendChild(this.buildTextBody(clip, kind));
    }

    // ---- 元信息 ----
    const meta = h('div', 'clip-meta');
    if (clip.type === 'image') {
      const icon = svgIcon(ICONS.image);
      meta.appendChild(icon);
      meta.appendChild(document.createTextNode([
        clip.width && clip.height ? `${clip.width}×${clip.height}` : '',
        formatBytes(clip.size),
        (clip.mime || '').replace('image/', '').toUpperCase(),
      ].filter(Boolean).join(' · ')));
    } else {
      meta.textContent = [
        formatAbsoluteTime(clip.created_at),
        formatBytes(clip.size),
        clip.hash ? `${String(clip.hash).slice(0, 12)}…` : '',
      ].filter(Boolean).join(' · ');
    }
    li.appendChild(meta);

    // ---- 操作 ----
    const actions = h('div', 'clip-actions');
    const copyBtn = h('button', 'btn', '复制');
    copyBtn.type = 'button';
    copyBtn.title = clip.type === 'image' ? '复制图片到剪贴板' : '复制到系统剪贴板';
    copyBtn.addEventListener('click', () => this.onCopy && this.onCopy(clip));
    actions.appendChild(copyBtn);

    const pinBtn = h('button', 'icon-btn sm' + (this.isPinned(clip.id) ? ' pinned' : ''));
    pinBtn.type = 'button';
    pinBtn.title = this.isPinned(clip.id) ? '取消置顶' : '置顶';
    pinBtn.setAttribute('aria-label', pinBtn.title);
    pinBtn.appendChild(svgIcon(ICONS.pin));
    pinBtn.addEventListener('click', () => this.togglePin(clip));
    actions.appendChild(pinBtn);

    const spacer = h('span', 'spacer');
    actions.appendChild(spacer);

    const delBtn = h('button', 'icon-btn sm danger');
    delBtn.type = 'button';
    delBtn.title = '从服务器删除这条记录';
    delBtn.setAttribute('aria-label', '删除');
    delBtn.appendChild(svgIcon(ICONS.trash));
    delBtn.addEventListener('click', () => this.onDelete && this.onDelete(clip));
    actions.appendChild(delBtn);
    li.appendChild(actions);

    return li;
  }

  buildTextBody(clip, kind) {
    const text = clip.text == null ? '' : String(clip.text);
    const box = h('div');

    if (kind === 'code') {
      const pre = h('pre', 'clip-code', text);
      box.appendChild(pre);
      return box;
    }

    if (kind === 'link') {
      const m = text.match(/https?:\/\/[^\s]+/);
      const url = m ? m[0] : text;
      const rest = text.slice(url.length).replace(/^[\s—–-]+/, '');
      let host = url;
      try { host = new URL(url).host; } catch { /* 保持原文 */ }

      const hostRow = h('div', 'link-host');
      hostRow.appendChild(svgIcon(ICONS.globe));
      hostRow.appendChild(h('span', null, host));
      box.appendChild(hostRow);

      const a = h('a', 'clip-link', url);
      a.href = url;
      a.target = '_blank';
      a.rel = 'noreferrer noopener';
      box.appendChild(a);

      if (rest) box.appendChild(h('div', 'link-desc', rest));
      return box;
    }

    if (kind === 'color') {
      const row = h('div', 'swatch');
      const chip = h('span', 'chip');
      chip.style.background = text;
      row.appendChild(chip);
      row.appendChild(h('code', null, text));
      box.appendChild(row);
      return box;
    }

    // 普通文本：长内容折叠
    const long = text.length > 260 || text.split('\n').length > 8;
    const pre = h('pre', 'clip-text' + (long && !this.expanded.has(clip.id) ? ' clamp' : ''), text);
    box.appendChild(pre);

    if (long) {
      const btn = h('button', 'expand-btn',
        this.expanded.has(clip.id) ? '收起' : '展开');
      btn.type = 'button';
      btn.addEventListener('click', () => {
        const collapsed = pre.classList.toggle('clamp');
        pre.classList.toggle('expanded', !collapsed);
        if (collapsed) this.expanded.delete(clip.id);
        else this.expanded.add(clip.id);
        btn.textContent = collapsed ? '展开' : '收起';
      });
      box.appendChild(btn);
    }
    return box;
  }

  buildImageBody(clip) {
    const box = h('div');
    const thumb = h('div', 'clip-thumb');
    thumb.title = '点击查看大图';
    const img = document.createElement('img');
    img.alt = '剪贴板图片';
    img.loading = 'lazy';
    thumb.appendChild(img);
    thumb.addEventListener('click', () => this.onZoom && this.onZoom(clip));
    box.appendChild(thumb);

    // 通过回调加载（app.js 负责 fetch blob → objectURL 与缓存）
    if (this.loadImage) this.loadImage(clip, img, thumb);
    return box;
  }

  /** 自动复制失败时的兜底：展开全文并选中 */
  expandAndSelect(id) {
    const el = this.groupsEl.querySelector(`.clip[data-id="${CSS.escape(String(id))}"] .clip-text`);
    if (!el) return;
    el.classList.remove('clamp');
    el.classList.add('expanded');
    this.expanded.add(Number(id));
    const range = document.createRange();
    range.selectNodeContents(el);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  }
}
