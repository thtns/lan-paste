//go:build darwin

package clipboard

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"strings"
)

// darwinClipboard 通过 pbpaste/pbcopy 读写文本；
// 图片走 osascript 的 «class PNGf» 数据类（系统自带，无 cgo，保持可交叉编译）。
type darwinClipboard struct{}

// New 返回 macOS 剪贴板实现。
func New() Clipboard { return darwinClipboard{} }

// Name 返回后端名称（用于启动日志）。
func (darwinClipboard) Name() string { return "pbpaste/pbcopy + osascript(PNGf)" }

func (darwinClipboard) ReadText() (string, error) {
	out, err := exec.Command("pbpaste").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (darwinClipboard) WriteText(text string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

// hasImage 先用 clipboard info 探测是否含 PNGf 类（无图片时避免二次 spawn）。
func hasImage() bool {
	out, err := exec.Command("osascript", "-e", "clipboard info").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "PNGf")
}

// ReadImage 读取剪贴板里的图片（PNG 字节）；无图片返回 (nil, nil)。
// `the clipboard as «class PNGf»` 的输出形如 «data PNGf89504E47...»，为十六进制文本。
func (darwinClipboard) ReadImage() (*Image, error) {
	if !hasImage() {
		return nil, nil
	}
	out, err := exec.Command("osascript", "-e", "the clipboard as «class PNGf»").Output()
	if err != nil {
		return nil, fmt.Errorf("osascript 读取图片失败: %w", err)
	}
	const marker = "PNGf"
	s := string(out)
	i := strings.Index(s, marker)
	if i < 0 {
		return nil, nil // 探测说有、取回没有：按无图片处理（内容刚变化等竞态）
	}
	hexStr := s[i+len(marker):]
	if j := strings.IndexRune(hexStr, '»'); j >= 0 {
		hexStr = hexStr[:j]
	}
	data, err := hex.DecodeString(strings.TrimSpace(hexStr))
	if err != nil {
		return nil, fmt.Errorf("PNGf 十六进制解析失败: %w", err)
	}
	if !bytes.HasPrefix(data, pngMagic) {
		return nil, fmt.Errorf("剪贴板 PNGf 数据不是 PNG")
	}
	img := &Image{Data: data}
	if cfg, err := png.DecodeConfig(bytes.NewReader(data)); err == nil {
		img.Width, img.Height = cfg.Width, cfg.Height
	}
	return img, nil
}

// WriteImage 把 PNG 写入系统剪贴板。
// 字节先落临时文件再交给 osascript 读取，避免大图的命令行参数长度限制：
//
//	set the clipboard to (read (POSIX file "<path>") as «class PNGf»)
func (darwinClipboard) WriteImage(img *Image) error {
	if len(img.Data) == 0 {
		return fmt.Errorf("图片内容为空")
	}
	if !bytes.HasPrefix(img.Data, pngMagic) {
		return fmt.Errorf("WriteImage 仅接受 PNG 编码")
	}
	tmp, err := os.CreateTemp("", "lanpaste-*.png")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(img.Data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	script := fmt.Sprintf(`set the clipboard to (read (POSIX file "%s") as «class PNGf»)`, path)
	if out, err := exec.Command("osascript", "-e", script).CombinedOutput(); err != nil {
		return fmt.Errorf("osascript 写入图片失败: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
