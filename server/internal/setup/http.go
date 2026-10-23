// setup 的 HTTP 处理器：/api/setup 系列。
//
// 鉴权策略：未配置（首次部署）时全部放开——局域网内谁先打开向导谁完成配置；
// 已配置后所有写操作要求 Bearer token（与剪贴板 API 同一个 token）。
package setup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// Server setup 端点集合。依赖全部由 main 注入，本包不感知具体存储实现。
type Server struct {
	Mgr     *Manager
	DataDir string
	Version string

	// EffectiveListen 当前进程实际监听的地址（main 启动时注入）。
	// 保存配置时用它判断是否真的换了端口：首次向导保存时 config.json 里还没有
	// listen 字段，不能拿空串当旧值，否则会误报"端口已修改需重启"。
	EffectiveListen string

	// CurrentToken 返回当前生效 token。
	CurrentToken func() string
	// ApplyStore 用新配置重建存储后端并热切换，返回旧后端的关闭函数。
	ApplyStore func(ctx context.Context, cfg Config, dsn string) (closeOld func(), err error)
	// ApplyToken 更新运行中的 token（api + ws + token 文件）。
	ApplyToken func(tok string) error
}

// Register 把 setup 端点挂到根 mux 上（比 /api/ 前缀更具体的模式优先匹配）。
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/setup", s.handleStatus)
	mux.HandleFunc("POST /api/setup/test", s.handleTest)
	mux.HandleFunc("POST /api/setup", s.handleSave)
	mux.HandleFunc("POST /api/setup/token", s.handleRegenToken)
}

// ---- 鉴权 ----

func (s *Server) authorized(r *http.Request) bool {
	if !s.Mgr.Configured() {
		return true // 首次部署引导：放开
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	return secureEqual(h[len(prefix):], s.CurrentToken())
}

func secureEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// ---- 处理器 ----

// handleStatus GET /api/setup：当前配置状态 + 当前 token（供向导展示 / 预填）。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": s.Mgr.Configured(),
		"status":     s.Mgr.View(),
		"token":      s.CurrentToken(),
		"data_dir":   s.DataDir,
		"version":    s.Version,
	})
}

// handleTest POST /api/setup/test：测试数据库连接（可自动建库），不落盘。
func (s *Server) handleTest(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var cfg Config
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	s.Mgr.InheritPassword(&cfg) // 页面不回显密码：留空时沿用现有值，测试才能通过
	if err := cfg.Normalize(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Mgr.Test(r.Context(), cfg.DB); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSave POST /api/setup：测试 → 落盘 config.json → 热切换存储后端。
func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var cfg Config
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	s.Mgr.InheritPassword(&cfg) // 必须在连接测试之前：留空 = 沿用现有密码
	if err := cfg.Normalize(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	oldListen := s.EffectiveListen
	if oldListen == "" {
		oldListen = s.Mgr.Listen()
	}
	if oldListen == "" {
		oldListen = DefaultListen // 进程由默认端口启动且无任何显式配置
	}

	// 内存存储直接保存；数据库存储先测连通（向导已测试过，此处兜底复核）
	if cfg.DB.Store != StoreMemory {
		if err := s.Mgr.Test(r.Context(), cfg.DB); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "连接测试失败: " + err.Error()})
			return
		}
	}
	if err := s.Mgr.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 热切换存储后端；失败时配置已保存，服务仍跑在旧后端上（重启后即按新配置）
	var applyErr string
	if s.ApplyStore != nil {
		dsn := s.Mgr.DSN()
		closeOld, err := s.ApplyStore(r.Context(), cfg, dsn)
		if err != nil {
			applyErr = err.Error()
		} else if closeOld != nil {
			closeOld()
		}
	}
	restartRequired := oldListen != cfg.Listen
	code := http.StatusOK
	if applyErr != "" {
		code = http.StatusInternalServerError
	}
	writeJSON(w, code, map[string]any{
		"ok":               applyErr == "",
		"error":            applyErr,
		"listen":           cfg.Listen,
		"restart_required": restartRequired,
	})
}

// handleRegenToken POST /api/setup/token：重新生成共享 token 并全端生效。
func (s *Server) handleRegenToken(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.ApplyToken == nil {
		writeError(w, http.StatusServiceUnavailable, "token rotation not available")
		return
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		writeError(w, http.StatusInternalServerError, "生成随机 token 失败: "+err.Error())
		return
	}
	tok := hex.EncodeToString(buf)
	if err := s.ApplyToken(tok); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok})
}

// ---- 响应辅助 ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
