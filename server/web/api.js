/**
 * authLogin 登录/注册（独立函数：首次配置时还没有 api 实例可用）。
 * register=true 时先注册再登录。返回 { token, user }。
 */
export async function authLogin(server, username, password, device, register) {
  const base2 = String(server || '').replace(/\/+$/, '');
  const post = (path, body) => fetch(base2 + path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json; charset=utf-8' },
    body: JSON.stringify(body),
  }).then(async (res) => {
    const data = await res.json().catch(() => null);
    if (!res.ok) throw new ApiError((data && data.error) || `请求失败（HTTP ${res.status}）`, res.status);
    return data;
  });
  if (register) {
    await post('/api/auth/register', { username, password });
  }
  const data = await post('/api/auth/login', { username, password, device });
  return { token: data.token, user: data.user };
}

/**
 * api.js — REST API 封装（契约见 docs/PROTOCOL.md）
 *
 * - 所有请求自动携带 Authorization: Bearer <token>
 * - 统一超时（10s）、统一错误解析（{"error": "..."} → Error.message + status）
 */

export class ApiError extends Error {
  /**
   * @param {string} message 人类可读的错误信息
   * @param {number} status HTTP 状态码；0 表示网络层失败（无法连接 / 超时）
   */
  constructor(message, status) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

/**
 * @param {{server: string, token: string}} cfg
 */
export function createApi({ server, token }) {
  const base = String(server || '').replace(/\/+$/, '');

  /** 底层请求：返回 { status, data } */
  async function request(path, { method = 'GET', body, timeout = 10000 } = {}) {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), timeout);
    let res;
    try {
      res = await fetch(base + path, {
        method,
        signal: ctrl.signal,
        headers: {
          Authorization: `Bearer ${token}`,
          ...(body !== undefined ? { 'Content-Type': 'application/json; charset=utf-8' } : {}),
        },
        body: body !== undefined ? JSON.stringify(body) : undefined,
      });
    } catch (err) {
      if (err && err.name === 'AbortError') throw new ApiError('请求超时', 0);
      throw new ApiError('无法连接服务器，请检查地址与网络', 0, { cause: err });
    } finally {
      clearTimeout(timer);
    }

    if (res.status === 204) return { status: 204, data: null };

    let data = null;
    const text = await res.text();
    if (text) {
      try { data = JSON.parse(text); } catch { data = null; }
    }
    if (!res.ok) {
      throw new ApiError((data && data.error) || `请求失败（HTTP ${res.status}）`, res.status);
    }
    return { status: res.status, data };
  }

  return {
    /** GET /api/clips?limit= → { clips: [...] }（created_at 降序） */
    async listClips(limit = 100) {
      const { data } = await request(`/api/clips?limit=${limit}`);
      return data || { clips: [] };
    },

    /**
     * POST /api/clips → { clip, created }
     * created=true 表示新建（201）；false 表示与最新一条相同，服务端去重返回已有记录（200）
     */
    async uploadClip({ text, hash, device }) {
      const { status, data } = await request('/api/clips', {
        method: 'POST',
        body: { type: 'text', text, hash, device },
      });
      return { clip: data, created: status === 201 };
    },

    /** DELETE /api/clips/{id} → 成功无 body */
    async deleteClip(id) {
      await request(`/api/clips/${encodeURIComponent(id)}`, { method: 'DELETE' });
    },

    /**
     * POST /api/clips/image → { clip, created }
     * body: { data: base64（可带 data: 前缀）, device, width, height }
     */
    async uploadImage({ data, device, width, height }) {
      const { status, data: clip } = await request('/api/clips/image', {
        method: 'POST',
        timeout: 30000, // 图片大、移动网络慢，放宽超时
        body: { data, device, width, height },
      });
      return { clip, created: status === 201 };
    },

    /** GET /api/clips/{id}/blob → Blob（带认证；供 <img> 之外的程序化加载） */
    async getBlob(id) {
      const ctrl = new AbortController();
      const timer = setTimeout(() => ctrl.abort(), 30000);
      let res;
      try {
        res = await fetch(`${base}/api/clips/${encodeURIComponent(id)}/blob`, {
          signal: ctrl.signal,
          headers: { Authorization: `Bearer ${token}` },
        });
      } catch (err) {
        throw new ApiError(err && err.name === 'AbortError' ? '图片加载超时' : '图片加载失败', 0);
      } finally {
        clearTimeout(timer);
      }
      if (!res.ok) throw new ApiError(`图片加载失败（HTTP ${res.status}）`, res.status);
      return res.blob();
    },

    // ---- 部署向导 /api/setup（未配置时无需 token） ----

    /** GET /api/setup → { configured, status, token, data_dir, version } */
    async setupStatus() {
      return request('/api/setup', { timeout: 5000 });
    },

    /** POST /api/setup/test → { ok } */
    async setupTest(cfg) {
      return request('/api/setup/test', { method: 'POST', body: cfg, timeout: 15000 });
    },

    /** POST /api/setup → { ok, error?, listen, restart_required } */
    async setupSave(cfg) {
      return request('/api/setup', { method: 'POST', body: cfg, timeout: 30000 });
    },

    /** POST /api/setup/token → { token } */
    async setupRegenToken() {
      return request('/api/setup/token', { method: 'POST', body: {}, timeout: 5000 });
    },
  };
}
