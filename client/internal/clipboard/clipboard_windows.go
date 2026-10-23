//go:build windows

package clipboard

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsClipboard 通过 Win32 API 读写 CF_UNICODETEXT，不依赖 cgo。
type windowsClipboard struct{}

// New 返回 Windows 剪贴板实现。
func New() Clipboard { return windowsClipboard{} }

// Name 返回后端名称（用于启动日志）。
func (windowsClipboard) Name() string { return "Win32 Clipboard API" }

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	pOpenClipboard              = user32.NewProc("OpenClipboard")
	pCloseClipboard             = user32.NewProc("CloseClipboard")
	pEmptyClipboard             = user32.NewProc("EmptyClipboard")
	pGetClipboardData           = user32.NewProc("GetClipboardData")
	pSetClipboardData           = user32.NewProc("SetClipboardData")
	pIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")

	pGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	pGlobalLock   = kernel32.NewProc("GlobalLock")
	pGlobalUnlock = kernel32.NewProc("GlobalUnlock")
	pGlobalFree   = kernel32.NewProc("GlobalFree")
	pGlobalSize   = kernel32.NewProc("GlobalSize")
)

const (
	cfUnicodeText = 13 // CF_UNICODETEXT
	gmemMoveable  = 0x0002
)

// openClipboard 轮询尝试打开剪贴板：其他进程可能短暂持有它。
// LazyProc.Call 在成功时 err 仍为非 nil 的 Errno(0)，因此只能以返回值判断成败。
func openClipboard() error {
	var last error
	for i := 0; i < 10; i++ {
		r, _, err := pOpenClipboard.Call(0)
		if r != 0 {
			return nil
		}
		last = err
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("OpenClipboard 失败（剪贴板被占用）: %v", last)
}

func (windowsClipboard) ReadText() (string, error) {
	// 无文本格式视为空剪贴板，返回空串而不是错误
	if r, _, _ := pIsClipboardFormatAvailable.Call(cfUnicodeText); r == 0 {
		return "", nil
	}
	if err := openClipboard(); err != nil {
		return "", err
	}
	defer pCloseClipboard.Call()

	h, _, err := pGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", fmt.Errorf("GetClipboardData: %v", err)
	}
	ptr, _, err := pGlobalLock.Call(h)
	if ptr == 0 {
		return "", fmt.Errorf("GlobalLock: %v", err)
	}
	defer pGlobalUnlock.Call(h)

	size, _, _ := pGlobalSize.Call(h)
	n := int(size) / 2 // 字节数 → UTF-16 单元数
	if n <= 0 {
		return "", nil
	}
	u16 := unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), n)
	// UTF16ToString 在第一个 NUL 处截断，正是 CF_UNICODETEXT 的语义
	return windows.UTF16ToString(u16), nil
}

func (windowsClipboard) WriteText(text string) error {
	data, _ := windows.UTF16FromString(text) // 含 NUL 终止符
	size := uintptr(len(data)) * 2

	if err := openClipboard(); err != nil {
		return err
	}
	defer pCloseClipboard.Call()

	if r, _, err := pEmptyClipboard.Call(); r == 0 {
		return fmt.Errorf("EmptyClipboard: %v", err)
	}

	h, _, err := pGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return fmt.Errorf("GlobalAlloc: %v", err)
	}
	ptr, _, err := pGlobalLock.Call(h)
	if ptr == 0 {
		pGlobalFree.Call(h)
		return fmt.Errorf("GlobalLock: %v", err)
	}
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), len(data)), data)
	pGlobalUnlock.Call(h)

	// 成功后内存所有权移交系统，失败时需自行释放
	if r, _, err := pSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		pGlobalFree.Call(h)
		return fmt.Errorf("SetClipboardData: %v", err)
	}
	return nil
}

// ---- 图片（v0.2）：优先 PNG 注册格式，回退 CF_DIB / CF_DIBV5 转 PNG ----

