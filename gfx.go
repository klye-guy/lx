package main

// gfx.go — drawing pictures in a terminal.
//
// Three ways, best first:
//
//	kitty   real pixels via the kitty graphics protocol (kitty, WezTerm,
//	        Ghostty, Konsole)
//	sixel   real pixels, 256 colors (foot, Konsole, xterm -ti vt340,
//	        mlterm, Windows Terminal, contour)
//	blocks  "▀" half-blocks, two pixels per cell, in 24-bit or 256 colors;
//	        works in any color terminal, over ssh, in tmux, and in less -R
//
// "auto" asks the terminal what it supports rather than guessing from
// $TERM: it sends a kitty probe, a cell-size query and a Device
// Attributes request, and reads the answers back from /dev/tty. Every
// terminal answers Device Attributes, so the wait ends as soon as that
// reply arrives.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type gfxMode int

const (
	gBlocks gfxMode = iota
	gKitty
	gSixel
)

var gfxName = map[gfxMode]string{gBlocks: "blocks", gKitty: "kitty", gSixel: "sixel"}

type termGfx struct {
	mode         gfxMode
	cellW, cellH int // pixels per character cell
	truecolor    bool
}

func detectGfx(want string) termGfx {
	g := termGfx{mode: gBlocks, cellW: 8, cellH: 16}
	ct := os.Getenv("COLORTERM")
	g.truecolor = ct == "truecolor" || ct == "24bit"

	var ws winsize
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); e == 0 &&
		ws.Col > 0 && ws.Row > 0 && ws.X > 0 && ws.Y > 0 {
		g.cellW, g.cellH = int(ws.X)/int(ws.Col), int(ws.Y)/int(ws.Row)
	}

	switch want {
	case "blocks":
		return g
	case "kitty":
		g.mode = gKitty
	case "sixel":
		g.mode = gSixel
	}
	if !isTTY(os.Stdout) {
		if want == "auto" {
			return g // piped: only blocks survive into a file or pager
		}
	}
	// tmux and screen swallow graphics unless specially configured
	inMux := os.Getenv("TMUX") != "" || os.Getenv("STY") != "" ||
		strings.HasPrefix(os.Getenv("TERM"), "screen") || strings.HasPrefix(os.Getenv("TERM"), "tmux")
	if want == "auto" && inMux {
		return g
	}

	kitty, sixel, cw, ch := queryTerm()
	if cw > 0 && ch > 0 {
		g.cellW, g.cellH = cw, ch
	}
	if want == "auto" {
		switch {
		case kitty:
			g.mode = gKitty
		case sixel:
			g.mode = gSixel
		}
	}
	if g.cellW < 2 || g.cellH < 4 {
		g.cellW, g.cellH = 8, 16
	}
	return g
}

// queryTerm asks the terminal directly. Raw mode is held for at most a
// second and always restored.
func queryTerm() (kitty, sixel bool, cellW, cellH int) {
	fd, err := syscall.Open("/dev/tty", syscall.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return
	}
	defer syscall.Close(fd)
	var old syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&old))); e != 0 {
		return
	}
	raw := old
	raw.Lflag &^= syscall.ICANON | syscall.ECHO
	raw.Cc[syscall.VMIN] = 0
	raw.Cc[syscall.VTIME] = 1 // reads give up after 0.1 s
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(&raw))); e != 0 {
		return
	}
	defer syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(&old)))

	q := "\x1b_Gi=31,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\" + // kitty: "can you show this 1×1 pixel?"
		"\x1b[16t" + // cell size in pixels
		"\x1b[c" // Device Attributes (everyone answers; 4 in the list = sixel)
	syscall.Write(fd, []byte(q))

	var buf []byte
	tmp := make([]byte, 256)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		n, err := syscall.Read(fd, tmp)
		if err != nil && err != syscall.EINTR && err != syscall.EAGAIN {
			break
		}
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if da1End(buf) >= 0 {
				break
			}
		}
	}
	s := string(buf)
	kitty = strings.Contains(s, "\x1b_Gi=31;OK")
	if i := strings.Index(s, "\x1b[?"); i >= 0 {
		if j := strings.IndexByte(s[i:], 'c'); j > 0 {
			for _, p := range strings.Split(s[i+3:i+j], ";") {
				if p == "4" {
					sixel = true
				}
			}
		}
	}
	if i := strings.Index(s, "\x1b[6;"); i >= 0 {
		if j := strings.IndexByte(s[i:], 't'); j > 0 {
			if h, w, ok := strings.Cut(s[i+4:i+j], ";"); ok {
				cellH, _ = strconv.Atoi(h)
				cellW, _ = strconv.Atoi(w)
			}
		}
	}
	return
}

