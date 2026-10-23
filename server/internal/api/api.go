// Package api 实现 REST 端点（/api/clips 系列）与 Bearer Token 认证中间件。
// 路由使用标准库 net/http 的 Go 1.22 方法/路径模式，不引入第三方 Web 框架。
// v1.1：新增图片上传（POST /api/clips/image）与图片下载（GET /api/clips/{id}/blob），
// 并支持存储后端 / token 的运行期热切换（供 setup 引导流程使用）。
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"lanpaste/server/internal/auth"
	"lanpaste/server/internal/blob"
	"lanpaste/server/internal/store"
)

// Broadcaster 数据变更时的广播回调，由 ws 包的 Hub 实现（结构化匹配）。
// 独立成接口是为了解耦 api 与 ws，避免 import 环：api 只依赖此接口，不知道 ws 的存在。
type Broadcaster interface {
	// BroadcastNew 广播新增事件给除 excludeDevice 之外的所有连接。
	BroadcastNew(clip store.Clip, excludeDevice string)
	// BroadcastDeleted 向该用户的所有连接广播删除事件。
	BroadcastDeleted(id int64, userID int64)
}

const (
	defaultLimit  = 50
	maxLimit      = 200
	maxBodyBytes  = 10 << 20 // 文本上限 10 MiB
	maxImageBody  = 24 << 20 // 图片请求体上限（base64 膨胀 4/3，对应解码后 16 MiB）
	maxImageBytes = 16 << 20 // 图片解码后字节上限
)

// 允许的图片类型 → 文件扩展名（http.DetectContentType 的返回值）。
var imageExts = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

// Server REST API 服务。
type Server struct {
	blobs *blob.Manager // 图片落盘管理器；nil 时图片端点返回 503（如单测场景）
	bc    Broadcaster   // 可为 nil（无 ws 时的测试 / 降级场景）
	auth  *auth.Store   // 账号体系；nil 时仅管理令牌可用（单测场景）

	mu    sync.Mutex   // 串行化「查最新 + 插入」，保证 hash 去重无并发窗口
	cfgMu sync.RWMutex // 保护 store / token 的运行期热切换（setup 引导）
	store store.Store
	token string
}

// New 创建 API 服务。b 与 blobs 允许为 nil。
func New(s store.Store, token string, b Broadcaster, blobs *blob.Manager, a *auth.Store) *Server {
	return &Server{store: s, token: token, bc: b, blobs: blobs, auth: a}
}

// SetStore 运行期切换存储后端（setup 向导保存配置后调用）。旧后端的关闭由调用方负责。
func (s *Server) SetStore(st store.Store) {
	s.cfgMu.Lock()
	s.store = st
	s.cfgMu.Unlock()
}

// SetToken 运行期更换共享 token（重新生成 token 后调用）。
func (s *Server) SetToken(tok string) {
	s.cfgMu.Lock()
	s.token = tok
	s.cfgMu.Unlock()
}

func (s *Server) cur() (store.Store, string) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.store, s.token
}

// CurrentStore 返回当前存储后端（TTL 清理等后台任务使用，运行期热切换后即为新后端）。
func (s *Server) CurrentStore() store.Store {
	st, _ := s.cur()
	return st
}

// Handler 返回带认证中间件的 /api 路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/register", s.handleAuthRegister)
	mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("POST /api/admin/reset-password", s.handleAdminResetPassword)
	mux.HandleFunc("POST /api/clips", s.handleCreate)
	mux.HandleFunc("POST /api/clips/image", s.handleCreateImage)
	mux.HandleFunc("GET /api/clips", s.handleList)
	mux.HandleFunc("GET /api/clips/latest", s.handleLatest)
	mux.HandleFunc("GET /api/clips/{id}", s.handleGet)
	mux.HandleFunc("GET /api/clips/{id}/blob", s.handleBlob)
	mux.HandleFunc("DELETE /api/clips/{id}", s.handleDelete)
	return s.requireAuth(mux)
}

// ---- 认证 ----

// ctxUserIDKey 携带「本次请求解析出的用户 ID」。
type ctxUserIDKey struct{}

