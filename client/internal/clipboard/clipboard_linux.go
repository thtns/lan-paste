//go:build linux

package clipboard

import (
	"bytes"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// linuxClipboard 依次探测 wl-clipboard（Wayland）、xsel、xclip（X11）。
// 后端在 New 时确定；都缺失时 Read/Write 返回 ErrUnavailable，引擎继续运行。
type linuxClipboard struct {
	backend  string
	readCmd  []string
	writeCmd []string
}

// New 探测可用后端并返回 Linux 剪贴板实现。
func New() Clipboard {
	if _, err1 := exec.LookPath("wl-paste"); err1 == nil {
		if _, err2 := exec.LookPath("wl-copy"); err2 == nil {
			return &linuxClipboard{
				backend:  "wl-clipboard",
				readCmd:  []string{"wl-paste", "--no-newline"},
				writeCmd: []string{"wl-copy"},
			}
		}
	}
	if _, err := exec.LookPath("xsel"); err == nil {
		return &linuxClipboard{
			backend:  "xsel",
			readCmd:  []string{"xsel", "--clipboard", "--output"},
			writeCmd: []string{"xsel", "--clipboard", "--input"},
		}
	}
	if _, err := exec.LookPath("xclip"); err == nil {
		return &linuxClipboard{
			backend:  "xclip",
			readCmd:  []string{"xclip", "-selection", "clipboard", "-out"},
			writeCmd: []string{"xclip", "-selection", "clipboard", "-in"},
		}
	}
	return &linuxClipboard{backend: "none"}
}

// Name 返回后端名称（用于启动日志）。
func (c *linuxClipboard) Name() string {
	if c.backend == "none" {
		return "不可用（未安装 wl-clipboard/xsel/xclip）"
	}
	return c.backend
}

// ReadText 读取剪贴板文本。
// 输出写入临时文件再读回：cmd.Stdout 用 *os.File 时子进程直接持有文件句柄，
// 避免 os/exec 管道被 xclip/xsel 后台驻留进程长期占住导致 Wait 挂起。
func (c *linuxClipboard) ReadText() (string, error) {
	if c.backend == "none" {
		return "", ErrUnavailable
	}
	dir, err := os.MkdirTemp("", "lanpaste-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	outPath := filepath.Join(dir, "clip.txt")
	f, err := os.Create(outPath)
	if err != nil {
		return "", err
	}

	cmd := exec.Command(c.readCmd[0], c.readCmd[1:]...)
	cmd.Stdout = f
	runErr := cmd.Run()
	f.Close()
	if runErr != nil {
		return "", fmt.Errorf("%s 读取失败: %w", c.backend, runErr)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WriteText 把文本写入剪贴板。
// wl-copy / xsel / xclip 均会在后台驻留以持有选区，主进程在收完 stdin 后即退出，
// cmd.Run 随之返回；驻留进程独立存活，剪贴板内容在客户端退出后依然可用。
func (c *linuxClipboard) WriteText(text string) error {
	if c.backend == "none" {
		return ErrUnavailable
	}
	cmd := exec.Command(c.writeCmd[0], c.writeCmd[1:]...)
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s 写入失败: %w", c.backend, err)
	}
	return nil
}

// ---- 图片（v0.2）：wl-paste/wl-copy 与 xclip 支持按 MIME 目标类型读写；xsel 不支持 ----

// readImageCmd 返回读取 image/png 的命令；不支持图片的后端返回 nil。
func (c *linuxClipboard) readImageCmd() []string {
	switch c.backend {
	case "wl-clipboard":
		return []string{"wl-paste", "-t", "image/png", "--no-newline"}
	case "xclip":
		return []string{"xclip", "-selection", "clipboard", "-t", "image/png", "-out"}
	default:
		return nil // xsel / none：无图片能力
	}
}

// ReadImage 读取剪贴板里的 PNG 图片；目标类型不存在（无图片）时返回 (nil, nil)。
// 输出走临时文件：与 ReadText 同理，避免 xclip 驻留进程占住管道导致 Wait 挂起。
func (c *linuxClipboard) ReadImage() (*Image, error) {
	argv := c.readImageCmd()
	if argv == nil {
		return nil, nil // xsel/none：按"无图片"处理，文本路径照常工作
	}
	dir, err := os.MkdirTemp("", "lanpaste-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	outPath := filepath.Join(dir, "clip.png")
	f, err := os.Create(outPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = f
	runErr := cmd.Run()
	f.Close()
	if runErr != nil {
		return nil, nil // 多数后端以非零退出表示目标类型不存在
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || !bytes.HasPrefix(data, pngMagic) {
		return nil, nil // 空或非 PNG 目标
	}
	img := &Image{Data: data}
	if cfg, err := png.DecodeConfig(bytes.NewReader(data)); err == nil {
		img.Width, img.Height = cfg.Width, cfg.Height
	}
	return img, nil
}

// WriteImage 把 PNG 写入剪贴板（wl-copy / xclip 以 -t image/png 声明类型并驻留）。
func (c *linuxClipboard) WriteImage(img *Image) error {
	if c.backend == "none" {
		return ErrUnavailable
	}
	var argv []string
	switch c.backend {
	case "wl-clipboard":
		argv = []string{"wl-copy", "-t", "image/png", "--fork"}
	case "xclip":
		argv = []string{"xclip", "-selection", "clipboard", "-t", "image/png", "-in"}
	default:
		return fmt.Errorf("%w（%s）", ErrNoImageSupport, c.backend)
	}
	if !bytes.HasPrefix(img.Data, pngMagic) {
		return fmt.Errorf("WriteImage 仅接受 PNG 编码")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(img.Data)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s 写入图片失败: %w", c.backend, err)
	}
	return nil
}
