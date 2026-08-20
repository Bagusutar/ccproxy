// 把 ccproxy 的图标栅格化成透明底的 .ico。
//
// 环境里没有任何 SVG/位图工具（rsvg、ImageMagick、Pillow 全无，装不了），
// 但这个图标只由「圆头粗线段」和「圆」构成——用有向距离场直接求覆盖率
// 即可得到抗锯齿结果，不需要任何依赖。
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
)

type rgb struct{ r, g, b float64 }

func hex(s string) rgb {
	var v uint32
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | uint32(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | uint32(c-'a'+10)
		}
	}
	return rgb{float64(v>>16&255) / 255, float64(v>>8&255) / 255, float64(v&255) / 255}
}

// 线段的有向距离
func segDist(px, py, x1, y1, x2, y2 float64) float64 {
	vx, vy := x2-x1, y2-y1
	wx, wy := px-x1, py-y1
	l2 := vx*vx + vy*vy
	t := 0.0
	if l2 > 0 {
		t = math.Max(0, math.Min(1, (wx*vx+wy*vy)/l2))
	}
	dx, dy := px-(x1+t*vx), py-(y1+t*vy)
	return math.Hypot(dx, dy)
}

type shape struct {
	kind   string // seg | circle
	x1, y1 float64
	x2, y2 float64
	rad    float64 // 线宽的一半 / 圆半径
	col    rgb
}

// trunk 是主干（输入侧那根线和左边的大节点）的颜色。
//
// 取中性灰而不是界面里那个 #1d1d1f：图标已经改成透明底，而 .ico 是位图，
// 没法像界面的 SVG 那样跟着系统主题翻转颜色。实测纯黑主干在 Windows 深色
// 任务栏（#202020）上 16/24/32/48 四个尺寸全部消失，整枚图标只剩三条
// 悬空的支路。这个灰在浅色 #f3f3f3 与深色 #202020 上都立得住。
const trunk = "#6e6e73"

// 按 z 序排列：先干线与支路，再端点圆——后画的覆盖先画的
var shapes = []shape{
	{"seg", 30, 32, 46, 16, 3.25, hex("#0f97a8")}, {"seg", 46, 16, 58, 16, 3.25, hex("#0f97a8")},
	{"seg", 30, 32, 46, 48, 3.25, hex("#7b4ddc")}, {"seg", 46, 48, 54, 48, 3.25, hex("#7b4ddc")},
	{"seg", 30, 32, 55, 32, 3.25, hex("#0071e3")},
	{"seg", 8, 32, 30, 32, 3.25, hex(trunk)},
	{"circle", 58, 16, 0, 0, 4, hex("#0f97a8")},
	{"circle", 54, 48, 0, 0, 4, hex("#7b4ddc")},
	{"circle", 55, 32, 0, 0, 6, hex("#0071e3")},
	{"circle", 30, 32, 0, 0, 6.5, hex(trunk)},
}

// render 画一张 size×size 的透明底图标。
func render(size int) *image.NRGBA {
	const SS = 4 // 每像素 4×4 超采样
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	scale := float64(size) / 64
	// 内容整体的位移，与 ui/index.html 里那段字形 SVG 的 translate 一致
	const tx, ty = -1.25, 0.0

	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var ar, ag, ab, aa float64
			for sy := 0; sy < SS; sy++ {
				for sx := 0; sx < SS; sx++ {
					// 像素中心（设计坐标系）
					ux := (float64(px) + (float64(sx)+0.5)/SS) / scale
					uy := (float64(py) + (float64(sy)+0.5)/SS) / scale

					var cr, cg, cb, ca float64

					gx := ux - tx
					gy := uy - ty
					for _, s := range shapes {
						var d float64
						if s.kind == "seg" {
							d = segDist(gx, gy, s.x1, s.y1, s.x2, s.y2) - s.rad
						} else {
							d = math.Hypot(gx-s.x1, gy-s.y1) - s.rad
						}
						if d <= 0 {
							cr, cg, cb, ca = s.col.r, s.col.g, s.col.b, 1
						}
					}
					ar += cr * ca
					ag += cg * ca
					ab += cb * ca
					aa += ca
				}
			}
			n := float64(SS * SS)
			if aa > 0 {
				// 预乘后取平均，再还原，避免边缘出现暗边
				img.SetNRGBA(px, py, color.NRGBA{
					uint8(math.Round(ar / aa * 255)),
					uint8(math.Round(ag / aa * 255)),
					uint8(math.Round(ab / aa * 255)),
					uint8(math.Round(aa / n * 255)),
				})
			}
		}
	}
	return img
}

