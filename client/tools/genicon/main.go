// genicon 生成 LAN Paste 应用图标 PNG（macOS iconset / 托盘图标共用同一设计）：
// 靛蓝圆角方块 + 三条白色文本线。
//
// 用法：go run ./tools/genicon -size 1024 -out icon.png
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"image"
	"image/color"
	"image/png"
	"os"
)

func main() {
	size := flag.Int("size", 1024, "边长（正方形，像素）")
	out := flag.String("out", "icon.png", "输出路径")
	ico := flag.Bool("ico", false, "输出为 ICO 容器（内嵌 PNG，供 rsrc 嵌入 Windows exe）")
	flag.Parse()

	img := image.NewNRGBA(image.Rect(0, 0, *size, *size))
	bg := color.NRGBA{R: 0x4f, G: 0x46, B: 0xe5, A: 0xff}
	white := color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}

	const base = 32.0 // 设计稿基于 32x32
	s := float64(*size) / base
	m := 2 * s   // 外边距
	cut := 3 * s // 圆角裁切
	for y := int(m); y < *size-int(m); y++ {
		for x := int(m); x < *size-int(m); x++ {
			fx, fy := float64(x), float64(y)
			inCorner := (fx < m+cut && fy < m+cut) ||
				(fx >= float64(*size)-m-cut && fy < m+cut) ||
				(fx < m+cut && fy >= float64(*size)-m-cut) ||
				(fx >= float64(*size)-m-cut && fy >= float64(*size)-m-cut)
			if !inCorner {
				img.SetNRGBA(x, y, bg)
			}
		}
	}
	// 三条白色文本线（与托盘图标同款布局，按比例缩放）
	for _, l := range [][3]float64{{9, 9, 23}, {15, 9, 23}, {21, 9, 16}} {
		y0, x0, x1 := l[0]*s, l[1]*s, l[2]*s
		for y := int(y0); y < int(y0+2*s); y++ {
			for x := int(x0); x < int(x1); x++ {
				img.SetNRGBA(x, y, white)
			}
		}
	}

	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, img); err != nil {
		panic(err)
	}
	data := pngBytes.Bytes()
	if *ico {
		data = wrapICO(data, *size)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		panic(err)
	}
}

// wrapICO 把单张 PNG 包成 ICO 容器（Vista+ 支持 PNG 压缩条目）。
func wrapICO(png []byte, size int) []byte {
	out := make([]byte, 22, 22+len(png))
	out[2], out[3] = 1, 0 // 类型：图标
	out[4], out[5] = 1, 0 // 条目数：1
	w := byte(size & 0xff)
	if size >= 256 {
		w = 0 // 256 用 0 表示
	}
	out[6], out[7] = w, w // 宽 / 高
	// 条目内字段（绝对偏移）：8=色板数 9=保留 10..11=planes 12..13=位深
	//                        14..17=数据大小 18..21=数据偏移（紧跟头部）
	out[10], out[11] = 1, 0
	out[12], out[13] = 32, 0
	binary.LittleEndian.PutUint32(out[14:18], uint32(len(png)))
	binary.LittleEndian.PutUint32(out[18:22], 22)
	return append(out, png...)
}