const (
	cfDIB       = 8
	cfDIBV5     = 17
	biRGB       = 0
	biBitfields = 3
)

var pRegisterClipboardFormat = user32.NewProc("RegisterClipboardFormatW")

// pngClipboardFormat 返回 "PNG" 注册格式 ID（系统会话内恒定）。
func pngClipboardFormat() uintptr {
	s := windows.StringToUTF16Ptr("PNG")
	r, _, _ := pRegisterClipboardFormat.Call(uintptr(unsafe.Pointer(s)))
	return r
}

// getClipboardBinary 取回指定格式的原始字节（调用前须已 IsClipboardFormatAvailable）。
func getClipboardBinary(format uintptr) ([]byte, error) {
	if err := openClipboard(); err != nil {
		return nil, err
	}
	defer pCloseClipboard.Call()

	h, _, err := pGetClipboardData.Call(format)
	if h == 0 {
		return nil, fmt.Errorf("GetClipboardData(%d): %v", format, err)
	}
	ptr, _, err := pGlobalLock.Call(h)
	if ptr == 0 {
		return nil, fmt.Errorf("GlobalLock: %v", err)
	}
	defer pGlobalUnlock.Call(h)

	size, _, _ := pGlobalSize.Call(h)
	out := make([]byte, size)
	copy(out, unsafe.Slice((*byte)(unsafe.Pointer(ptr)), size))
	return out, nil
}

// ReadImage 读取剪贴板图片；无任何图片格式时返回 (nil, nil)。
// 浏览器/截图工具多直接提供 PNG；画图/Office 等提供 DIB，这里统一转成 PNG。
func (windowsClipboard) ReadImage() (*Image, error) {
	for _, id := range []uintptr{pngClipboardFormat(), cfDIBV5, cfDIB} {
		if id == 0 {
			continue
		}
		if r, _, _ := pIsClipboardFormatAvailable.Call(id); r == 0 {
			continue
		}
		data, err := getClipboardBinary(id)
		if err != nil {
			return nil, err
		}
		if id == pngClipboardFormat() && bytes.HasPrefix(data, pngMagic) {
			img := &Image{Data: data}
			if cfg, err := png.DecodeConfig(bytes.NewReader(data)); err == nil {
				img.Width, img.Height = cfg.Width, cfg.Height
			}
			return img, nil
		}
		return dibToPNG(data)
	}
	return nil, nil
}

// WriteImage 把 PNG 解码后以 32bpp CF_DIB 写入剪贴板（Windows 各应用兼容性最好）。
func (windowsClipboard) WriteImage(img *Image) error {
	dib, err := pngToDIB(img.Data)
	if err != nil {
		return err
	}
	if err := openClipboard(); err != nil {
		return err
	}
	defer pCloseClipboard.Call()

	if r, _, err := pEmptyClipboard.Call(); r == 0 {
		return fmt.Errorf("EmptyClipboard: %v", err)
	}
	h, _, err := pGlobalAlloc.Call(gmemMoveable, uintptr(len(dib)))
	if h == 0 {
		return fmt.Errorf("GlobalAlloc: %v", err)
	}
	ptr, _, err := pGlobalLock.Call(h)
	if ptr == 0 {
		pGlobalFree.Call(h)
		return fmt.Errorf("GlobalLock: %v", err)
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(ptr)), len(dib)), dib)
	pGlobalUnlock.Call(h)

	if r, _, err := pSetClipboardData.Call(cfDIB, h); r == 0 {
		pGlobalFree.Call(h)
		return fmt.Errorf("SetClipboardData: %v", err)
	}
	return nil
}

