#!/usr/bin/env node
/**
 * LAN Paste — Web 前端自测用 mock 服务器（零第三方依赖，Node >= 20）
 *
 *   node mock-server.mjs
 *   PORT=9000 PASTE_TOKEN=xxx node mock-server.mjs
 *
 * 按 docs/PROTOCOL.md 仿真：
 *   - 静态托管本目录（Cache-Control: no-store，方便调试）
 *   - REST：POST/GET /api/clips、GET /api/clips/latest、GET/DELETE /api/clips/{id}、GET /healthz
 *   - WebSocket /ws?token=&device=：连接后推 hello；POST 广播 new（排除同设备的连接）；
 *     DELETE 广播 deleted；每 30s 发 ping（浏览器自动回 pong）
 *
 * 数据仅存内存，重启即重置（启动时预置 3 条剪贴板内容）。
 * 说明：本文件位于 server/web/ 内，会被 go:embed 一并内嵌，但页面不会引用它，无副作用。
 */

import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';

const ROOT = path.dirname(fileURLToPath(import.meta.url));
const PORT = Number(process.env.PORT || 8899);
let TOKEN = process.env.PASTE_TOKEN || 'devtoken';
const WS_GUID = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11';
const MAX_BODY = 24 * 1024 * 1024; // 24MB（图片 base64）

/** @type {Array<object>} 按 created_at 升序存放；图片条目带 _blob Buffer（不序列化） */
const clips = [];
let nextId = 1;
/** @type {Set<{socket: import('net').Socket, device: string}>} */
const wsClients = new Set();

const sha256 = (s) => crypto.createHash('sha256').update(s, 'utf8').digest('hex');
const sha256Buf = (b) => crypto.createHash('sha256').update(b).digest('hex');

function makeClip(text, device, isoTime) {
  const clip = {
    id: nextId++,
    type: 'text',
    text,
    hash: sha256(text),
    source_device: device,
    size: Buffer.byteLength(text, 'utf8'),
    created_at: isoTime || new Date().toISOString(),
  };
  clips.push(clip);
  return clip;
}

function makeImageClip(buf, device, { width = 0, height = 0, mime = 'image/png' } = {}, isoTime) {
  const clip = {
    id: nextId++,
    type: 'image',
    text: '',
    hash: sha256Buf(buf),
    source_device: device,
    size: buf.length,
    mime,
    width,
    height,
    blob_url: '',
    created_at: isoTime || new Date().toISOString(),
    _blob: buf,
  };
  clips.push(clip);
  return clip;
}

/** 序列化时剥掉内部 _blob 字段 */
function publicClip(c) {
  if (c.type !== 'image') return c;
  const { _blob, ...rest } = c;
  return { ...rest, blob_url: `/api/clips/${c.id}/blob` };
}

// ---- 预置数据 ----
makeClip('npm run dev -- --host', 'macbook-pro', new Date(Date.now() - 3 * 60000).toISOString());
makeClip('WiFi:Home-5G\n密码:paste@2024', 'iphone-15', new Date(Date.now() - 40 * 60000).toISOString());
makeClip('sudo apt install -y build-essential', 'linux-box', new Date(Date.now() - 65 * 60000).toISOString());
makeClip('评审会议改到周五 15:00\n- 提前更新文档\n- 预约会议室\n- 同步给缺席的同学', 'windows-dell',
  new Date(Date.now() - 26 * 3600000).toISOString());
// 预置一张 4x4 蓝色 PNG，演示图片卡片与灯箱
const PRESET_PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAQAAAAECAYAAACp8Z5+AAAAFklEQVR4nGP8z8Dwn4EIwESMolGFAQBeBgYIf4FK/AAAAABJRU5ErkJggg==',
  'base64');
makeImageClip(PRESET_PNG, 'iphone-15', { width: 4, height: 4 },
  new Date(Date.now() - 27 * 3600000).toISOString());

// ---------------------------------------------------------------------------
// WebSocket 帧编解码（服务端 → 客户端不掩码；客户端 → 服务端必须掩码）
// ---------------------------------------------------------------------------

