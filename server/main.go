// LAN Paste 服务端入口：REST API + WebSocket 广播 + 内嵌 Web UI + 引导配置。
// 详见 docs/PROTOCOL.md（API 契约）与 server/README.md（运行说明）。
//
// 配置优先级：命令行 flag 显式指定 > config.json（向导生成）> 默认值。
// 首次部署推荐流程：跑 install.sh → 浏览器打开服务端地址 → 向导页里选数据库、
// 测试连接、保存（存储后端即时热切换，改监听地址才需要重启）。
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"lanpaste/server/internal/api"
	"lanpaste/server/internal/auth"
	"lanpaste/server/internal/blob"
	"lanpaste/server/internal/setup"
	"lanpaste/server/internal/store"
	"lanpaste/server/internal/store/memstore"
	"lanpaste/server/internal/store/mysqlstore"
	"lanpaste/server/internal/store/pgstore"
	"lanpaste/server/internal/ws"
)

//go:embed all:web
var webFS embed.FS

const version = "0.3.0"

func main() {
	dataDirFlag := flag.String("data-dir", defaultDataDir(), "数据目录（config.json / token / 图片附件）")
	listenFlag := flag.String("listen", "", "HTTP 监听地址（留空则用 config.json 或默认 :8080）")
	storeFlag := flag.String("store", "", `存储后端覆盖："memory" / "postgres" / "mysql"（留空则用 config.json 或默认 memory）`)
	configFlag := flag.String("config", "", "配置文件路径（默认 <data-dir>/config.json）")
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println("lanpaste-server", version)
		return
	}
	dataDir := *dataDirFlag
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatalf("创建数据目录 %s 失败: %v", dataDir, err)
	}
	configPath := *configFlag
	if configPath == "" {
		configPath = filepath.Join(dataDir, "config.json")
	}

	// ---- 配置：flag 显式指定 > config.json > 默认值 ----
	mgr, err := setup.Load(configPath)
	if err != nil {
		log.Fatalf("%v", err)
	}
	listenAddr := *listenFlag
	if listenAddr == "" {
		listenAddr = mgr.Listen()
	}
	if listenAddr == "" {
		listenAddr = setup.DefaultListen
	}
	storeType := *storeFlag
	if storeType == "" {
		storeType = mgr.StoreType()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- 初始存储后端 ----
	dsn := mgr.DSN()
	if storeType == setup.StorePostgres && os.Getenv("PASTE_PG_DSN") != "" {
		dsn = os.Getenv("PASTE_PG_DSN") // 兼容 v1.0 的环境变量方式
	}
	curStore, closeStore, err := buildStore(ctx, storeType, dsn)
	if err != nil {
		log.Fatalf("初始化存储后端 %s 失败: %v", storeType, err)
	}

	// ---- token / 附件 / ws / api ----
	token, err := loadToken(dataDir)
	if err != nil {
		log.Fatalf("加载 token 失败: %v", err)
	}
	blobs, err := blob.New(filepath.Join(dataDir, "files"))
	if err != nil {
		log.Fatalf("%v", err)
	}
	authStore, err := auth.Load(filepath.Join(dataDir, "users.json"))
	if err != nil {
		log.Fatalf("%v", err)
	}
	hub := ws.NewHub(token, authStore)
	apiSrv := api.New(curStore, token, hub, blobs, authStore)

	// 向导/设置页保存配置后立即触发一次 TTL 清理（非阻塞），保留天数改动即时生效
	sweepNow := make(chan struct{}, 1)

	// ---- setup 引导回调（热切换 / token 轮换） ----
	setupSrv := &setup.Server{
		Mgr:             mgr,
		DataDir:         dataDir,
		Version:         version,
		EffectiveListen: listenAddr, // 供保存配置时判断端口是否真的变化
		CurrentToken:    func() string { return tokenAtomic.get() },
		ApplyStore: func(ctx context.Context, cfg setup.Config, newDSN string) (func(), error) {
			newSt, closeNew, err := buildStore(ctx, cfg.DB.Store, newDSN)
			if err != nil {
				return nil, err
			}
			apiSrv.SetStore(newSt)
			oldClose := closeStore
			curStore = newSt
			closeStore = closeNew
			log.Printf("setup: 存储后端已切换为 %s", cfg.DB.Store)
			select {
			case sweepNow <- struct{}{}:
			default: // 已有待处理的触发信号
			}
			return func() {
				if oldClose != nil {
					oldClose()
				}
			}, nil
		},
		ApplyToken: func(tok string) error {
			if os.Getenv("PASTE_TOKEN") != "" {
				return fmt.Errorf("当前 token 由环境变量 PASTE_TOKEN 覆盖，无法在线更换（先去掉该环境变量）")
			}
			tpath := filepath.Join(dataDir, "token")
			if err := os.WriteFile(tpath, []byte(tok+"\n"), 0o600); err != nil {
				return fmt.Errorf("写入 token 文件失败: %w", err)
			}
			apiSrv.SetToken(tok)
			hub.SetToken(tok)
			tokenAtomic.set(tok)
			log.Printf("setup: 访问 token 已更换并写入 %s（各客户端需更新）", tpath)
			return nil
		},
	}

	// ---- 路由 ----
	root := http.NewServeMux()
	root.Handle("/api/", apiSrv.Handler())
	setupSrv.Register(root) // /api/setup*（比 /api/ 前缀更具体，优先匹配）
	root.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok")) // 探活，无认证（契约）
	})
	root.HandleFunc("/download", handleDownload) // 手机扫码接入页
	root.Handle("/ws", hub)
	root.Handle("/", staticHandler())

	srv := &http.Server{Addr: listenAddr, Handler: root}
	go func() {
		state := "已完成引导"
		if !mgr.Configured() {
			state = "未配置（请打开 http://<本机地址>" + listenAddr + " 完成部署向导）"
		}
		log.Printf("LAN Paste 服务端 v%s 已启动：监听 %s（存储 %s，数据目录 %s，%s）",
			version, listenAddr, storeType, dataDir, state)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	go ttlJanitor(ctx, mgr, apiSrv, blobs, sweepNow)

	<-ctx.Done()
	log.Printf("收到退出信号，正在关闭…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("优雅关闭超时: %v", err)
	}
	if closeStore != nil {
		closeStore()
	}
}

