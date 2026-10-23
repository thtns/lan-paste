//go:build !notray && (darwin || windows || tray)

// Package tray 提供桌面客户端的托盘/菜单栏界面：
//
//	darwin  ：菜单栏图标（NSStatusBar，cgo，需在 macOS 本机构建）
//	windows ：系统托盘（纯 Win32 syscall，可 CGO_ENABLED=0 交叉编译）
//	linux   ：加 -tags tray 时启用（依赖 gtk，需 cgo）；默认无托盘纯后台
//
// 菜单：同步状态 / 暂停·恢复同步 / 打开历史页面 / 退出。
package tray

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/url"
	"os/exec"
	"runtime"
	"sync"

	"fyne.io/systray"
)

// Options 托盘初始化参数。
type Options struct {
	Device       string           // 设备名（tooltip 展示）
	ServerURL    string           // 服务端地址（「打开历史页面」跳转目标）
	Token        string           // 设备令牌（历史页面经 ?token= 免登录直连）
	OnPause      func(pause bool) // 暂停/恢复回调（接引擎 SetPaused；未配置模式可为 nil）
	Unconfigured bool             // true：待配置模式（显示「设置…」，隐藏同步相关项）
	OnSetup      func()           // 点击「设置…」回调（main 打开应用内设置向导）
}

var (
	mu         sync.Mutex
	mStatus    *systray.MenuItem
	mSetup     *systray.MenuItem // 「设置…」：应用内设置向导入口（任何状态都可见）
	mPause     *systray.MenuItem // 「暂停/恢复同步」：配置完成后显示
	mHistory   *systray.MenuItem // 「打开历史页面」：配置完成后显示
	lastStatus = struct {
		connected bool
		devices   int
	}{connected: false, devices: 0}
	quitFn  func()
	onPause func(pause bool)
)

// Supported 报告当前平台/构建是否提供托盘界面。
func Supported() bool { return true }

// Quit 退出托盘（Run 返回）。ctx 取消时由 main 调用。
func Quit() {
	mu.Lock()
	fn := quitFn
	mu.Unlock()
	if fn != nil {
		fn()
	}
}

// SetStatus 更新菜单里的连接状态（引擎 OnStatus 回调线程调用，并发安全）。
// 托盘尚未就绪时缓存，onReady 后应用。
func SetStatus(connected bool, devices int) {
	mu.Lock()
	lastStatus.connected = connected
	lastStatus.devices = devices
	m := mStatus
	mu.Unlock()
	if m == nil {
		return
	}
	applyStatus(m, connected, devices)
}

func applyStatus(m *systray.MenuItem, connected bool, devices int) {
	if connected {
		if devices > 0 {
			m.SetTitle(fmt.Sprintf("已连接 · %d 台设备在线", devices))
		} else {
			m.SetTitle("已连接")
		}
	} else {
		m.SetTitle("连接中…")
	}
}

// Run 启动托盘并阻塞直到退出（systray 要求在主 goroutine 运行）。
func Run(o Options) error {
	mu.Lock()
	onPause = o.OnPause
	mu.Unlock()

	onReady := func() {
		systray.SetIcon(iconBytes())
		tooltip := "LAN Paste — " + o.Device
		if o.Unconfigured {
			tooltip += "（待配置）"
		} else {
			tooltip += "（" + o.ServerURL + "）"
		}
		systray.SetTooltip(tooltip)

		// 直接构建到包级变量（onReady 在主线程执行一次；后续由 SetStatus 更新）
		mu.Lock()
		mStatus = systray.AddMenuItem("连接中…", "当前同步状态")
		mStatus.Disable()
		mSetup = systray.AddMenuItem("设置…", "配置服务器地址与账号（应用内完成，不开浏览器）")
		systray.AddSeparator()
		mPause = systray.AddMenuItem("暂停同步", "暂停双向同步（不上传也不写入）")
		mHistory = systray.AddMenuItem("打开历史页面", "在浏览器打开 "+o.ServerURL)
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("退出", "退出 LAN Paste 客户端")

		if o.Unconfigured {
			mStatus.SetTitle("尚未配置：请点击下方「设置…」")
			mPause.Hide()
			mHistory.Hide()
			mSetup.SetTitle("设置…（必填）")
		} else {
			// 已配置也保留入口：令牌失效/换服务器/换账号时重新登录
			mSetup.SetTitle("账号与设置…")
		}
		st := lastStatus
		quitFn = systray.Quit
		mu.Unlock()
		applyStatus(mStatus, st.connected, st.devices)

		go func() {
			paused := false
			for range mPause.ClickedCh {
				paused = !paused
				mu.Lock()
				fn := onPause
				mu.Unlock()
				if fn != nil {
					fn(paused)
				}
				if paused {
					mPause.SetTitle("恢复同步")
					mPause.Check()
				} else {
					mPause.SetTitle("暂停同步")
					mPause.Uncheck()
				}
			}
		}()
		go func() {
			for range mSetup.ClickedCh {
				mu.Lock()
				fn := o.OnSetup
				mu.Unlock()
				if fn != nil {
					fn()
				}
			}
		}()
		go func() {
			for range mHistory.ClickedCh {
				// 重配置走进程重启，o.ServerURL/o.Token 恒为当前配置；
				// ?token= 让网页免登录直连（页面会立即从地址栏抹掉 token）
				if u := o.ServerURL; u != "" {
					openBrowser(u + "/?token=" + url.QueryEscape(o.Token))
				}
			}
		}()
		go func() {
			for range mQuit.ClickedCh {
				Quit()
			}
		}()
	}

	systray.Run(onReady, nil)
	return nil
}