// authUID 取本次请求的用户 ID。
func authUID(r *http.Request) int64 {
	uid, _ := r.Context().Value(ctxUserIDKey{}).(int64)
	return uid
}

// requireAuth 校验 Authorization: Bearer <设备令牌>（经账号体系解析所属用户）。
// 管理令牌仅用于服务器管理（/api/setup 等），不能访问剪贴板。
// /api/auth/*（注册/登录）本身匿名开放。
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/auth/") {
			next.ServeHTTP(w, r)
			return
		}
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if s.auth == nil {
			writeError(w, http.StatusServiceUnavailable, "auth not available")
			return
		}
		uid, ok := s.auth.VerifyToken(h[len(prefix):])
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxUserIDKey{}, uid))
		next.ServeHTTP(w, r)
	})
}

// ---- 账号（v1.2 多用户）----

// handleAuthRegister POST /api/auth/register 创建账号。
func (s *Server) handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth not available")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	u, err := s.auth.Register(req.Username, req.Password)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 首个注册用户自动接管共享令牌时代的历史记录（user_id=0 的 legacy 数据）
	if err := s.store.AdoptLegacy(r.Context(), u.ID); err != nil {
		log.Printf("auth: 首个用户 %s 接管历史记录失败（忽略）: %v", u.Username, err)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "user": u.Username, "id": u.ID})
}

// handleAuthLogin POST /api/auth/login 登录并签发设备令牌。
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth not available")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Device   string `json:"device"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	device := req.Device
	if device == "" {
		device = "unknown"
	}
	if len(device) > 32 {
		device = device[:32]
	}
	token, user, err := s.auth.Login(req.Username, req.Password, device)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token,
		"user":  user.Username,
		"id":    user.ID,
	})
}

