package syncer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"lanpaste/client/internal/clipboard"
)

// fakeImageClipboard 模拟真实剪贴板的图片重编码行为：
// 「写入即定形」——写入的字节经一次重编码后存定（macOS PNGf / Windows DIB 回退），
// 之后每次读回稳定返回同一份（但与写入时字节不同、像素相同）。
type fakeImageClipboard struct {
	mu     sync.Mutex
	png    []byte
	hasImg bool
	writes int // WriteImage 调用次数（引擎写入）
}

func (f *fakeImageClipboard) ReadText() (string, error) { return "", nil }

func (f *fakeImageClipboard) WriteText(s string) error { return nil }

// reencodePNG 重编码 PNG：像素不变、字节改变（模拟剪贴板物化）。
func reencodePNG(t *testing.T, pngBytes []byte) []byte {
	m, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (f *fakeImageClipboard) ReadImage() (*clipboard.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.hasImg {
		return nil, nil
	}
	img := &clipboard.Image{Data: f.png}
	if cfg, err := png.DecodeConfig(bytes.NewReader(f.png)); err == nil {
		img.Width, img.Height = cfg.Width, cfg.Height
	}
	return img, nil
}

func (f *fakeImageClipboard) WriteImage(img *clipboard.Image) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// 引擎写入的 PNG 进入剪贴板时被物化重编码（像素不变、字节变）
	f.png = reencodePNG(&testing.T{}, img.Data)
	f.hasImg = true
	f.writes++
	return nil
}

// SetImage 模拟用户复制（原始字节，不计入 writes）。
func (f *fakeImageClipboard) SetImage(pngBytes []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.png = pngBytes
	f.hasImg = true
}

func (f *fakeImageClipboard) WriteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

