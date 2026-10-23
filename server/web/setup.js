/**
 * setup.js — 部署向导页逻辑
 *
 * - 未配置（configured=false）：直接展示向导，保存无需 token
 * - 已配置：先输入管理令牌解锁，解锁后展示并预填当前配置（数据库密码不回显）
 * - 测试连接 / 保存：走 /api/setup/test 与 /api/setup；保存成功后服务端热切换存储
 * - 管理令牌仅用于服务器管理（解锁向导 / 设置接口）；设备接入使用账号登录
 */

import { toast } from './utils.js';

const $ = (id) => document.getElementById(id);
const els = {
  authCard: $('authCard'),
  authToken: $('authToken'),
  authBtn: $('authBtn'),
  authError: $('authError'),
  body: $('wizardBody'),
  loading: $('wizardLoading'),
  storeOpts: $('storeOpts'),
  dbCard: $('dbCard'),
  dbHost: $('dbHost'),
  dbPort: $('dbPort'),
  dbUser: $('dbUser'),
  dbPass: $('dbPass'),
  dbName: $('dbName'),
  sslRow: $('sslRow'),
  dbSSL: $('dbSSL'),
  listen: $('listen'),
  tokenText: $('tokenText'),
  regenBtn: $('regenBtn'),
  testBtn: $('testBtn'),
  testResult: $('testResult'),
  saveBtn: $('saveBtn'),
  saveResult: $('saveResult'),
  skipBtn: $('skipBtn'),
  qrLink: $('qrLink'),
  settingsLink: $('settingsLink'),
};

const state = {
  token: '',        // 已配置时用于授权的管理令牌（页面内存，不落 localStorage）
  knownToken: '',   // 服务端当前生效的管理令牌
  configured: false,
  status: null,
};

// ---- 基础请求 ----

async function api(method, path, body) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), 30000);
  try {
    const headers = { 'Content-Type': 'application/json; charset=utf-8' };
    if (state.token) headers.Authorization = `Bearer ${state.token}`;
    const res = await fetch(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: ctrl.signal,
    });
    let data = null;
    const text = await res.text();
    if (text) { try { data = JSON.parse(text); } catch { /* ignore */ } }
    if (!res.ok) {
      const err = new Error((data && (data.error || data.message)) || `请求失败（HTTP ${res.status}）`);
      err.status = res.status;
      err.data = data;
      throw err;
    }
    return data;
  } catch (err) {
    if (err.name === 'AbortError') throw new Error('请求超时');
    throw err;
  } finally {
    clearTimeout(timer);
  }
}

function setResult(el, cls, text) {
  el.hidden = !text;
  el.className = `wizard-result ${cls || ''}`;
  el.textContent = text || '';
}

// ---- 表单读写 ----

function currentStore() {
  const active = els.storeOpts.querySelector('.store-opt.active');
  return active ? active.dataset.v : 'memory';
}

function selectStore(v) {
  for (const opt of els.storeOpts.querySelectorAll('.store-opt')) {
    const active = opt.dataset.v === v;
    opt.classList.toggle('active', active);
    opt.querySelector('input').checked = active;
  }
  els.dbCard.hidden = v === 'memory';
  els.sslRow.hidden = v !== 'postgres';
}

function formConfig() {
  const port = parseInt(els.dbPort.value, 10);
  return {
    listen: els.listen.value.trim() || ':8080',
    db: {
      store: currentStore(),
      host: els.dbHost.value.trim(),
      port: Number.isFinite(port) ? port : 0,
      user: els.dbUser.value.trim(),
      password: els.dbPass.value, // 留空：已配置时不修改原密码（服务端约定）
      database: els.dbName.value.trim(),
      sslmode: els.dbSSL.value.trim(),
    },
  };
}

function prefill(status) {
  selectStore(status.store || 'memory');
  els.dbHost.value = status.db?.host || '';
  els.dbPort.value = status.db?.port || '';
  els.dbUser.value = status.db?.user || '';
  els.dbPass.value = ''; // 密码不回显
  els.dbName.value = status.db?.database || 'paste';
  els.dbSSL.value = status.db?.sslmode || 'disable';
  els.listen.value = status.listen || ':8080';
  updateLinks();
}

/** 入口链接（多用户后二维码只含服务端地址，手机打开后用账号登录） */
function updateLinks() {
  if (els.qrLink) els.qrLink.href = '/download';
  if (els.settingsLink) els.settingsLink.href = 'settings.html' + (state.token ? `?token=${encodeURIComponent(state.token)}` : '');
}

// ---- 初始化 ----

