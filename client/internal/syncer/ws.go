package syncer

import (
	"context"
	"math/rand/v2"
	"net/url"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	wsDialTimeout = 10 * time.Second // 建连超时
	wsReadTimeout = 90 * time.Second // 单次读超时：服务端每 30s ping，超时即视为连接已死
	wsReadLimit   = 16 << 20         // 单帧上限 16MB，覆盖大段文本
	wsMaxBackoff  = 30 * time.Second // 重连退避上限
	wsHealthyFor  = 1 * time.Minute  // 连接存活超过该时长视为健康，重置退避
)

// buildWSURL 把 REST 服务端地址转成 /ws 的 WebSocket 地址：
// http://h:p → ws://h:p/ws?token=…&device=…（https → wss）。
func buildWSURL(serverURL, token, device string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return serverURL // main 已做过格式校验，理论上不会走到
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	u.Path = "/ws"
	q := u.Query()
	q.Set("token", token)
	q.Set("device", device)
	u.RawQuery = q.Encode()
	return u.String()
}

// wsLoop 持续维持一条 WebSocket 连接：断开后指数退避重连（1s 起、翻倍、上限 30s，
// 附加少量抖动）；连接存活超过 wsHealthyFor 则重置退避。
func (e *Engine) wsLoop(ctx context.Context) {
	wsURL := buildWSURL(e.opts.ServerURL, e.opts.Token, e.opts.Device)
	backoff := time.Second
	for {
		start := time.Now()
		err := e.wsSession(ctx, wsURL)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) >= wsHealthyFor {
			backoff = time.Second
		} else {
			backoff = min(2*backoff, wsMaxBackoff)
		}
		sleep := backoff + time.Duration(rand.Int64N(int64(backoff)/4+1))
		e.log.Printf("已断开：%v；%.1fs 后重连", err, sleep.Seconds())
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
	}
}

// wsSession 建立一次连接并阻塞读取，直到出错或 ctx 取消。
func (e *Engine) wsSession(ctx context.Context, wsURL string) error {
	dialCtx, cancel := context.WithTimeout(ctx, wsDialTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close(websocket.StatusNormalClosure, "client shutdown")
	conn.SetReadLimit(wsReadLimit)
	// ctx 取消时主动关闭连接，解除阻塞中的 Read
	go func() {
		<-ctx.Done()
		conn.Close(websocket.StatusNormalClosure, "client shutdown")
	}()
	e.log.Printf("已连接 %s", wsURL)

	for {
		var msg wsMessage
		readCtx, readCancel := context.WithTimeout(ctx, wsReadTimeout)
		err := wsjson.Read(readCtx, conn, &msg)
		readCancel()
		if err != nil {
			return err
		}
		switch msg.Event {
		case "hello":
			e.log.Printf("连接就绪：服务端 %s，在线设备 %d 台", msg.Server, msg.DeviceCount)
		case "new":
			if msg.Clip != nil {
				e.handleRemote(ctx, msg.Clip)
			}
		case "deleted":
			e.log.Printf("远端删除了记录 #%d", msg.ID)
		case "":
			// 空事件忽略
		default:
			e.log.Printf("收到未知事件 %q，已忽略", msg.Event)
		}
	}
}