func da1End(b []byte) int {
	i := bytes.Index(b, []byte("\x1b[?"))
	if i < 0 {
		return -1
	}
	for j := i + 3; j < len(b); j++ {
		c := b[j]
		if c == 'c' {
			return j
		}
		if !(c >= '0' && c <= '9' || c == ';') {
			return -1
		}
	}
	return -1
}

// ---------------------------------------------------------------- fitting

// fitBox says how big a picture of w×h pixels can be drawn inside an area
// of cols×rows cells while keeping its shape: the result is in the
// backend's own units (half-cell pixels for blocks, real pixels otherwise)
// plus the number of cells it covers.
func (g termGfx) fitBox(w, h, cols, rows int) (pw, ph, cw, ch int) {
	if w <= 0 || h <= 0 {
		return 0, 0, 0, 0
	}
	boxW, boxH := float64(cols*g.cellW), float64(rows*g.cellH)
	s := boxW / float64(w)
	if t := boxH / float64(h); t < s {
		s = t
	}
	physW, physH := float64(w)*s, float64(h)*s
	switch g.mode {
	case gBlocks:
		cw = int(physW/float64(g.cellW) + 0.5)
		hp := int(physH/(float64(g.cellH)/2) + 0.5) // half-cells
		cw, hp = max(cw, 1), max(hp, 1)
		cw, hp = min(cw, cols), min(hp, rows*2)
		return cw, hp, cw, (hp + 1) / 2
	default:
		pw, ph = max(int(physW), 1), max(int(physH), 1)
		cw = min((pw+g.cellW-1)/g.cellW, cols)
		ch = min((ph+g.cellH-1)/g.cellH, rows)
		return pw, ph, cw, ch
	}
}

// pixelBox is the largest picture, in real pixels, an area could show:
// what ffmpeg is asked to scale to.
func (g termGfx) pixelBox(cols, rows int) (int, int) {
	if g.mode == gBlocks {
		return max(cols*2, 16), max(rows*4, 16) // a little extra for the area filter
	}
	return cols * g.cellW, rows * g.cellH
}

// ---------------------------------------------------------------- scaling