// openBrowser 用系统默认浏览器打开 URL。
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// iconBytes 返回托盘图标字节：macOS/Linux 用 PNG，
// Windows 用 ICO（fyne systray 的 Windows 后端经 LoadImage 加载，只认 .ico）。
func iconBytes() []byte {
	img := drawIcon(32)
	if runtime.GOOS == "windows" {
		return icoFromNRGBA(img)
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// drawIcon 程序化绘制托盘图标（不引入图片资源文件）：
// 靛蓝圆角方块 + 三条白色文本线（呼应 LAN Paste 剪贴板主题）。
func drawIcon(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	bg := color.NRGBA{R: 0x4f, G: 0x46, B: 0xe5, A: 0xff}
	for y := 2; y < size-2; y++ {
		for x := 2; x < size-2; x++ {
			// 粗略圆角：四角各裁掉 3px
			if (x < 5 && y < 5) || (x >= size-5 && y < 5) ||
				(x < 5 && y >= size-5) || (x >= size-5 && y >= size-5) {
				continue
			}
			img.SetNRGBA(x, y, bg)
		}
	}
	white := color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	lines := [][4]int{{9, 9, 22}, {9, 15, 22}, {9, 21, 16}} // y, x 起, x 止
	for _, l := range lines {
		for x := l[1]; x <= l[2]; x++ {
			img.SetNRGBA(x, l[0], white)
			img.SetNRGBA(x, l[0]+1, white)
		}
	}
	return img
}

// icoFromNRGBA 把图像编码为 ICO（经典 32bpp DIB 条目，Windows LoadImage 完全兼容）。
// 布局：ICONDIR(6B) + ICONDIRENTRY(16B) + BITMAPINFOHEADER(40B) + XOR(BGRA,自底向上) + AND 掩码。
func icoFromNRGBA(img *image.NRGBA) []byte {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	rowSize := w * 4 // 32bpp 天然 4 字节对齐
	andRow := (w + 31) / 32 * 4
	dib := make([]byte, 40+rowSize*h+andRow*h)
	binary.LittleEndian.PutUint32(dib[0:4], 40)           // biSize
	binary.LittleEndian.PutUint32(dib[4:8], uint32(w))    // biWidth
	binary.LittleEndian.PutUint32(dib[8:12], uint32(h*2)) // biHeight = XOR+AND 两层
	binary.LittleEndian.PutUint16(dib[12:14], 1)          // biPlanes
	binary.LittleEndian.PutUint16(dib[14:16], 32)         // biBitCount
	// 像素：BGRA，自底向上
	for y := 0; y < h; y++ {
		srcY := h - 1 - y
		for x := 0; x < w; x++ {
			i := srcY*img.Stride + x*4
			o := 40 + y*rowSize + x*4
			dib[o] = img.Pix[i+2]   // B
			dib[o+1] = img.Pix[i+1] // G
			dib[o+2] = img.Pix[i]   // R
			dib[o+3] = img.Pix[i+3] // A
		}
	}
	// AND 掩码区保持全 0：不透明度完全由像素 alpha 表达

	ico := make([]byte, 22)
	ico[2], ico[3] = 1, 0 // 类型：图标
	ico[4], ico[5] = 1, 0 // 条目数：1
	ico[6] = byte(w & 0xff)
	ico[7] = byte(h & 0xff)
	ico[10], ico[11] = 1, 0
	ico[12], ico[13] = 32, 0
	binary.LittleEndian.PutUint32(ico[14:18], uint32(len(dib)))
	binary.LittleEndian.PutUint32(ico[18:22], 22)
	return append(ico, dib...)
}