function encodeFrame(opcode, payload = Buffer.alloc(0)) {
  const len = payload.length;
  let header;
  if (len < 126) {
    header = Buffer.from([0x80 | opcode, len]);
  } else if (len < 65536) {
    header = Buffer.alloc(4);
    header[0] = 0x80 | opcode;
    header[1] = 126;
    header.writeUInt16BE(len, 2);
  } else {
    header = Buffer.alloc(10);
    header[0] = 0x80 | opcode;
    header[1] = 127;
    header.writeBigUInt64BE(BigInt(len), 2);
  }
  return Buffer.concat([header, payload]);
}

function sendJsonTo(client, obj) {
  try {
    client.socket.write(encodeFrame(0x1, Buffer.from(JSON.stringify(obj), 'utf8')));
  } catch { /* 连接已断，忽略 */ }
}

/** 广播；exceptDevice 存在时排除该设备的连接（协议：只广播给「其他」客户端） */
function broadcast(obj, exceptDevice = null) {
  const msg = JSON.stringify(obj);
  for (const c of wsClients) {
    if (exceptDevice && c.device === exceptDevice) continue;
    try { c.socket.write(encodeFrame(0x1, Buffer.from(msg, 'utf8'))); } catch { /* ignore */ }
  }
}

/** 增量解析客户端帧（本 mock 只需处理 close/ping/pong，文本帧按协议客户端不会发） */
function makeFrameParser(onFrame) {
  let buf = Buffer.alloc(0);
  return (chunk) => {
    buf = Buffer.concat([buf, chunk]);
    for (;;) {
      if (buf.length < 2) return;
      const opcode = buf[0] & 0x0f;
      const masked = (buf[1] & 0x80) !== 0;
      let len = buf[1] & 0x7f;
      let off = 2;
      if (len === 126) {
        if (buf.length < 4) return;
        len = buf.readUInt16BE(2);
        off = 4;
      } else if (len === 127) {
        if (buf.length < 10) return;
        len = Number(buf.readBigUInt64BE(2));
        off = 10;
      }
      let mask = null;
      if (masked) {
        if (buf.length < off + 4) return;
        mask = buf.subarray(off, off + 4);
        off += 4;
      }
      if (buf.length < off + len) return;
      let payload = buf.subarray(off, off + len);
      if (mask) {
        const out = Buffer.allocUnsafe(len);
        for (let i = 0; i < len; i++) out[i] = payload[i] ^ mask[i % 4];
        payload = out;
      }
      buf = buf.subarray(off + len);
      onFrame(opcode, payload);
    }
  };
}

// ---------------------------------------------------------------------------
// HTTP：REST + 静态托管
// ---------------------------------------------------------------------------

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.webmanifest': 'application/manifest+json',
  '.json': 'application/json; charset=utf-8',
  '.md': 'text/markdown; charset=utf-8',
  '.txt': 'text/plain; charset=utf-8',
};

function sendJson(res, status, obj) {
  const body = JSON.stringify(obj);
  res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8' });
  res.end(body);
}

function readJsonBody(req, limit = MAX_BODY) {
  return new Promise((resolve, reject) => {
    let size = 0;
    const chunks = [];
    req.on('data', (c) => {
      size += c.length;
      if (size > limit) {
        reject(Object.assign(new Error('payload too large'), { status: 413 }));
        req.destroy();
        return;
      }
      chunks.push(c);
    });
    req.on('end', () => {
      try {
        resolve(JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}'));
      } catch {
        reject(Object.assign(new Error('invalid json body'), { status: 400 }));
      }
    });
    req.on('error', reject);
  });
}