// handleAdminResetPassword POST /api/admin/reset-password：管理令牌重置用户密码。
// 走 requireAuth（管理令牌），路径刻意不在 /api/auth/ 下。
func (s *Server) handleAdminResetPassword(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth not available")
		return
	}
	var req struct {
		Username    string `json:"username"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if err := s.auth.ResetPassword(req.Username, req.NewPassword); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAuthLogout POST /api/auth/logout 注销当前设备令牌。
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth not available")
		return
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	s.auth.RemoveDevice(h[len(prefix):])
	w.WriteHeader(http.StatusNoContent)
}

// secureEqual 常数时间字符串比较，防时序侧信道。
func secureEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// ---- 处理器 ----

type createReq struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Hash   string `json:"hash"`
	Device string `json:"device"`
}

// handleCreate POST /api/clips 上传一条文本剪贴板内容。
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Type != "text" {
		writeError(w, http.StatusBadRequest, `unsupported type, only "text" (or POST /api/clips/image for image)`)
		return
	}
	device := req.Device
	if device == "" {
		device = "unknown"
	}

	// 哈希以服务端计算为准（契约约定），客户端上报值仅供参考
	sum := sha256.Sum256([]byte(req.Text))
	hash := hex.EncodeToString(sum[:])

	uid := authUID(r)
	// 去重检查 + 写入用同一把锁串行化，避免并发重复插入
	s.mu.Lock()
	defer s.mu.Unlock()
	st, _ := s.cur()
	latest, err := st.Latest(r.Context(), uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store error: "+err.Error())
		return
	}
	if latest != nil && latest.Hash == hash {
		// 与最新一条相同：不新建、不广播，返回已有记录（200 而非 201）
		decorate(latest)
		writeJSON(w, http.StatusOK, latest)
		return
	}
	// 文本回环兜底：剪贴板往返可能改变行尾（\n ↔ \r\n），旧版客户端会把
	// 「同内容不同行尾」当新记录再传一次并广播。忽略 \r 归一化后与最新一条
	// 一致也按去重处理（返回已有记录，不新建、不广播）。
	if latest != nil && latest.Type == "text" && normalizeText(latest.Text) == normalizeText(req.Text) {
		decorate(latest)
		writeJSON(w, http.StatusOK, latest)
		return
	}

	c := &store.Clip{
		Type:         "text",
		Text:         req.Text,
		Hash:         hash,
		SourceDevice: device,
		Size:         int64(len(req.Text)), // UTF-8 字节长度
		UserID:       uid,
	}
	if err := st.Add(r.Context(), c); err != nil {
		writeError(w, http.StatusInternalServerError, "store error: "+err.Error())
		return
	}
	decorate(c)
	writeJSON(w, http.StatusCreated, c)
	if s.bc != nil {
		// 广播给除来源设备之外的所有连接
		s.bc.BroadcastNew(*c, c.SourceDevice)
	}
}

// normalizeText 忽略 \r 的文本归一化：剪贴板往返可能把 \n 变成 \r\n，
// 字节 hash 失配但可见内容一致，去重比对时按归一化形式判断。
func normalizeText(s string) string {
	return strings.ReplaceAll(s, "\r", "")
}

// createImageReq 图片上传请求体。data 为图片字节的 base64（兼容 data URL 前缀）；
// width/height 可选（浏览器端量得的像素尺寸），webp 等服务端无法解码尺寸时采用。
type createImageReq struct {
	Data   string `json:"data"`
	Device string `json:"device"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// handleCreateImage POST /api/clips/image 上传一张图片。
// 流程：解码 base64 → 嗅探类型（png/jpeg/gif/webp）→ hash 去重 → 落盘 → 建记录 → 广播。
func (s *Server) handleCreateImage(w http.ResponseWriter, r *http.Request) {
	if s.blobs == nil {
		writeError(w, http.StatusServiceUnavailable, "image storage not configured")
		return
	}
	var req createImageReq
	body := http.MaxBytesReader(w, r.Body, maxImageBody)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body（图片过大？上限 16MB）: "+err.Error())
		return
	}
	device := req.Device
	if device == "" {
		device = "unknown"
	}

	data, err := decodeImageData(req.Data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "empty image data")
		return
	}
	if len(data) > maxImageBytes {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("图片超过上限 %d MB", maxImageBytes>>20))
		return
	}

	// 类型嗅探 + 扩展名
	mime := http.DetectContentType(data[:min(512, len(data))])
	ext, ok := imageExts[mime]
	if !ok {
		writeError(w, http.StatusUnsupportedMediaType, "仅支持 png / jpeg / gif / webp，检测到 "+mime)
		return
	}

	// 尺寸：服务端能解则用服务端的（png/jpeg/gif）；解不了（webp）用客户端上报值
	width, height := decodeDims(data, mime)
	if width == 0 && req.Width > 0 && req.Height > 0 {
		width, height = req.Width, req.Height
	}

	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	rel := filepath.ToSlash(filepath.Join("files",
		time.Now().UTC().Format("20060102"), hash+"."+ext))

	uid := authUID(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	st, _ := s.cur()
	latest, err := st.Latest(r.Context(), uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store error: "+err.Error())
		return
	}
	if latest != nil && latest.Hash == hash {
		decorate(latest)
		writeJSON(w, http.StatusOK, latest) // 同一张图重复上传：去重，不落盘不广播
		return
	}

	if err := s.blobs.Save(rel, bytes.NewReader(data)); err != nil {
		writeError(w, http.StatusInternalServerError, "save image: "+err.Error())
		return
	}

	c := &store.Clip{
		Type:         "image",
		Hash:         hash,
		SourceDevice: device,
		Size:         int64(len(data)),
		Mime:         mime,
		Width:        width,
		Height:       height,
		BlobPath:     rel,
		UserID:       uid,
	}
	if err := st.Add(r.Context(), c); err != nil {
		_ = s.blobs.Delete(rel) // 回滚已落盘文件，避免孤儿附件
		writeError(w, http.StatusInternalServerError, "store error: "+err.Error())
		return
	}
	decorate(c)
	writeJSON(w, http.StatusCreated, c)
	if s.bc != nil {
		s.bc.BroadcastNew(*c, c.SourceDevice)
	}
}