async function loadStatus() {
  els.loading.hidden = false;
  try {
    const data = await api('GET', '/api/setup');
    state.configured = !!data.configured;
    state.status = data.status;
    state.knownToken = data.token || '';
    const vEl = document.getElementById('srvVersion');
    if (vEl && data.version) vEl.textContent = `服务端版本 v${data.version}`;
    els.tokenText.textContent = data.token || '（不可用）';
    if (state.configured) {
      // 已配置：先解锁。URL 带 ?token= 时自动尝试一次（从主页面跳转的便利路径）
      const urlTok = new URLSearchParams(location.search).get('token');
      if (urlTok) {
        state.token = urlTok;
        try {
          await api('GET', '/api/setup');
          return enterWizard();
        } catch { /* token 无效，走手动输入 */ }
      }
      els.authCard.hidden = false;
      els.authToken.focus();
    } else {
      enterWizard();
    }
  } catch (err) {
    els.loading.hidden = true;
    els.authCard.hidden = false;
    if (err.status === 401) {
      // 服务端已配置：匿名读取被拒是预期状态，静默进入解锁流程，不当错误处理
      setResult(els.authError, '', '');
      els.authToken.focus();
      return;
    }
    // 其余错误（如服务端过旧没有该端点）：提示但仍允许手动填写
    toast(`读取服务端状态失败：${err.message}`, 'error');
    setResult(els.authError, 'err', `读取服务端状态失败：${err.message}（可直接跳过向导使用）`);
  }
}

function enterWizard() {
  els.loading.hidden = true;
  els.authCard.hidden = true;
  els.body.hidden = false;
  els.skipBtn.hidden = state.configured; // 已配置就没有「跳过」的概念
  updateLinks();
  if (state.status) prefill(state.status);
}

// ---- 事件 ----

els.storeOpts.addEventListener('click', (ev) => {
  const opt = ev.target.closest('.store-opt');
  if (opt) selectStore(opt.dataset.v);
});
els.storeOpts.addEventListener('change', (ev) => {
  if (ev.target.name === 'store') selectStore(ev.target.value);
});

els.authBtn.addEventListener('click', async () => {
  state.token = els.authToken.value.trim();
  setResult(els.authError, '', '');
  if (!state.token) {
    setResult(els.authError, 'err', '请输入 token');
    return;
  }
  try {
    const data = await api('GET', '/api/setup');
    state.configured = !!data.configured;
    state.status = data.status;
    state.knownToken = data.token || state.knownToken;
    els.tokenText.textContent = data.token || '（不可用）';
    enterWizard();
  } catch (err) {
    setResult(els.authError, 'err', err.status === 401 ? 'token 校验失败，请检查' : err.message);
  }
});
els.authToken.addEventListener('keydown', (ev) => {
  if (ev.key === 'Enter') els.authBtn.click();
});

els.testBtn.addEventListener('click', async () => {
  const cfg = formConfig();
  if (cfg.db.store === 'memory') {
    setResult(els.testResult, 'ok', '内存存储无需测试，直接保存即可。');
    return;
  }
  if (!cfg.db.host || !cfg.db.database) {
    setResult(els.testResult, 'err', '请先填写主机与数据库名');
    return;
  }
  els.testBtn.disabled = true;
  els.testBtn.textContent = '测试中…';
  setResult(els.testResult, '', '');
  try {
    await api('POST', '/api/setup/test', cfg);
    setResult(els.testResult, 'ok', '连接成功，可以保存了。');
  } catch (err) {
    const extra = err.data && err.data.error ? `：${err.data.error}` : `：${err.message}`;
    setResult(els.testResult, 'err', '连接失败' + extra);
  } finally {
    els.testBtn.disabled = false;
    els.testBtn.textContent = '测试连接';
  }
});

els.saveBtn.addEventListener('click', async () => {
  const cfg = formConfig();
  els.saveBtn.disabled = true;
  els.saveBtn.textContent = '保存中…';
  setResult(els.saveResult, '', '');
  try {
    const out = await api('POST', '/api/setup', cfg);
    if (out && out.ok === false) {
      setResult(els.saveResult, 'err', out.error || '保存失败');
      return;
    }
    if (out && out.restart_required) {
      els.saveResult.className = 'wizard-result ok';
      els.saveResult.hidden = false;
      els.saveResult.textContent =
        `配置已保存，存储已启用。监听地址变更为 ${out.listen || cfg.listen}，需重启服务生效：systemctl restart lanpaste。`;
      const link = document.createElement('a');
      link.className = 'linklike';
      link.href = './';
      link.textContent = '重启完成后，点此进入主页面（使用账号登录）';
      els.saveResult.appendChild(document.createElement('br'));
      els.saveResult.appendChild(link);
      toast('配置已保存');
    } else {
      // 保存成功：回主页面（账号登录流程）
      toast('配置已保存，正在返回主页面…');
      setTimeout(() => { location.href = './'; }, 700);
    }
  } catch (err) {
    const extra = err.data && err.data.error ? `：${err.data.error}` : `：${err.message}`;
    setResult(els.saveResult, 'err', '保存失败' + extra);
  } finally {
    els.saveBtn.disabled = false;
    els.saveBtn.textContent = '保存并启用';
  }
});

els.regenBtn.addEventListener('click', async () => {
  if (!confirm('确定重新生成管理令牌？解锁部署向导/设置接口需要使用新令牌。')) return;
  els.regenBtn.disabled = true;
  try {
    const out = await api('POST', '/api/setup/token', {});
    els.tokenText.textContent = out.token;
    state.knownToken = out.token || state.knownToken;
    updateLinks();
    toast('管理令牌已重新生成', 'warn');
  } catch (err) {
    toast(`重新生成失败：${err.message}`, 'error');
  } finally {
    els.regenBtn.disabled = false;
  }
});

els.skipBtn.addEventListener('click', () => {
  try { localStorage.setItem('lanpaste.setupDismissed', '1'); } catch { /* ignore */ }
  location.href = './';
});

loadStatus();