async function handleApi(req, res, url) {
  // POST /api/setup*（部署向导仿真：始终成功）
  if (url.pathname === '/api/setup' && req.method === 'GET') {
    return sendJson(res, 200, {
      configured: false,
      status: { configured: false, listen: ':8899', store: 'memory', db: { store: 'memory' } },
      token: TOKEN,
      data_dir: '/tmp/lanpaste-mock',
      version: 'mock',
    });
  }
  if (url.pathname === '/api/setup/test' && req.method === 'POST') {
    await readJsonBody(req);
    return sendJson(res, 200, { ok: true });
  }
  if (url.pathname === '/api/setup' && req.method === 'POST') {
    const body = await readJsonBody(req);
    return sendJson(res, 200, { ok: true, listen: body.listen, restart_required: false });
  }
  if (url.pathname === '/api/setup/token' && req.method === 'POST') {
    TOKEN = crypto.randomBytes(32).toString('hex');
    console.log(`[mock] token 已更换: ${TOKEN}`);
    return sendJson(res, 200, { token: TOKEN });
  }

  // POST /api/clips（文本）
  if (req.method === 'POST' && url.pathname === '/api/clips') {
    const body = await readJsonBody(req);
    if (typeof body.text !== 'string' || body.text.length === 0) {
      return sendJson(res, 400, { error: 'text 字段必填' });
    }
    const hash = sha256(body.text);
    const latest = clips[clips.length - 1];
    if (latest && latest.hash === hash) {
      return sendJson(res, 200, latest); // 与最新一条相同：不新建、不广播
    }
    const clip = makeClip(body.text, body.device || 'unknown');
    broadcast({ event: 'new', clip }, body.device || 'unknown');
    return sendJson(res, 201, clip);
  }

  // POST /api/clips/image（图片，base64）
  if (req.method === 'POST' && url.pathname === '/api/clips/image') {
    const body = await readJsonBody(req);
    let buf;
    try {
      buf = Buffer.from(String(body.data || '').replace(/^data:[^,]*,/, ''), 'base64');
    } catch {
      return sendJson(res, 400, { error: 'invalid base64' });
    }
    if (!buf.length) return sendJson(res, 400, { error: 'empty image data' });
    const mime = body.mime || 'image/png';
    const hash = sha256Buf(buf);
    const latest = clips[clips.length - 1];
    if (latest && latest.hash === hash) {
      return sendJson(res, 200, publicClip(latest));
    }
    const clip = makeImageClip(buf, body.device || 'unknown', {
      width: Number(body.width) || 0,
      height: Number(body.height) || 0,
      mime,
    });
    broadcast({ event: 'new', clip: publicClip(clip) }, body.device || 'unknown');
    return sendJson(res, 201, publicClip(clip));
  }

  // GET /api/clips?limit=
  if (req.method === 'GET' && url.pathname === '/api/clips') {
    let limit = Number(url.searchParams.get('limit') || 50);
    if (!Number.isFinite(limit) || limit < 1) limit = 50;
    limit = Math.min(200, limit);
    return sendJson(res, 200, { clips: [...clips].reverse().slice(0, limit).map(publicClip) });
  }

  // GET /api/clips/latest
  if (req.method === 'GET' && url.pathname === '/api/clips/latest') {
    const latest = clips[clips.length - 1];
    if (!latest) { res.writeHead(204); return res.end(); }
    return sendJson(res, 200, publicClip(latest));
  }

  // GET /api/clips/{id}/blob
  const bm = url.pathname.match(/^\/api\/clips\/(\d+)\/blob$/);
  if (bm && req.method === 'GET') {
    const clip = clips.find((c) => c.id === Number(bm[1]));
    if (!clip || clip.type !== 'image') return sendJson(res, 404, { error: 'not found' });
    res.writeHead(200, {
      'Content-Type': clip.mime || 'application/octet-stream',
      'Content-Length': clip._blob.length,
      'Cache-Control': 'private, max-age=3600',
    });
    return res.end(req.method === 'HEAD' ? undefined : clip._blob);
  }

  const m = url.pathname.match(/^\/api\/clips\/(\d+)$/);
  if (m) {
    const clip = clips.find((c) => c.id === Number(m[1]));
    if (req.method === 'GET') {
      return clip ? sendJson(res, 200, publicClip(clip)) : sendJson(res, 404, { error: 'clip not found' });
    }
    if (req.method === 'DELETE') {
      if (!clip) return sendJson(res, 404, { error: 'clip not found' });
      clips.splice(clips.indexOf(clip), 1);
      broadcast({ event: 'deleted', id: clip.id }); // 删除事件广播给所有客户端
      res.writeHead(204);
      return res.end();
    }
  }

  return sendJson(res, 404, { error: 'not found' });
}