// dibToPNG 把剪贴板里的 DIB（BITMAPINFOHEADER/V4/V5，24/32bpp，BI_RGB 或标准掩码
// BI_BITFIELDS）转成 PNG。行序默认自底向上；biHeight 为负表示自顶向下。
func dibToPNG(dib []byte) (*Image, error) {
	if len(dib) < 40 {
		return nil, fmt.Errorf("DIB 数据过短")
	}
	hdrSize := binary.LittleEndian.Uint32(dib[0:4])
	if hdrSize < 40 || int(hdrSize) > len(dib) {
		return nil, fmt.Errorf("DIB 头大小异常: %d", hdrSize)
	}
	width := int32(binary.LittleEndian.Uint32(dib[4:8]))
	height := int32(binary.LittleEndian.Uint32(dib[8:12]))
	bitCount := binary.LittleEndian.Uint16(dib[14:16])
	compression := binary.LittleEndian.Uint32(dib[16:20])

	if width <= 0 || height == 0 {
		return nil, fmt.Errorf("DIB 尺寸异常: %dx%d", width, height)
	}
	topDown := height < 0
	h := int(height)
	if topDown {
		h = -h
	}
	switch {
	case compression == biRGB && (bitCount == 24 || bitCount == 32):
	case compression == biBitfields && bitCount == 32:
		// 常见标准掩码即 BGRA 字节序，按 32bpp 处理
	default:
		return nil, fmt.Errorf("不支持的 DIB 格式: %dbpp compression=%d", bitCount, compression)
	}

	off := hdrSize
	if bitCount <= 8 { // 调色板（本实现不处理 ≤8bpp 像素，但必须跳过调色板区）
		clrUsed := binary.LittleEndian.Uint32(dib[32:36])
		if clrUsed == 0 {
			clrUsed = 1 << bitCount
		}
		off += clrUsed * 4
	}
	rowSize := (int(width)*int(bitCount) + 31) / 32 * 4
	pix := dib[off:]
	if len(pix) < rowSize*h {
		return nil, fmt.Errorf("DIB 像素数据不完整: 需 %d 字节，得到 %d", rowSize*h, len(pix))
	}

	out := image.NewNRGBA(image.Rect(0, 0, int(width), h))
	for y := 0; y < h; y++ {
		srcY := y
		if !topDown {
			srcY = h - 1 - y
		}
		row := pix[srcY*rowSize : srcY*rowSize+rowSize]
		for x := 0; x < int(width); x++ {
			i := x * int(bitCount) / 8
			b, g, r := row[i], row[i+1], row[i+2]
			a := uint8(255)
			if bitCount == 32 {
				a = row[i+3]
				if a == 0 {
					a = 255 // 旧应用常把 alpha 写 0，避免整图透明
				}
			}
			out.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: a})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return &Image{Data: buf.Bytes(), Width: int(width), Height: h}, nil
}

// pngToDIB 把 PNG 解码并编码为 32bpp BI_RGB 的 CF_DIB（自底向上行序）。
func pngToDIB(data []byte) ([]byte, error) {
	m, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("PNG 解码失败: %w", err)
	}
	b := m.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("图片尺寸异常: %dx%d", w, h)
	}
	rowSize := (w*32 + 31) / 32 * 4
	dib := make([]byte, 40+rowSize*h)
	binary.LittleEndian.PutUint32(dib[0:4], 40)         // biSize = BITMAPINFOHEADER
	binary.LittleEndian.PutUint32(dib[4:8], uint32(w))  // biWidth
	binary.LittleEndian.PutUint32(dib[8:12], uint32(h)) // biHeight（正数 = 自底向上）
	binary.LittleEndian.PutUint16(dib[12:14], 1)        // biPlanes
	binary.LittleEndian.PutUint16(dib[14:16], 32)       // biBitCount
	binary.LittleEndian.PutUint32(dib[16:20], biRGB)    // biCompression

	pix := dib[40:]
	for y := 0; y < h; y++ {
		srcY := h - 1 - y
		for x := 0; x < w; x++ {
			c := color.NRGBAModel.Convert(m.At(b.Min.X+x, b.Min.Y+srcY)).(color.NRGBA)
			i := y*rowSize + x*4
			pix[i], pix[i+1], pix[i+2], pix[i+3] = c.B, c.G, c.R, c.A
		}
	}
	return dib, nil
}
