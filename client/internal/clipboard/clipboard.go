// Package clipboard 提供三平台剪贴板读写的统一接口：
//
//	darwin  ：exec pbpaste / pbcopy（文本）；osascript（图片，PNGf 数据类）
//	windows ：x/sys/windows 调 Win32 剪贴板 API（CF_UNICODETEXT / PNG / CF_DIB）
//	linux   ：优先 wl-clipboard（Wayland），回退 xsel、xclip（X11）
//
// 全部实现均为纯 Go（CGO_ENABLED=0 可交叉编译）。
// 图片统一约定：Image.Data 一律为 PNG 编码字节，平台差异在各自实现内部转换。
package clipboard

import (
	"errors"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// Clipboard 是各平台实现必须满足的统一接口（文本）。
type Clipboard interface {
	ReadText() (string, error)
	WriteText(string) error
}

// Image 表示一张剪贴板图片。Data 统一为 PNG 编码字节，
// Width / Height 为像素尺寸（未知可为 0）。
type Image struct {
	Data   []byte
	Width  int
	Height int
}

// ImageClipboard 是可选的图片扩展接口。
// 支持图片的平台实现同时实现它；引擎通过类型断言探测，
// 未实现的平台继续只同步文本（收到图片条目时记日志跳过）。
type ImageClipboard interface {
	// ReadImage 读取剪贴板图片；剪贴板当前没有图片时返回 (nil, nil)。
	ReadImage() (*Image, error)
	// WriteImage 把一张 PNG 图片写入系统剪贴板。
	WriteImage(img *Image) error
}

// ErrUnavailable 表示当前系统没有可用的剪贴板后端
// （例如 Linux 上 wl-paste/wl-copy、xsel、xclip 均未安装）。
// 引擎据此输出一次性告警并继续运行。
var ErrUnavailable = errors.New("未找到可用的剪贴板工具（需要 wl-clipboard、xsel 或 xclip 之一）")

// ErrNoImageSupport 表示该平台后端不支持图片（如 Linux 的 xsel 只有文本）。
var ErrNoImageSupport = errors.New("当前剪贴板后端不支持图片")

// pngMagic PNG 文件签名（各平台实现共享）。
var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// FixEncoding 修正剪贴板文本编码：
// 部分应用（微信/QQ 等国内软件）往剪贴板纯文本类型写入 GBK 字节而非 UTF-8，
// 直接上传会变成一堆 U+FFFD。检测到非法 UTF-8 时按 GB18030（GBK 超集）尝试转码；
// 无法恢复时原样返回（上传时非法字节会被替换为 U+FFFD，至少不丢记录）。
func FixEncoding(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	dec := simplifiedchinese.GB18030.NewDecoder()
	if out, err := dec.Bytes([]byte(s)); err == nil && utf8.Valid(out) {
		return string(out)
	}
	return s
}

// HasCJK 判断字符串是否包含中日韩字符（转码结果合理性参考）。
func HasCJK(s string) bool {
	for _, r := range s {
		if (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF) {
			return true
		}
	}
	return false
}