// handleBlob GET /api/clips/{id}/blob 输出图片内容（带 Bearer 认证）。
func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	uid := authUID(r)
	st, _ := s.cur()
	c, err := st.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store error: "+err.Error())
		return
	}
	if c == nil || c.Type != "image" || c.BlobPath == "" || !visibleTo(c, uid) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	f, err := s.blobs.Open(c.BlobPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "blob file missing")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err == nil {
		w.Header().Set("ETag", `"`+c.Hash+`"`)
		w.Header().Set("Cache-Control", "private, max-age=86400")
	}
	name := "clipboard-" + strconv.FormatInt(id, 10)
	if ext := filepath.Ext(c.BlobPath); ext != "" {
		name += ext
	}
	mime := c.Mime
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	if fi != nil {
		http.ServeContent(w, r, name, fi.ModTime(), f)
	} else {
		http.ServeContent(w, r, name, time.Time{}, f)
	}
}

// handleList GET /api/clips?limit=50 历史列表（时间降序）。
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	uid := authUID(r)
	st, _ := s.cur()
	clips, err := st.List(r.Context(), uid, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store error: "+err.Error())
		return
	}
	if clips == nil {
		clips = []store.Clip{} // 保证空列表序列化为 "clips": [] 而非 null
	}
	for i := range clips {
		decorate(&clips[i])
	}
	writeJSON(w, http.StatusOK, map[string][]store.Clip{"clips": clips})
}

// handleLatest GET /api/clips/latest 最新一条；无记录返回 204 无 body。
func (s *Server) handleLatest(w http.ResponseWriter, r *http.Request) {
	uid := authUID(r)
	st, _ := s.cur()
	c, err := st.Latest(r.Context(), uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store error: "+err.Error())
		return
	}
	if c == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	decorate(c)
	writeJSON(w, http.StatusOK, c)
}

// handleGet GET /api/clips/{id} 单条查询。
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	uid := authUID(r)
	st, _ := s.cur()
	c, err := st.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store error: "+err.Error())
		return
	}
	if c == nil || !visibleTo(c, uid) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	decorate(c)
	writeJSON(w, http.StatusOK, c)
}

// visibleTo 归属判断：用户只见自己的条目
// （别人的条目按 404 处理，不泄露存在性）。
func visibleTo(c *store.Clip, uid int64) bool {
	return c.UserID == uid
}

// handleDelete DELETE /api/clips/{id} 删除单条（连带删除图片附件）并广播删除事件。
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	uid := authUID(r)
	st, _ := s.cur()
	// 先取元数据（拿到 BlobPath），再删记录，最后清理附件
	c, err := st.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store error: "+err.Error())
		return
	}
	if c != nil && !visibleTo(c, uid) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	deleted, err := st.Delete(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store error: "+err.Error())
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if c != nil && c.BlobPath != "" && s.blobs != nil {
		if err := s.blobs.Delete(c.BlobPath); err != nil {
			log.Printf("api: 清理附件 %s 失败（忽略）: %v", c.BlobPath, err)
		}
	}
	if s.bc != nil {
		s.bc.BroadcastDeleted(id, uid)
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseID 从路径参数解析 id；非法值按 404 处理（不可能存在的 id）。
func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "not found")
		return 0, false
	}
	return id, true
}

// ---- 内部辅助 ----

// decorate 为图片条目补上 blob_url（数据库不存派生数据）。
func decorate(c *store.Clip) {
	if c != nil && c.Type == "image" && c.BlobPath != "" {
		c.BlobURL = fmt.Sprintf("/api/clips/%d/blob", c.ID)
	}
}

// decodeImageData 解析请求里的图片数据：兼容 data URL 前缀（data:image/png;base64,xxx）与裸 base64。
func decodeImageData(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if i := strings.Index(s, ";base64,"); i >= 0 && strings.HasPrefix(s, "data:") {
		s = s[i+len(";base64,"):]
	}
	if d, err := base64.StdEncoding.DecodeString(s); err == nil {
		return d, nil
	}
	// 容忍未填充的 base64
	d, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return nil, fmt.Errorf("invalid base64 image data")
	}
	return d, nil
}

// decodeDims 尝试用标准库解码 png/jpeg/gif 的像素尺寸；webp 不支持（返回 0，由客户端上报）。
func decodeDims(data []byte, mime string) (int, int) {
	if mime == "image/webp" {
		return 0, 0
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---- 响应辅助 ----

type errorResp struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errorResp{Error: msg})
}
