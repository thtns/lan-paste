/**
 * ws.js — WebSocket 封装
 *
 * - 连接 GET /ws?token=<token>&device=<device>（浏览器无法自定义请求头，token 走查询参数）
 * - 断线后指数退避自动重连（1s → 2s → 4s ... 上限 30s，附加随机抖动）
 * - 状态回调 onStatus(status, info)：
 *     'connecting'  { attempt }          正在建立连接
 *     'online'      { attempt: 0 }       连接已建立
 *     'offline'     { attempt }          连接断开（即将安排重连）
 *     'scheduled'   { attempt, delayMs } 已安排下一次重连
 * - 事件回调 onEvent(msg)：服务端 JSON 消息原样透传（hello / new / deleted）
 */

export class SyncSocket {
  /**
   * @param {{server: string, token: string, device: string,
   *          onEvent?: (msg: object) => void,
   *          onStatus?: (status: string, info: object) => void}} opts
   */
  constructor({ server, token, device, onEvent, onStatus }) {
    this.server = server;
    this.token = token;
    this.device = device;
    this.onEvent = onEvent;
    this.onStatus = onStatus;
    this.attempt = 0;        // 已失败的重连次数
    this.closed = false;     // 主动关闭后不再重连
    this.timer = null;
    this.ws = null;
  }

  connect() {
    if (this.closed) return;
    this.onStatus && this.onStatus('connecting', { attempt: this.attempt });
    let ws;
    try {
      ws = new WebSocket(this.buildUrl());
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.ws = ws;
    ws.onopen = () => {
      this.attempt = 0;
      this.onStatus && this.onStatus('online', { attempt: 0 });
    };
    ws.onmessage = (ev) => {
      let msg = null;
      try { msg = JSON.parse(ev.data); } catch { return; }
      if (msg && typeof msg === 'object') this.onEvent && this.onEvent(msg);
    };
    ws.onclose = () => {
      if (this.ws !== ws) return; // 已被新连接取代
      this.ws = null;
      this.onStatus && this.onStatus('offline', { attempt: this.attempt });
      this.scheduleReconnect();
    };
    ws.onerror = () => { /* 出错后浏览器总会触发 onclose，统一在那里处理 */ };
  }

  scheduleReconnect() {
    if (this.closed) return;
    const delay = Math.min(30000, 1000 * 2 ** this.attempt) + Math.floor(Math.random() * 400);
    this.attempt += 1;
    this.onStatus && this.onStatus('scheduled', { attempt: this.attempt, delayMs: delay });
    this.timer = setTimeout(() => this.connect(), delay);
  }

  /** 主动关闭（配置变更时使用），不触发重连 */
  close() {
    this.closed = true;
    if (this.timer) clearTimeout(this.timer);
    if (this.ws) {
      this.ws.onclose = null;
      try { this.ws.close(); } catch { /* ignore */ }
      this.ws = null;
    }
  }

  /** 由服务器地址推导 ws(s)://host/ws?token=&device= */
  buildUrl() {
    const u = new URL(this.server);
    const proto = u.protocol === 'https:' ? 'wss:' : 'ws:';
    return `${proto}//${u.host}/ws?token=${encodeURIComponent(this.token)}&device=${encodeURIComponent(this.device)}`;
  }
}