// scaleTo resizes with an area average (sharp when shrinking, no jaggies),
// sampling at most 8×8 source pixels per output pixel so a 50-megapixel
// photo costs the same as a small one.
func scaleTo(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if sw == 0 || sh == 0 {
		return dst
	}
	get := pixelReader(src)
	for y := 0; y < h; y++ {
		y0, y1 := y*sh/h, (y+1)*sh/h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		ys := max((y1-y0)/8, 1)
		for x := 0; x < w; x++ {
			x0, x1 := x*sw/w, (x+1)*sw/w
			if x1 <= x0 {
				x1 = x0 + 1
			}
			xs := max((x1-x0)/8, 1)
			var r, g, bb, a, n uint64
			for sy := y0; sy < y1; sy += ys {
				for sx := x0; sx < x1; sx += xs {
					pr, pg, pb, pa := get(b.Min.X+sx, b.Min.Y+sy)
					r, g, bb, a = r+uint64(pr), g+uint64(pg), bb+uint64(pb), a+uint64(pa)
					n++
				}
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = uint8(r / n >> 8)
			dst.Pix[i+1] = uint8(g / n >> 8)
			dst.Pix[i+2] = uint8(bb / n >> 8)
			dst.Pix[i+3] = uint8(a / n >> 8)
		}
	}
	return dst
}

// pixelReader returns premultiplied 16-bit RGBA, with fast paths for the
// layouts the decoders actually produce.
func pixelReader(src image.Image) func(x, y int) (uint32, uint32, uint32, uint32) {
	switch s := src.(type) {
	case *image.YCbCr:
		return func(x, y int) (uint32, uint32, uint32, uint32) {
			yi, ci := s.YOffset(x, y), s.COffset(x, y)
			r, g, b := color.YCbCrToRGB(s.Y[yi], s.Cb[ci], s.Cr[ci])
			return uint32(r) * 257, uint32(g) * 257, uint32(b) * 257, 0xffff
		}
	case *image.RGBA:
		return func(x, y int) (uint32, uint32, uint32, uint32) {
			i := s.PixOffset(x, y)
			p := s.Pix[i : i+4 : i+4]
			return uint32(p[0]) * 257, uint32(p[1]) * 257, uint32(p[2]) * 257, uint32(p[3]) * 257
		}
	case *image.NRGBA:
		return func(x, y int) (uint32, uint32, uint32, uint32) {
			i := s.PixOffset(x, y)
			p := s.Pix[i : i+4 : i+4]
			a := uint32(p[3]) * 257
			return uint32(p[0]) * a / 255, uint32(p[1]) * a / 255, uint32(p[2]) * a / 255, a
		}
	}
	return func(x, y int) (uint32, uint32, uint32, uint32) { return src.At(x, y).RGBA() }
}

// ---------------------------------------------------------------- blocks

// blockLines draws img (w × 2·rows half-pixels) as rows of "▀" cells.
// Each returned line is exactly img-width cells wide and ends reset.
func (g termGfx) blockLines(img *image.RGBA) []string {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	px := func(x, y int) (r, gg, b uint8, ok bool) {
		if y >= h {
			return 0, 0, 0, false
		}
		i := img.PixOffset(x, y)
		a := img.Pix[i+3]
		if a < alphaCut {
			return 0, 0, 0, false
		}
		un := func(c uint8) uint8 { return uint8(min(int(c)*255/int(a), 255)) }
		return un(img.Pix[i]), un(img.Pix[i+1]), un(img.Pix[i+2]), true
	}
	color := func(fg bool, r, gg, b uint8) string {
		base := "38"
		if !fg {
			base = "48"
		}
		if g.truecolor {
			return fmt.Sprintf("%s;2;%d;%d;%d", base, r, gg, b)
		}
		return base + ";5;" + strconv.Itoa(xterm256(r, gg, b))
	}
	var lines []string
	for y := 0; y < h; y += 2 {
		var sb strings.Builder
		last := ""
		for x := 0; x < w; x++ {
			tr, tg, tb, top := px(x, y)
			br, bg, bb, bot := px(x, y+1)
			var code, ch string
			switch {
			case top && bot:
				code, ch = color(true, tr, tg, tb)+";"+color(false, br, bg, bb), "▀"
			case top:
				code, ch = color(true, tr, tg, tb), "▀"
			case bot:
				code, ch = color(true, br, bg, bb), "▄"
			default:
				code, ch = "", " "
			}
			if code != last {
				sb.WriteString("\x1b[0m")
				if code != "" {
					sb.WriteString("\x1b[" + code + "m")
				}
				last = code
			}
			sb.WriteString(ch)
		}
		sb.WriteString("\x1b[0m")
		lines = append(lines, sb.String())
	}
	return lines
}

// xterm256 picks the nearest entry from the 6×6×6 color cube or the 24
// grays.
func xterm256(r, g, b uint8) int {
	levels := [6]int{0, 95, 135, 175, 215, 255}
	near := func(v uint8) int {
		best, bd := 0, 1<<30
		for i, l := range levels {
			if d := (int(v) - l) * (int(v) - l); d < bd {
				best, bd = i, d
			}
		}
		return best
	}
	ri, gi, bi := near(r), near(g), near(b)
	cr, cg, cb := levels[ri], levels[gi], levels[bi]
	dist := func(x, y, z int) int {
		return (int(r)-x)*(int(r)-x) + (int(g)-y)*(int(g)-y) + (int(b)-z)*(int(b)-z)
	}
	cube := 16 + 36*ri + 6*gi + bi
	avg := (int(r) + int(g) + int(b)) / 3
	gi2 := min(max((avg-8+5)/10, 0), 23)
	gv := 8 + 10*gi2
	if dist(gv, gv, gv) < dist(cr, cg, cb) {
		return 232 + gi2
	}
	return cube
}

// ---------------------------------------------------------------- kitty

func kittyImage(img *image.RGBA) string {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if enc.Encode(&buf, img) != nil {
		return ""
	}
	data := base64.StdEncoding.EncodeToString(buf.Bytes())
	var sb strings.Builder
	const chunk = 4096
	for i := 0; i < len(data); i += chunk {
		end := min(i+chunk, len(data))
		more := 0
		if end < len(data) {
			more = 1
		}
		if i == 0 {
			// q=2: no replies; C=1: leave the cursor where it is
			fmt.Fprintf(&sb, "\x1b_Ga=T,f=100,q=2,C=1,m=%d;", more)
		} else {
			fmt.Fprintf(&sb, "\x1b_Gm=%d;", more)
		}
		sb.WriteString(data[i:end])
		sb.WriteString("\x1b\\")
	}
	return sb.String()
}

// ---------------------------------------------------------------- sixel

var sixelPalette = func() color.Palette {
	var p color.Palette
	levels := []uint8{0, 51, 102, 153, 204, 255}
	for _, r := range levels {
		for _, g := range levels {
			for _, b := range levels {
				p = append(p, color.RGBA{r, g, b, 255})
			}
		}
	}
	for i := 1; i <= 39; i++ { // grays between the cube's steps
		v := uint8(i * 255 / 40)
		p = append(p, color.RGBA{v, v, v, 255})
	}
	return p
}()

func sixelImage(img *image.RGBA) string {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	// flatten transparency onto nothing: transparent pixels stay unpainted
	opaque := image.NewRGBA(b)
	draw.Draw(opaque, b, image.NewUniform(color.Black), image.Point{}, draw.Src)
	draw.Draw(opaque, b, img, b.Min, draw.Over)
	pal := image.NewPaletted(b, sixelPalette)
	draw.FloydSteinberg.Draw(pal, b, opaque, b.Min)

	var sb strings.Builder
	sb.WriteString("\x1bP0;1;0q") // P2=1: unpainted pixels stay transparent
	fmt.Fprintf(&sb, "\"1;1;%d;%d", w, h)
	used := make([]bool, len(sixelPalette))
	for _, c := range pal.Pix {
		used[c] = true
	}
	for i, c := range sixelPalette {
		if used[i] {
			r, g, bl, _ := c.RGBA()
			fmt.Fprintf(&sb, "#%d;2;%d;%d;%d", i, r*100/0xffff, g*100/0xffff, bl*100/0xffff)
		}
	}
	row := make([]byte, w)
	for y0 := 0; y0 < h; y0 += 6 {
		inBand := map[uint8]bool{}
		for y := y0; y < min(y0+6, h); y++ {
			for x := 0; x < w; x++ {
				if img.Pix[img.PixOffset(x, y)+3] >= alphaCut {
					inBand[pal.Pix[pal.PixOffset(x, y)]] = true
				}
			}
		}
		first := true
		for c := 0; c < len(sixelPalette); c++ {
			if !inBand[uint8(c)] {
				continue
			}
			if !first {
				sb.WriteByte('$') // back to the start of this band
			}
			first = false
			for x := 0; x < w; x++ {
				var bits byte
				for k := 0; k < 6 && y0+k < h; k++ {
					y := y0 + k
					if pal.Pix[pal.PixOffset(x, y)] == uint8(c) && img.Pix[img.PixOffset(x, y)+3] >= alphaCut {
						bits |= 1 << k
					}
				}
				row[x] = 63 + bits
			}
			fmt.Fprintf(&sb, "#%d", c)
			for x := 0; x < w; {
				n := 1
				for x+n < w && row[x+n] == row[x] {
					n++
				}
				if n > 3 {
					fmt.Fprintf(&sb, "!%d%c", n, row[x])
				} else {
					sb.Write(bytes.Repeat([]byte{row[x]}, n))
				}
				x += n
			}
		}
		sb.WriteByte('-')
	}
	sb.WriteString("\x1b\\")
	return sb.String()
}

// alphaCut: pixels less opaque than this are left as terminal background,
// so icons and cut-outs keep their shape while soft shadows still show.
const alphaCut = 48
