/*
 * sw.js — LAN Paste Service Worker
 *
 * 策略：网络优先（network-first），失败才回退缓存。
 * 服务端升级后用户刷新一次即可拿到新页面，不会再出现"部署了没变化"的旧缓存问题；
 * 缓存仅作局域网短暂离线时的兜底。/api、/ws、/healthz 与非 GET 请求一律直连不缓存。
 */

const CACHE_NAME = 'lanpaste-static-v8';
const PRECACHE_ASSETS = [
  './',
  './index.html',
  './setup.html',
  './settings.html',
  './app.js',
  './setup.js',
  './settings.js',
  './api.js',
  './ws.js',
  './ui.js',
  './utils.js',
  './style.css',
  './icon.svg',
  './manifest.webmanifest',
];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME)
      .then((cache) => cache.addAll(PRECACHE_ASSETS))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE_NAME).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return; // POST/DELETE 等实时请求直连

  let url;
  try { url = new URL(req.url); } catch { return; }
  if (url.origin !== self.location.origin) return;          // 跨域（如另行配置的服务器地址）放行
  if (url.pathname === '/ws' || url.pathname.startsWith('/api/') || url.pathname === '/healthz') {
    return; // API 与 WebSocket 握手不缓存
  }

  // 网络优先：拿得到最新版就返回最新版，只有断网/服务器不可达时才用缓存兜底
  event.respondWith(
    fetch(req).then((res) => {
      if (res && res.ok) {
        const copy = res.clone();
        caches.open(CACHE_NAME).then((cache) => cache.put(req, copy));
      }
      return res;
    }).catch(() =>
      caches.match(req, { ignoreSearch: true }).then((hit) => {
        if (hit) return hit;
        // 离线兜底：导航请求回退到已缓存的首页
        if (req.mode === 'navigate') return caches.match('./index.html');
        return undefined;
      })
    )
  );
});
