// Package ws 实现 /ws?token=<token> WebSocket 端点与连接管理（Hub）。
//
// Hub 实现 api.Broadcaster 接口（Go 结构化匹配），ws 包因此无需 import api，
// 由 main 负责把 Hub 注入 api，避免循环依赖。
package ws

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"lanpaste/server/internal/auth"
	"lanpaste/server/internal/store"
)

const (
	serverName   = "lanpaste/v0.1"
	pingInterval = 30 * time.Second // 契约：服务端每 30s 发 Ping
	writeTimeout = 10 * time.Second
	sendBuffer   = 32 // 每连接发送缓冲条数；溢出视为慢客户端直接断开
)

// Hub 管理全部 WebSocket 连接；注册 / 注销 / 广播均并发安全。
// 多用户（v1.2）：每条连接在握手时经设备令牌解析所属用户，广播按用户隔离。
type Hub struct {
	token string
	auth  *auth.Store // 账号体系；nil 时仅管理令牌可用
	mu    sync.Mutex
	conns map[int64]*client
	next  int64 // 连接 ID 分配器
}

// client 一条 WebSocket 连接的注册信息。
type client struct {
	id     int64
	device string // 连接时通过 &device= 查询参数声明
	userID int64  // 握手时解析：0 = 管理令牌
	cancel context.CancelFunc
	send   chan []byte // 所有下行消息（含 hello / new / deleted）经此串行写出
}

// NewHub 创建 Hub。token 用于 /ws?token= 查询参数鉴权；
// auth 供设备令牌解析所属用户（可 nil，此时仅管理令牌可用）。
func NewHub(token string, authStore *auth.Store) *Hub {
	return &Hub{token: token, auth: authStore, conns: make(map[int64]*client)}
}

// SetToken 运行期更换共享 token（setup 引导页重新生成 token 后调用）。
// 已建立的连接不受影响，新连接按新 token 校验。
func (h *Hub) SetToken(tok string) {
	h.mu.Lock()
	h.token = tok
	h.mu.Unlock()
}

// ---- 事件消息结构（与契约一致） ----

type helloEvent struct {
	Event       string `json:"event"`
	Server      string `json:"server"`
	DeviceCount int    `json:"device_count"`
}

type newEvent struct {
	Event string      `json:"event"`
	Clip  *store.Clip `json:"clip"`
}

type deletedEvent struct {
	Event string `json:"event"`
	ID    int64  `json:"id"`
}

// ServeHTTP 处理 GET /ws?token=<token>&device=<name>。
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	authStore := h.auth
	h.mu.Unlock()

	presented := r.URL.Query().Get("token")
	if authStore == nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}
	userID, ok := authStore.VerifyToken(presented)
	if !ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}

	// 局域网内浏览器 / PWA / file:// 页面来源不固定，跳过 Origin 校验，鉴权完全依赖 token
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return // Accept 已写过错误响应
	}

	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cl := &client{device: r.URL.Query().Get("device"), userID: userID, cancel: cancel, send: make(chan []byte, sendBuffer)}

	// 注册并在持锁状态下发送 hello，保证 hello 一定是该连接收到的第一条消息
	h.register(cl)

	go h.writeLoop(cl, c, baseCtx)

	// 读循环：MVP 客户端→服务端无消息，仅为处理 Ping/Pong 与关闭帧（浏览器自动回 Pong）
	for {
		if _, _, err := c.Read(baseCtx); err != nil {
			break
		}
	}
	h.unregister(cl)
	cancel()
	_ = c.Close(websocket.StatusNormalClosure, "bye")
}

// register 加锁注册连接，并在锁内把 hello 事件放入发送队列。
func (h *Hub) register(cl *client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.next++
	cl.id = h.next
	h.conns[cl.id] = cl
	hello, err := json.Marshal(helloEvent{Event: "hello", Server: serverName, DeviceCount: len(h.conns)})
	if err != nil {
		log.Printf("ws: 序列化 hello 失败: %v", err)
		return
	}
	cl.send <- hello // 缓冲区刚创建，必然放得下
}

// unregister 注销连接（幂等）。
func (h *Hub) unregister(cl *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, cl.id)
}

// writeLoop 单 goroutine 负责该连接的全部写出（coder/websocket 要求同时只有一个写者），
// 并按 30s 周期发 Ping。
func (h *Hub) writeLoop(cl *client, c *websocket.Conn, ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case msg := <-cl.send:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				cl.cancel() // 写失败：唤醒读循环退出，走统一注销流程
				return
			}
		case <-ticker.C:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.Ping(wctx)
			cancel()
			if err != nil {
				cl.cancel()
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// BroadcastNew 实现 api.Broadcaster：向除 excludeDevice 外的所有连接广播 new 事件。
// excludeDevice 为空时广播给所有人。
func (h *Hub) BroadcastNew(clip store.Clip, excludeDevice string) {
	msg, err := json.Marshal(newEvent{Event: "new", Clip: &clip})
	if err != nil {
		log.Printf("ws: 序列化 new 事件失败: %v", err)
		return
	}
	h.broadcast(msg, excludeDevice, clip.UserID)
}

// BroadcastDeleted 实现 api.Broadcaster：向该用户的所有连接广播 deleted 事件。
func (h *Hub) BroadcastDeleted(id int64, userID int64) {
	msg, err := json.Marshal(deletedEvent{Event: "deleted", ID: id})
	if err != nil {
		log.Printf("ws: 序列化 deleted 事件失败: %v", err)
		return
	}
	h.broadcast(msg, "", userID)
}

func (h *Hub) broadcast(msg []byte, excludeDevice string, userID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, cl := range h.conns {
		if excludeDevice != "" && cl.device == excludeDevice {
			continue // 来源设备不回发，避免回环
		}
		if userID != cl.userID {
			continue // 多用户隔离：只发给自己账号的连接
		}
		select {
		case cl.send <- msg:
		default:
			// 慢客户端：缓冲溢出，直接断开以免拖垮广播
			log.Printf("ws: 设备 %q（连接 %d）发送缓冲溢出，断开", cl.device, cl.id)
			delete(h.conns, cl.id)
			cl.cancel()
		}
	}
}

// secureEqual 常数时间字符串比较（与 api 包同款，两包各自持有以免互相依赖）。
func secureEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
