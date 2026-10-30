package main

// render.go — styled text spans, true display width, and wrapping.
//
// Everything that can wrap is kept as spans (text + SGR code) until the
// last moment, so a line can be broken at any character without cutting
// an escape sequence in half or losing its color.

import (
	"strings"
	"unicode"
)

type span struct {
	t string // text
	c string // SGR code, "" = plain
}

func (p *Painter) render(sp []span) string {
	var b strings.Builder
	for _, s := range sp {
		if s.c == "" || !p.on || s.t == "" {
			b.WriteString(s.t)
		} else {
			b.WriteString("\x1b[" + s.c + "m" + s.t + "\x1b[0m")
		}
	}
	return b.String()
}

func spansWidth(sp []span) int {
	w := 0
	for _, s := range sp {
		w += dispWidth(s.t)
	}
	return w
}

func padSpans(sp []span, width int, right bool) []span {
	gap := width - spansWidth(sp)
	if gap <= 0 {
		return sp
	}
	fill := span{t: strings.Repeat(" ", gap)}
	if right {
		return append([]span{fill}, sp...)
	}
	return append(append([]span{}, sp...), fill)
}

// ---------------------------------------------------------------- width

// Terminal cell widths: CJK and most emoji take two cells, combining
// marks take none. Getting this right keeps columns straight when names
// are in Japanese, Chinese, Korean or contain emoji.
var wideRanges = [][2]rune{
	{0x1100, 0x115F}, {0x231A, 0x231B}, {0x2329, 0x232A}, {0x23E9, 0x23EC},
	{0x23F0, 0x23F0}, {0x23F3, 0x23F3}, {0x25FD, 0x25FE}, {0x2614, 0x2615},
	{0x2648, 0x2653}, {0x267F, 0x267F}, {0x2693, 0x2693}, {0x26A1, 0x26A1},
	{0x26AA, 0x26AB}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5}, {0x26CE, 0x26CE},
	{0x26D4, 0x26D4}, {0x26EA, 0x26EA}, {0x26F2, 0x26F3}, {0x26F5, 0x26F5},
	{0x26FA, 0x26FA}, {0x26FD, 0x26FD}, {0x2705, 0x2705}, {0x270A, 0x270B},
	{0x2728, 0x2728}, {0x274C, 0x274C}, {0x274E, 0x274E}, {0x2753, 0x2755},
	{0x2757, 0x2757}, {0x2795, 0x2797}, {0x27B0, 0x27B0}, {0x27BF, 0x27BF},
	{0x2B1B, 0x2B1C}, {0x2B50, 0x2B50}, {0x2B55, 0x2B55}, {0x2E80, 0x303E},
	{0x3041, 0x33FF}, {0x3400, 0x4DBF}, {0x4E00, 0x9FFF}, {0xA000, 0xA4CF},
	{0xA960, 0xA97F}, {0xAC00, 0xD7A3}, {0xF900, 0xFAFF}, {0xFE10, 0xFE19},
	{0xFE30, 0xFE6F}, {0xFF00, 0xFF60}, {0xFFE0, 0xFFE6}, {0x16FE0, 0x18AFF},
	{0x1B000, 0x1B2FF}, {0x1F004, 0x1F004}, {0x1F0CF, 0x1F0CF}, {0x1F18E, 0x1F18E},
	{0x1F191, 0x1F19A}, {0x1F200, 0x1F2FF}, {0x1F300, 0x1F64F}, {0x1F680, 0x1F6FF},
	{0x1F7E0, 0x1F7EB}, {0x1F90C, 0x1F9FF}, {0x1FA70, 0x1FAFF}, {0x20000, 0x3FFFD},
}

func runeWidth(r rune) int {
	switch {
	case r < 0x300:
		return 1
	case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r),
		r == 0x200B, r == 0x200C, r == 0x200D, r >= 0xFE00 && r <= 0xFE0F:
		return 0
	}
	for _, rg := range wideRanges {
		if r < rg[0] {
			break
		}
		if r <= rg[1] {
			return 2
		}
	}
	return 1
}

func dispWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// ---------------------------------------------------------------- wrapping

type glyph struct {
	r rune
	c string
	w int
}

func toGlyphs(sp []span) []glyph {
	var g []glyph
	for _, s := range sp {
		for _, r := range s.t {
			g = append(g, glyph{r, s.c, runeWidth(r)})
		}
	}
	return g
}

func fromGlyphs(g []glyph) []span {
	var out []span
	var b strings.Builder
	cur := ""
	for i, x := range g {
		if i > 0 && x.c != cur {
			out = append(out, span{b.String(), cur})
			b.Reset()
		}
		cur = x.c
		b.WriteRune(x.r)
	}
	if b.Len() > 0 {
		out = append(out, span{b.String(), cur})
	}
	return out
}

// breakAfter marks characters a filename can sensibly be split after.
func breakAfter(r rune) bool {
	switch r {
	case ' ', '/', '-', '_', '.', ';', ')', ']', '+', '=', '·', '→':
		return true
	}
	return r >= 0x2E80 // CJK text can break between any two characters
}

// wrapSpans greedily fills lines of at most width cells, preferring to
// break at word or path boundaries and falling back to a hard break.
func wrapSpans(sp []span, width int) [][]span {
	if width <= 0 {
		return [][]span{sp}
	}
	g := toGlyphs(sp)
	var lines [][]span
	for len(g) > 0 {
		w, i := 0, 0
		for i < len(g) && w+g[i].w <= width {
			w += g[i].w
			i++
		}
		if i == len(g) {
			lines = append(lines, fromGlyphs(g))
			break
		}
		cut := i
		if g[i].r != ' ' {
			fallback := -1
			for j := i - 1; j > i/3; j-- {
				if !breakAfter(g[j].r) {
					continue
				}
				// avoid stranding a scrap like "zst" or "it" on the next line
				if len(g)-(j+1) < 5 {
					if fallback < 0 {
						fallback = j + 1
					}
					continue
				}
				cut = j + 1
				fallback = -1
				break
			}
			if fallback > 0 {
				cut = fallback
			}
		}
		if cut == 0 {
			cut = 1
		}
		line := g[:cut]
		for len(line) > 0 && line[len(line)-1].r == ' ' {
			line = line[:len(line)-1]
		}
		lines = append(lines, fromGlyphs(line))
		g = g[cut:]
		for len(g) > 0 && g[0].r == ' ' {
			g = g[1:]
		}
	}
	if len(lines) == 0 {
		lines = [][]span{nil}
	}
	return lines
}

// truncMiddle shortens s to width cells as "start…end", keeping the end
// because that's where extensions and version numbers live.
func truncMiddle(s string, width int) string {
	if width <= 0 || dispWidth(s) <= width {
		return s
	}
	if width <= 1 {
		return "…"
	}
	rs := []rune(s)
	keepEnd := (width - 1) / 3
	keepStart := width - 1 - keepEnd
	var a, z []rune
	w := 0
	for _, r := range rs {
		if w+runeWidth(r) > keepStart {
			break
		}
		w += runeWidth(r)
		a = append(a, r)
	}
	w = 0
	for i := len(rs) - 1; i >= 0; i-- {
		if w+runeWidth(rs[i]) > keepEnd {
			break
		}
		w += runeWidth(rs[i])
		z = append([]rune{rs[i]}, z...)
	}
	return string(a) + "…" + string(z)
}
