package server

import (
	"strings"

	"github.com/skip2/go-qrcode"
)

// renderQRASCII 把 url 渲染为 ASCII 半块字符二维码（▀▄█ + 空格）。
// 包含 2 模块 quiet zone，确保 80 列终端可完整显示。
// 每个字符行编码 2 个模块行（上半=高 4 位、下半=低 4 位），
// 从而把二维码高度减半，终端里更紧凑。
func renderQRASCII(url string) (string, error) {
	qr, err := qrcode.New(url, qrcode.Low)
	if err != nil {
		return "", err
	}
	bitmap := qr.Bitmap() // [][]bool，含 quiet zone（默认 4 模块边框）

	// 把 quiet zone 裁剪到 2 模块（减小体积，仍可扫）。
	bm := trimQuietZone(bitmap, 2)

	rows := len(bm)
	if rows == 0 {
		return "", nil
	}
	cols := len(bm[0])

	var b strings.Builder
	// 每次处理两行模块，映射为一个字符行
	for y := 0; y < rows; y += 2 {
		for x := 0; x < cols; x++ {
			top := bm[y][x]
			var bottom bool
			if y+1 < rows {
				bottom = bm[y+1][x]
			}
			switch {
			case top && bottom:
				b.WriteString("█")
			case top && !bottom:
				b.WriteString("▀")
			case !top && bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// trimQuietZone 裁剪二维码 bitmap 的 quiet zone 到 target 模块。
// go-qrcode 的 Bitmap() 默认带 4 模块 quiet zone；裁到 target 更紧凑。
func trimQuietZone(bitmap [][]bool, target int) [][]bool {
	full := 4 // go-qrcode 默认 quiet zone
	if target >= full {
		return bitmap
	}
	trim := full - target
	if trim <= 0 {
		return bitmap
	}
	out := make([][]bool, 0, len(bitmap)-2*trim)
	for y := trim; y < len(bitmap)-trim; y++ {
		row := make([]bool, len(bitmap[y])-2*trim)
		copy(row, bitmap[y][trim:len(bitmap[y])-trim])
		out = append(out, row)
	}
	return out
}