// tokenAtomic 是当前 token 的进程内镜像（loadToken / ApplyToken 写，setup 回调读）。
// 简单场景下用互斥锁足够；避免 setup 包反向依赖 main 的变量。
var tokenAtomic atomicToken

type atomicToken struct {
	v string
}

func (a *atomicToken) set(s string) { a.v = s }
func (a *atomicToken) get() string  { return a.v }

// ttlJanitor 周期清理超过保留期的历史（连附件一起删）。
// 「历史保留天数」为 0 时不清理；每次 tick 重新读配置，改动无需重启即生效。
// 设置页保存配置后会通过 sweepNow 触发一次立即清理。
func ttlJanitor(ctx context.Context, mgr *setup.Manager, apiSrv *api.Server, blobs *blob.Manager, sweepNow chan struct{}) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Minute): // 启动后 1 分钟先扫一次
	case <-sweepNow:
	}
	sweep := func() {
		days := mgr.RetentionDays()
		if days <= 0 {
			return
		}
		cutoff := time.Now().UTC().AddDate(0, 0, -days)
		paths, err := apiSrv.CurrentStore().DeleteOlderThan(ctx, cutoff)
		if err != nil {
			log.Printf("TTL 清理失败（%d 天前）: %v", days, err)
			return
		}
		if len(paths) == 0 {
			return
		}
		removed := 0
		for _, p := range paths {
			if err := blobs.Delete(p); err != nil {
				log.Printf("TTL 清理附件 %s 失败（忽略）: %v", p, err)
				continue
			}
			removed++
		}
		log.Printf("TTL 清理：删除 %d 条 %d 天前的记录（附件已删 %d 个）", len(paths), days, removed)
	}
	sweep()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		case <-sweepNow:
			sweep()
		}
	}
}

// buildStore 按类型构建存储后端。返回实例与关闭函数（memory 的为 nil）。
func buildStore(ctx context.Context, storeType, dsn string) (store.Store, func(), error) {
	switch storeType {
	case "", setup.StoreMemory:
		log.Printf("存储后端：memory（容量 1000 条，重启后数据丢失）")
		return memstore.New(), nil, nil
	case setup.StorePostgres:
		if dsn == "" {
			return nil, nil, fmt.Errorf("postgres 需要 DSN（向导配置或环境变量 PASTE_PG_DSN），例如 postgres://paste:pass@10.0.0.8:5432/paste?sslmode=disable")
		}
		pgs, err := pgstore.New(ctx, dsn) // 内含幂等建表 + v1.1 补列迁移
		if err != nil {
			return nil, nil, err
		}
		log.Printf("存储后端：postgres")
		return pgs, pgs.Close, nil
	case setup.StoreMySQL:
		if dsn == "" {
			return nil, nil, fmt.Errorf("mysql 需要 DSN（请通过部署向导配置）")
		}
		ms, err := mysqlstore.New(ctx, dsn)
		if err != nil {
			return nil, nil, err
		}
		log.Printf("存储后端：mysql")
		return ms, ms.Close, nil
	default:
		return nil, nil, fmt.Errorf("未知的存储类型 %q（支持 memory / postgres / mysql）", storeType)
	}
}

// staticHandler 托管内嵌的 server/web/ 静态文件，index.html 为目录默认页。
// 无 SPA 回退：未知路径直接 404（契约约定）。
func staticHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("内嵌 web 资源异常: %v", err)
	}
	return http.FileServer(http.FS(sub))
}

// loadToken 加载共享 token：环境变量 PASTE_TOKEN 优先；否则读取 <data-dir>/token；
// 文件不存在则生成 64 位 hex（32 随机字节）并写回文件、打印到 stdout。
func loadToken(dataDir string) (string, error) {
	if t := os.Getenv("PASTE_TOKEN"); t != "" {
		tokenAtomic.set(t)
		return t, nil
	}
	path := filepath.Join(dataDir, "token")
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			tokenAtomic.set(t)
			return t, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机 token 失败: %w", err)
	}
	token := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("写入 token 文件 %s 失败: %w", path, err)
	}
	fmt.Println("========================================")
	fmt.Printf("  已生成访问 token（首次启动）: %s\n", token)
	fmt.Printf("  已保存至: %s（可用环境变量 PASTE_TOKEN 覆盖）\n", path)
	fmt.Println("========================================")
	tokenAtomic.set(token)
	return token, nil
}

// defaultDataDir 数据目录默认值：~/.local/share/lanpaste，取不到 home 时退回相对路径 data/。
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "data"
	}
	return filepath.Join(home, ".local", "share", "lanpaste")
}
