package main

// term.go — terminal detection, colors, size/number formatting, safe names.

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
	"unsafe"
)

type winsize struct{ Row, Col, X, Y uint16 }

func isTTY(f *os.File) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return e == 0
}

func termWidth() int {
	var ws winsize
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if e == 0 && ws.Col > 0 {
		return int(ws.Col)
	}
	if c, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && c > 0 {
		return c
	}
	return 0 // unknown (piped): don't constrain
}

// ---------------------------------------------------------------- colors

type Painter struct {
	on    bool
	amber string
}

func NewPainter(mode string) *Painter {
	on := false
	switch mode {
	case "always":
		on = true
	case "never":
		on = false
	default:
		on = isTTY(os.Stdout) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	}
	amber := "38;5;179"
	if ct := os.Getenv("COLORTERM"); ct == "truecolor" || ct == "24bit" {
		amber = "38;2;232;163;61" // #e8a33d
	}
	return &Painter{on: on, amber: amber}
}

func (p *Painter) wrap(code, s string) string {
	if !p.on || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p *Painter) Dim(s string) string    { return p.wrap("2", s) }
func (p *Painter) Bold(s string) string   { return p.wrap("1", s) }
func (p *Painter) Amber(s string) string  { return p.wrap(p.amber, s) }
func (p *Painter) AmberB(s string) string { return p.wrap("1;"+p.amber, s) }
func (p *Painter) Warn(s string) string   { return p.wrap("1;31", s) }

// Kind colors follow the long-standing ls conventions, so they look right
// in any terminal theme: folders blue, programs green, links cyan.
func kindCode(k Kind) string {
	switch k {
	case KFolder:
		return "1;34"
	case KProgram:
		return "1;32"
	case KLink:
		return "36"
	case KBrokenLink:
		return "1;31"
	case KDevice:
		return "33"
	case KPipe, KSocket:
		return "35"
	}
	return ""
}

func (p *Painter) Kind(k Kind, s string) string { return p.wrap(kindCode(k), s) }

// sizeCode makes the big numbers jump out.
func (p *Painter) sizeCode(b int64) string {
	switch {
	case b >= 1<<30:
		return "1;" + p.amber
	case b >= 100<<20:
		return p.amber
	case b < 1<<20:
		return "2"
	}
	return ""
}

// SizeHeat makes the big numbers jump out.
func (p *Painter) SizeHeat(b int64, s string) string { return p.wrap(p.sizeCode(b), s) }

// ---------------------------------------------------------------- numbers

func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func fmtFloat(f float64, dec int) string {
	s := strconv.FormatFloat(f, 'f', dec, 64)
	whole, frac, _ := strings.Cut(s, ".")
	w, _ := strconv.ParseInt(whole, 10, 64)
	out := commas(w)
	if frac != "" {
		out += "." + frac
	}
	return out
}

const (
	kib = 1024.0
	mib = kib * 1024
	gib = mib * 1024
	tib = gib * 1024
)

func fmtSize(b int64, units string) string {
	f := float64(b)
	switch units {
	case "bytes":
		return commas(b) + " B"
	case "kb":
		return fmtFloat(f/kib, 1) + " KB"
	case "mb":
		return fmtFloat(f/mib, 2) + " MB"
	case "auto":
		switch {
		case b < 1024:
			return strconv.FormatInt(b, 10) + " B"
		case f < mib:
			return fmtFloat(f/kib, 1) + " KB"
		case f < gib:
			return fmtFloat(f/mib, 1) + " MB"
		case f < tib:
			return fmtFloat(f/gib, 2) + " GB"
		default:
			return fmtFloat(f/tib, 2) + " TB"
		}
	default: // gb
		if b == 0 {
			return "0 GB"
		}
		g := f / gib
		if g < 0.001 {
			return "<0.001 GB"
		}
		if g < 0.01 {
			return fmtFloat(g, 3) + " GB"
		}
		return fmtFloat(math.Floor(g*100+0.5)/100, 2) + " GB"
	}
}

// ---------------------------------------------------------------- names

// safeName escapes control characters so a hostile filename can't inject
// terminal escape sequences or fake extra lines.
func safeName(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f || s[i] >= 0x80 {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, "\\x%02x", s[i])
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			fmt.Fprintf(&b, "\\x%02x", r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

func vlen(s string) int { return dispWidth(s) }