function serveStatic(url, req, res) {
  let p;
  try {
    p = decodeURIComponent(url.pathname);
  } catch {
    p = '/';
  }
  if (p === '/') p = '/index.html';
  const file = path.normalize(path.join(ROOT, p));
  if (!file.startsWith(ROOT + path.sep) && file !== ROOT) {
    res.writeHead(403);
    return res.end('forbidden');
  }
  fs.readFile(file, (err, data) => {
    if (err) {
      res.writeHead(404, { 'Content-Type': 'text/plain; charset=utf-8' });
      return res.end('404 not found');
    }
    const type = MIME[path.extname(file).toLowerCase()] || 'application/octet-stream';
    res.writeHead(200, { 'Content-Type': type, 'Cache-Control': 'no-store' });
    res.end(req.method === 'HEAD' ? undefined : data);
  });
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, `http://${req.headers.host || 'localhost'}`);

  if (url.pathname === '/healthz') {
    res.writeHead(200, { 'Content-Type': 'text/plain' });
    return res.end('ok');
  }

  if (url.pathname === '/api' || url.pathname.startsWith('/api/')) {
    if (req.headers.authorization !== `Bearer ${TOKEN}`) {
      return sendJson(res, 401, { error: 'unauthorized' });
    }
    try {
      await handleApi(req, res, url);
    } catch (err) {
      const status = err && err.status ? err.status : 500;
      sendJson(res, status, { error: String((err && err.message) || err) });
    }
    return;
  }

  return serveStatic(url, req, res);
});

// ---------------------------------------------------------------------------
// WebSocket 升级握手
// ---------------------------------------------------------------------------

server.on('upgrade', (req, socket) => {
  let url;
  try {
    url = new URL(req.url, `http://${req.headers.host || 'localhost'}`);
  } catch {
    socket.destroy();
    return;
  }
  if (url.pathname !== '/ws') {
    socket.write('HTTP/1.1 404 Not Found\r\nConnection: close\r\n\r\n');
    socket.destroy();
    return;
  }
  if (url.searchParams.get('token') !== TOKEN) {
    socket.write('HTTP/1.1 401 Unauthorized\r\nConnection: close\r\n\r\n');
    socket.destroy();
    return;
  }
  const key = req.headers['sec-websocket-key'];
  if (!key || String(req.headers.upgrade || '').toLowerCase() !== 'websocket') {
    socket.destroy();
    return;
  }
  const accept = crypto.createHash('sha1').update(key + WS_GUID).digest('base64');
  socket.write(
    'HTTP/1.1 101 Switching Protocols\r\n' +
    'Upgrade: websocket\r\n' +
    'Connection: Upgrade\r\n' +
    `Sec-WebSocket-Accept: ${accept}\r\n\r\n`
  );
  socket.setNoDelay(true);

  const device = url.searchParams.get('device') || 'unknown';
  const client = { socket, device };
  wsClients.add(client);
  sendJsonTo(client, { event: 'hello', server: 'lanpaste-mock/v1', device_count: wsClients.size });

  const parse = makeFrameParser((opcode, payload) => {
    if (opcode === 0x8) { // close
      try { socket.write(encodeFrame(0x8, payload.subarray(0, 2))); } catch { /* ignore */ }
      wsClients.delete(client);
      socket.end();
    } else if (opcode === 0x9) { // ping → pong
      try { socket.write(encodeFrame(0xA, payload)); } catch { /* ignore */ }
    }
    // 0x1 文本帧 / 0xA pong：MVP 客户端 → 服务端无消息，忽略
  });
  socket.on('data', parse);
  const drop = () => wsClients.delete(client);
  socket.on('close', drop);
  socket.on('error', drop);
});

// 服务端心跳：每 30s ping 一次（浏览器自动回 pong）
setInterval(() => {
  for (const c of wsClients) {
    try { c.socket.write(encodeFrame(0x9, Buffer.alloc(0))); } catch { /* ignore */ }
  }
}, 30000).unref();

server.listen(PORT, '0.0.0.0', () => {
  console.log('LAN Paste mock 服务器已启动（数据在内存中，重启即重置）');
  console.log(`  页面:  http://localhost:${PORT}/`);
  console.log(`  向导:  http://localhost:${PORT}/setup.html`);
  console.log(`  Token: ${TOKEN}`);
  console.log(`  已预置 ${clips.length} 条剪贴板内容（含 1 张图片）`);
});