// testPNG 生成 w×h 的确定性 PNG（像素由 seed 决定，同尺寸不同 seed 像素不同）。
func genPNG(t *testing.T, w, h int, seed uint8) []byte {
	t.Helper()
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.SetNRGBA(x, y, color.NRGBA{R: seed + uint8(x), G: seed + uint8(y), B: seed ^ uint8(x*y), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// imageFakeServer 在文本假服务端之外补充图片契约：
// POST /api/clips/image（hash=图片字节 sha256，与最新一条相同则 200 去重不广播）、
// GET /api/clips/{id}/blob。
type imageFakeServer struct {
	*httptest.Server
	token string

	mu       sync.Mutex
	nextID   int64
	latest   *Clip
	blobs    map[int64][]byte
	imgPosts int // 图片 POST 总数
	conns    map[*websocket.Conn]string
}

func newImageFakeServer(t *testing.T, token string) *imageFakeServer {
	t.Helper()
	s := &imageFakeServer{token: token, blobs: map[int64][]byte{}, conns: map[*websocket.Conn]string{}}
	s.Server = httptest.NewServer(s)
	t.Cleanup(s.Server.Close)
	return s
}

func (s *imageFakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/clips/image" && r.Method == http.MethodPost:
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		var req struct {
			Data   string `json:"data"`
			Device string `json:"device"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad json")
			return
		}
		data, err := base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad base64")
			return
		}
		hash := HashBytes(data)

		s.mu.Lock()
		s.imgPosts++
		if s.latest != nil && s.latest.Hash == hash {
			existing := *s.latest
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, &existing)
			return
		}
		s.nextID++
		id := s.nextID
		clip := &Clip{
			ID: id, Type: "image", Hash: hash, SourceDevice: req.Device,
			Size: int64(len(data)), BlobURL: "/api/clips/1/blob",
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		clip.BlobURL = blobPath(id)
		s.blobs[id] = data
		s.latest = clip
		conns := make(map[*websocket.Conn]string, len(s.conns))
		for c, d := range s.conns {
			conns[c] = d
		}
		s.mu.Unlock()

		for conn, dev := range conns {
			if dev == req.Device {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = wsjson.Write(ctx, conn, wsMessage{Event: "new", Clip: clip})
			cancel()
		}
		writeJSON(w, http.StatusCreated, clip)

	case len(r.URL.Path) > len("/api/clips/") && r.Method == http.MethodGet &&
		strings.HasPrefix(r.URL.Path, "/api/clips/") && strings.HasSuffix(r.URL.Path, "/blob"):
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		seg := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/clips/"), "/blob")
		id, err := strconv.ParseInt(seg, 10, 64)
		if err != nil {
			writeErr(w, http.StatusNotFound, "bad id")
			return
		}
		s.mu.Lock()
		data, ok := s.blobs[id]
		s.mu.Unlock()
		if !ok {
			writeErr(w, http.StatusNotFound, "no blob")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)

	case r.URL.Path == "/ws":
		if r.URL.Query().Get("token") != s.token {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		device := r.URL.Query().Get("device")
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = wsjson.Write(ctx, conn, wsMessage{Event: "hello", Server: "lanpaste/fake", DeviceCount: 1})
		cancel()
		s.mu.Lock()
		s.conns[conn] = device
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
			conn.CloseNow()
		}()
		for {
			if _, _, err := conn.Read(context.Background()); err != nil {
				return
			}
		}

	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func blobPath(id int64) string {
	return "/api/clips/" + strconv.FormatInt(id, 10) + "/blob"
}

// imgStats 返回图片 POST 总数。
func (s *imageFakeServer) imgStats() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.imgPosts
}

// TestImagePingPongGuard 复现并回归「图片回环风暴」：
// A 上传图片 → 服务端广播 → B 写入系统剪贴板 → 剪贴板读回被重编码（字节变、像素不变）
// → B 不得把它当新内容再上传（否则两台设备无限乒乓，直到页面卡死）。
// 同时验证真正不同的新图（同尺寸不同像素）不受误伤。
func TestImagePingPongGuard(t *testing.T) {
	fs := newImageFakeServer(t, "tok")

	clipA := &fakeImageClipboard{}
	clipB := &fakeImageClipboard{}
	eA := New(Options{ServerURL: fs.URL, Token: "tok", Device: "device-a", PollInterval: 10 * time.Millisecond, Logger: quietLogger()}, clipA)
	eB := New(Options{ServerURL: fs.URL, Token: "tok", Device: "device-b", PollInterval: 10 * time.Millisecond, Logger: quietLogger()}, clipB)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eA.Run(ctx)
	go eB.Run(ctx)

	waitFor(t, "两个客户端均连上 /ws", 5*time.Second, func() bool { return fs.connectedCount() >= 2 })

	// A 上传原图 → B 应写入一次
	clipA.SetImage(genPNG(t, 64, 48, 7))
	waitFor(t, "B 收到图片并写入", 5*time.Second, func() bool { return clipB.WriteCount() == 1 })

	// B 读回的是重编码变体（字节不同、像素相同）——不得再上传（核心回归点）
	time.Sleep(400 * time.Millisecond)
	if n := fs.imgStats(); n != 1 {
		t.Fatalf("回环未阻断：期望图片上传 1 次，实际 %d 次", n)
	}

	// 反向乒乓：B 把变体推给 A？不会——B 没有上传。A 读回自己的变体也不得上传。
	time.Sleep(200 * time.Millisecond)
	if n := fs.imgStats(); n != 1 {
		t.Fatalf("稳定期出现额外上传：%d 次", n)
	}

	// 真正的新图（同尺寸、不同像素）必须正常上传，不被像素守卫误伤
	clipB.SetImage(genPNG(t, 64, 48, 99))
	waitFor(t, "B 的新图上传（新记录）", 5*time.Second, func() bool { return fs.imgStats() >= 2 })
	waitFor(t, "A 收到 B 的新图并写入", 5*time.Second, func() bool { return clipA.WriteCount() == 1 })

	// A 读回重编码变体 → 同样不得回传
	time.Sleep(400 * time.Millisecond)
	if n := fs.imgStats(); n != 2 {
		t.Fatalf("A 侧回环未阻断：期望图片上传共 2 次，实际 %d 次", n)
	}
}

// connectedCount 返回当前 WebSocket 连接数。
func (s *imageFakeServer) connectedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}
