// lanpaste-client 是 LAN Paste 的桌面常驻客户端：
// 轮询本地剪贴板上传到服务端，并接收服务端 WebSocket 推送写入本地剪贴板。
// 单二进制、纯 Go（macOS 托盘需 cgo），支持 macOS/Windows/Linux。
// 首次配置在程序内完成：原生系统对话框（非浏览器）收集服务器与账号，
// 登录换取设备令牌后自动开始同步。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"

	"lanpaste/client/internal/autostart"
	"lanpaste/client/internal/clipboard"
	"lanpaste/client/internal/setup"
	"lanpaste/client/internal/syncer"
	"lanpaste/client/internal/tray"
)

const version = "0.4.3"

// errSilent 表示提示信息已打印过（如用法、引导信息），main 无需重复输出。
var errSilent = errors.New("silent")

const usageHeader = `LAN Paste 剪贴板同步客户端 v%s

用法：
  %s [-server URL] [-token TOKEN] [-device NAME] [-poll DUR]   前台运行同步代理
  %s -install                                                    注册开机自启后退出
  %s -uninstall                                                  移除开机自启后退出

选项：
`

func main() {
	err := run(os.Args[1:])
	if err == nil {
		return
	}
	if errors.Is(err, errSilent) {
		// 正常静默退出（-version/-help/单实例重复启动）：以 0 退出，
		// 否则 macOS LaunchAgent（SuccessfulExit=false）会当作崩溃反复拉起。
		return
	}
	fmt.Fprintln(os.Stderr, "lanpaste-client:", err)
	os.Exit(1)
}