// writeICO 打包多尺寸 PNG。Vista 起 .ico 支持内嵌 PNG，省去 BMP+掩码的麻烦。
func writeICO(path string, sizes []int) error {
	type ent struct {
		size int
		data []byte
	}
	var ents []ent
	for _, s := range sizes {
		var buf bytes.Buffer
		img := render(s)
		if err := png.Encode(&buf, img); err != nil {
			return err
		}
		ents = append(ents, ent{s, buf.Bytes()})
	}

	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, uint16(0))
	binary.Write(&out, binary.LittleEndian, uint16(1)) // 1 = ICO
	binary.Write(&out, binary.LittleEndian, uint16(len(ents)))
	offset := 6 + 16*len(ents)
	for _, e := range ents {
		b := byte(e.size)
		if e.size >= 256 {
			b = 0 // 256 用 0 表示
		}
		out.WriteByte(b)
		out.WriteByte(b)
		out.WriteByte(0)
		out.WriteByte(0)
		binary.Write(&out, binary.LittleEndian, uint16(1))
		binary.Write(&out, binary.LittleEndian, uint16(32))
		binary.Write(&out, binary.LittleEndian, uint32(len(e.data)))
		binary.Write(&out, binary.LittleEndian, uint32(offset))
		offset += len(e.data)
	}
	for _, e := range ents {
		out.Write(e.data)
	}
	return os.WriteFile(path, out.Bytes(), 0o644)
}

func main() {
	if err := writeICO("icon.ico", []int{16, 24, 32, 48, 64, 128, 256}); err != nil {
		panic(err)
	}
	// 透明底的图标没法自己判断落在什么颜色上，只看一张图看不出问题。
	// 这里把同一枚图标合成到浅色与深色两种系统底色上并排导出，
	// 主干那种接近纯黑的颜色一旦在深色底上消失，这张图会直接显示出来。
	if err := writeGrounds("preview.png"); err != nil {
		panic(err)
	}
	println("ok")
}

// groundLight / groundDark 取 Windows 11 任务栏的实际底色。
var groundLight = color.NRGBA{243, 243, 243, 255}
var groundDark = color.NRGBA{32, 32, 32, 255}

// writeGrounds 导出一张检查用的对照图：两行底色，每行是几个真实尺寸的图标。
func writeGrounds(path string) error {
	sizes := []int{256, 64, 48, 32, 24, 16}
	const pad, gap = 24, 20
	w := pad*2 + 256
	for _, s := range sizes[1:] {
		w += gap + s
	}
	rowH := pad*2 + 256
	out := image.NewNRGBA(image.Rect(0, 0, w, rowH*2))

	for row, ground := range []color.NRGBA{groundLight, groundDark} {
		top := row * rowH
		band := image.Rect(0, top, w, top+rowH)
		draw.Draw(out, band, &image.Uniform{ground}, image.Point{}, draw.Src)
		x := pad
		for _, s := range sizes {
			// 底对齐，方便比较小尺寸下的可读性
			y := top + pad + 256 - s
			draw.Draw(out, image.Rect(x, y, x+s, y+s), render(s), image.Point{}, draw.Over)
			x += s + gap
		}
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, out)
}
