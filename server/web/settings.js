/**
 * settings.js — 服务设置页（运行时参数：历史保留天数等）
 *
 * - 未初始化（configured=false）：引导去部署向导（此处保存会意外创建仅内存的配置）
 * - 已配置：?token= 自动尝试，失败/缺失则手动输入解锁
 * - 保存：POST /api/setup（listen 与 db 原样回传、密码留空由服务端沿用原值，
 *   只改变 retention_days）→ 保存成功后服务端立即触发一次 TTL 清理
 */

import { toast } from './utils.js';

const $ = (id) => document.getElementById(id);
const els = {
  authCard: $('authCard'),
  authToken: $('authToken'),
  authBtn: $('authBtn'),
  authError: $('authError'),
  notSetupCard: $('notSetupCard'),
  body: $('settingsBody'),
  loading: $('settingsLoading'),
  retention: $('retention'),
  saveBtn: $('saveBtn'),
  saveResult: $('saveResult'),
  resetUser: $('resetUser'),
  resetPass: $('resetPass'),
  resetPwdBtn: $('resetPwdBtn'),
  resetResult: $('resetResult'),
};

const state = {
  token: '',      // 解锁用的 token（页面内存）
  status: null,   // GET /api/setup 的 status（listen / db 原样回传用）
  configured: false,
};

// ---- 基础请求 ----

async function api(method, path, body) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), 15000);
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

// ---- 流程 ----

async function loadStatus() {
  els.loading.hidden = false;
  // URL 带 ?token= 时先自动尝试（主页面/向导的入口链接会带上）
  const urlTok = new URLSearchParams(location.search).get('token');
  if (urlTok) {
    state.token = urlTok;
    try {
      return enter(await api('GET', '/api/setup'));
    } catch { /* token 无效：走手动输入 */ }
  }
  try {
    const data = await api('GET', '/api/setup');
    enter(data);
  } catch (err) {
    if (err.status === 401) {
      els.loading.hidden = true;
      els.authCard.hidden = false;
      els.authToken.focus();
    } else {
      els.loading.hidden = true;
      els.authCard.hidden = false;
      setResult(els.authError, 'err', `读取服务端状态失败：${err.message}`);
    }
  }
}

function enter(data) {
  state.configured = !!data.configured;
  state.status = data.status;
  els.loading.hidden = true;
  els.authCard.hidden = true;
  if (!state.configured || !state.status) {
    // 未初始化：去部署向导，避免在此处意外创建只有内存存储的配置
    els.notSetupCard.hidden = false;
    return;
  }
  els.body.hidden = false;
  els.retention.value = state.status.retention_days > 0 ? state.status.retention_days : 0;
}

async function save() {
  const n = parseInt(els.retention.value, 10);
  const retention = Number.isFinite(n) && n >= 0 ? n : 0;
  const db = state.status.db || { store: 'memory' };
  els.saveBtn.disabled = true;
  els.saveBtn.textContent = '保存中…';
  setResult(els.saveResult, '', '');
  try {
    const out = await api('POST', '/api/setup', {
      listen: state.status.listen || ':8080', // 原样回传，不变更
      retention_days: retention,
      db: { ...db, password: '' },            // 留空 = 服务端沿用原密码
    });
    if (out && out.ok === false) {
      setResult(els.saveResult, 'err', out.error || '保存失败');
      return;
    }
    setResult(els.saveResult, 'ok',
      retention > 0 ? `已保存：保留 ${retention} 天，已立即触发一次清理。` : '已保存：永不清理。');
    toast('服务设置已保存');
  } catch (err) {
    const extra = err.data && err.data.error ? `：${err.data.error}` : `：${err.message}`;
    setResult(els.saveResult, 'err', '保存失败' + extra);
  } finally {
    els.saveBtn.disabled = false;
    els.saveBtn.textContent = '保存';
  }
}

// ---- 事件 ----

els.authBtn.addEventListener('click', async () => {
  state.token = els.authToken.value.trim();
  setResult(els.authError, '', '');
  if (!state.token) {
    setResult(els.authError, 'err', '请输入 token');
    return;
  }
  try {
    enter(await api('GET', '/api/setup'));
  } catch (err) {
    setResult(els.authError, 'err', err.status === 401 ? 'token 校验失败，请检查' : err.message);
  }
});
els.authToken.addEventListener('keydown', (ev) => {
  if (ev.key === 'Enter') els.authBtn.click();
});
els.saveBtn.addEventListener('click', save);
els.retention.addEventListener('keydown', (ev) => {
  if (ev.key === 'Enter') els.saveBtn.click();
});

// 重置用户密码（管理令牌接口）
els.resetPwdBtn.addEventListener('click', async () => {
  const user = els.resetUser.value.trim();
  const pass = els.resetPass.value;
  if (!user || pass.length < 6) {
    setResult(els.resetResult, 'err', '请填写用户名与至少 6 位的新密码');
    return;
  }
  if (!state.token) {
    setResult(els.resetResult, 'err', '需要先解锁（输入管理令牌）');
    return;
  }
  els.resetPwdBtn.disabled = true;
  try {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), 15000);
    const res = await fetch('/api/admin/reset-password', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json; charset=utf-8', Authorization: `Bearer ${state.token}` },
      body: JSON.stringify({ username: user, new_password: pass }),
      signal: ctrl.signal,
    });
    clearTimeout(timer);
    const data = await res.json().catch(() => null);
    if (res.ok && data && data.ok) {
      setResult(els.resetResult, 'ok', `已重置 ${user} 的密码，该用户所有设备需重新登录。`);
    } else {
      setResult(els.resetResult, 'err', (data && data.error) || `失败（HTTP ${res.status}）`);
    }
  } catch (err) {
    setResult(els.resetResult, 'err', '失败：' + err.message);
  } finally {
    els.resetPwdBtn.disabled = false;
  }
});

loadStatus();