func run(args []string) error {
	r, err := resolve(args)
	if err != nil {
		return err
	}
	if r.uninstall {
		return autostart.Uninstall(log.Default())
	}
	// 未配置 + -install：终端引导（安装自启前必须先有配置，
	// 配置由命令行 flag 或应用内向导完成）。
	if r.guide && r.install {
		printGuide(r.configPath)
		return errSilent
	}

	// 有显式 flag（或文件尚不存在）时回写配置：
	// -install 的自启条目无参运行，必须能从配置文件读齐参数
	if r.saveWanted {
		if err := saveConfig(r.configPath, r.cfg); err != nil {
			return fmt.Errorf("保存配置到 %s 失败: %w", r.configPath, err)
		}
		log.Printf("配置已保存到 %s", r.configPath)
	}
	if r.install {
		bin, err := executablePath()
		if err != nil {
			return err
		}
		return autostart.Install(bin, log.Default())
	}

	// Windows 的 -H=windowsgui 构建没有控制台：日志尽早切到文件，
	// 保证单实例锁冲突等早期消息也能留痕。
	if runtime.GOOS == "windows" {
		logDir := filepath.Dir(r.configPath)
		if err := os.MkdirAll(logDir, 0o700); err == nil {
			logPath := filepath.Join(logDir, "client.log")
			var f *os.File
			if st, serr := os.Stat(logPath); serr == nil && st.Size() > 8<<20 {
				f, _ = os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			} else {
				f, _ = os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			}
			if f != nil {
				// 文件必须在前面：GUI 子系统进程的 stderr 是无效句柄，
				// MultiWriter 遇到第一个写错误就返回，stderr 放前面会吞掉所有日志
				log.SetOutput(io.MultiWriter(f, os.Stderr))
				defer f.Close()
				log.Printf("lanpaste-client %s 进程启动（Windows GUI 模式）", version)
			}
		}
	}

	// 单实例保护：重复启动直接退出（避免菜单栏出现多个图标）。
	// 锁在进程存活期间持有，退出自动释放；版本/安装/卸载等快速命令不受影响，
	// 且放在 -install 之后——应用运行中也能执行「安装自启」。
	lockDir := filepath.Dir(r.configPath)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	instLock := flock.New(filepath.Join(lockDir, "client.lock"))
	var locked bool
	for i := 0; ; i++ {
		var err error
		locked, err = instLock.TryLock()
		if err != nil {
			log.Printf("警告：实例锁获取失败（忽略并继续）: %v", err)
			break
		}
		if locked {
			break
		}
		if i >= 10 { // 约 3 秒：覆盖「重启时旧进程尚未完全释放锁」的竞态窗口
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !locked {
		log.Printf("已有 LAN Paste 客户端在运行（实例锁：%s），本次启动退出", instLock.Path())
		alertGUIAsync("LAN Paste 已在运行。请查看系统托盘/菜单栏图标（Windows 可能在任务栏左下角 ^ 折叠区）；\n若尚未配置，请点击菜单中的「设置…」完成账号登录。")
		return errSilent
	}
	defer instLock.Close()

	device := r.cfg.Device
	if device == "" {
		device = defaultDeviceName()
	}
	poll := time.Duration(r.cfg.PollMS) * time.Millisecond
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	if poll < 50*time.Millisecond {
		log.Printf("警告：轮询间隔 %s 过小，已调整为 50ms", poll)
		poll = 50 * time.Millisecond
	}

	clip := clipboard.New()

	trayOn := !r.noTray && tray.Supported()
	var statusCb func(connected bool, devices int)
	if trayOn {
		statusCb = tray.SetStatus // 托盘菜单展示连接状态
	}
	buildEngine := func(cfg Config) *syncer.Engine {
		dev := cfg.Device
		if dev == "" {
			dev = device
		}
		return syncer.New(syncer.Options{
			ServerURL:    cfg.Server,
			Token:        cfg.Token,
			Device:       dev,
			PollInterval: poll,
			OnStatus:     statusCb,
		}, clip)
	}

	backend := "unknown"
	if n, ok := clip.(interface{ Name() string }); ok {
		backend = n.Name()
	}
	log.Printf("lanpaste-client %s 启动：server=%s device=%s poll=%s 剪贴板后端=%s",
		version, r.cfg.Server, device, poll, backend)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 应用内设置向导（原生系统对话框，不经浏览器）：
	// 成功 → 保存配置；取消/失败 → ok=false。
	runWizard := func(cur Config) (Config, bool) {
		res, err := setup.Run(setup.Credentials{
			Server: cur.Server, Username: cur.User, Device: device,
		})
		if err == setup.ErrCancelled {
			return Config{}, false
		}
		if err != nil {
			log.Printf("设置向导不可用: %v", err)
			alertGUI("设置向导不可用：" + err.Error())
			return Config{}, false
		}
		cfg := Config{Server: res.Server, Token: res.Token, Device: res.Device, User: res.Username}
		if err := saveConfig(r.configPath, cfg); err != nil {
			log.Printf("保存配置失败: %v", err)
			alertGUI("保存配置失败：" + err.Error())
			return Config{}, false
		}
		log.Printf("配置已保存到 %s", r.configPath)
		setup.NotifyConfigSaved() // Windows 托盘常被折叠，必须给出可见确认
		return cfg, true
	}

	// 未配置：先跑一次向导（终端构建为终端问答），成功则直接进入已配置模式。
	// 取消：有托盘 → 待配置托盘（菜单「设置…」重试）；无托盘 → 打印引导退出。
	if r.guide {
		if cfg, ok := runWizard(r.cfg); ok {
			r.cfg = cfg
			r.guide = false
			if cfg.Device != "" {
				device = cfg.Device
			}
			log.Printf("首次配置完成：同步即将开始（服务端 %s，设备 %s）", r.cfg.Server, device)
		} else if !trayOn {
			printGuide(r.configPath)
			return errSilent
		}
	}

	// 托盘/菜单栏模式（darwin/windows 默认启用；-no-tray 或无界面构建时走纯后台）
	if trayOn {
		var engine *syncer.Engine
		if !r.guide {
			engine = buildEngine(r.cfg)
			go engine.Run(ctx)
		}
		go func() {
			<-ctx.Done()
			tray.Quit()
		}()

		// 托盘「设置…」：重新跑向导（换服务器/令牌失效/换账号）。
		// 保存成功 → 重启自身。顺序很关键：先释放实例锁，再拉起新进程，
		// 否则新进程会因抢锁失败而退出（闪退）。
		cfgCh := make(chan Config, 1)
		onSetup := func() {
			cfg, ok := runWizard(r.cfg)
			if !ok {
				return
			}
			select {
			case cfgCh <- cfg:
			default:
			}
		}
		go func() {
			cfg := <-cfgCh
			log.Printf("配置已更新：重启客户端以应用（服务端 %s，设备 %s）", cfg.Server, cfg.Device)
			exe, e := os.Executable()
			if e == nil {
				if p, pe := filepath.EvalSymlinks(exe); pe == nil {
					exe = p
				}
			}
			_ = instLock.Close() // 先释放实例锁
			if e == nil {
				_ = exec.Command(exe).Start()
			}
			os.Exit(0)
		}()

		var onPause func(bool)
		if engine != nil {
			onPause = engine.SetPaused
		}
		log.Printf("托盘/菜单栏界面已启动（-no-tray 可关闭），退出请用托盘菜单「退出」")
		if err := tray.Run(tray.Options{
			Device:       device,
			ServerURL:    r.cfg.Server,
			Token:        r.cfg.Token,
			Unconfigured: r.guide,
			OnPause:      onPause,
			OnSetup:      onSetup,
		}); err != nil {
			log.Printf("托盘退出: %v", err)
		}
		log.Printf("已退出")
		return nil
	}

	buildEngine(r.cfg).Run(ctx)
	log.Printf("已退出")
	return nil
}

// resolution 是 resolve 的结果：配置文件与 flag 合并后的最终意图。
type resolution struct {
	cfg        Config
	configPath string
	saveWanted bool // 需要把合并结果回写配置文件
	install    bool
	uninstall  bool
	noTray     bool // 禁用托盘/菜单栏，纯后台运行
	guide      bool // 配置不完整，需要进入设置向导
}

// resolve 解析 flag、加载并合并配置（flag 优先于配置文件），
// 不产生任何副作用（不写文件、不启动引擎），便于单测。
func resolve(args []string) (*resolution, error) {
	fs := flag.NewFlagSet("lanpaste-client", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), usageHeader, version, fs.Name(), fs.Name(), fs.Name())
		fs.PrintDefaults()
	}
	var (
		flagServer  = fs.String("server", "", "服务端地址，如 http://10.0.0.5:8080（缺 scheme 时默认 http://）")
		flagToken   = fs.String("token", "", "认证 token（服务端首次启动时打印，或见其数据目录 token 文件）")
		flagDevice  = fs.String("device", "", "设备名，默认取本机 hostname")
		flagPoll    = fs.Duration("poll", 500*time.Millisecond, "本地剪贴板轮询间隔（最小 50ms）")
		flagInstall = fs.Bool("install", false, "注册开机自启（使用当前二进制）后退出")
		flagUninst  = fs.Bool("uninstall", false, "移除开机自启后退出")
		flagNoTray  = fs.Bool("no-tray", false, "禁用托盘/菜单栏界面，纯后台运行")
		flagConfig  = fs.String("config", "", "配置文件路径（默认 ~/.config/lanpaste/config.json，或环境变量 LANPASTE_CLIENT_CONFIG）")
		flagVersion = fs.Bool("version", false, "打印版本后退出")
	)
	if err := fs.Parse(args); err != nil {
		return nil, errSilent // flag 包已经打印了错误与用法
	}
	if *flagVersion {
		fmt.Printf("lanpaste-client %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return nil, errSilent
	}
	if *flagInstall && *flagUninst {
		return nil, errors.New("-install 与 -uninstall 不能同时指定")
	}

	// 显式传入的 flag：值优先于配置文件
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	configPath := *flagConfig
	if configPath == "" {
		if env := os.Getenv("LANPASTE_CLIENT_CONFIG"); env != "" {
			configPath = env
		} else {
			configPath = defaultConfigPath()
		}
	}

	cfg, exists, err := loadConfig(configPath)
	if err != nil {
		return nil, err
	}
	if setFlags["server"] {
		cfg.Server = normalizeServerURL(*flagServer)
	}
	if setFlags["token"] {
		cfg.Token = *flagToken
	}
	if setFlags["device"] {
		cfg.Device = *flagDevice
	}
	if setFlags["poll"] {
		cfg.PollMS = int(flagPoll.Milliseconds())
	}

	r := &resolution{
		cfg:        cfg,
		configPath: configPath,
		install:    *flagInstall,
		uninstall:  *flagUninst,
		noTray:     *flagNoTray,
	}
	// -uninstall 不依赖配置；其余情况配置不完整则进入设置向导
	if !r.uninstall && (cfg.Server == "" || cfg.Token == "") {
		r.guide = true
		return r, nil
	}
	// 配置完整时：有除 -config 外的显式 flag（server/token/device/poll），
	// 或配置文件尚不存在 → 回写。-config 只是定位文件，不触发回写；-uninstall 不落盘。
	hasOverride := false
	for name := range setFlags {
		if name != "config" && name != "no-tray" {
			hasOverride = true
		}
	}
	r.saveWanted = !r.uninstall && (hasOverride || !exists)
	return r, nil
}

// executablePath 返回当前二进制的绝对真实路径（解析符号链接，
// /tmp → /private/tmp 这类必须展开，launchd/systemd 才能正确启动）。
func executablePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Abs(p)
}

// defaultDeviceName 取 hostname 作为默认设备名。
func defaultDeviceName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return strings.TrimSuffix(h, ".local")
}
