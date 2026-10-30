package main

// tree.go — the -T space map: a TreeSize-style ranked tree with bars.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type treeRender struct {
	o     *Options
	p     *Painter
	total int64
	barW  int
	sizeW int
	width int
	noPct bool // very narrow terminals drop the SHARE column
}

// fixed is the width of everything left of the tree branches.
func (t *treeRender) fixed() int {
	w := t.p.gutterW() + t.sizeW + 2
	if !t.noPct {
		w += 6 + 2
	}
	if t.barW > 0 {
		w += t.barW + 2
	}
	return w
}

// line prints one tree row, truncating the name (middle-out) and any note
// so the row never wraps.
func (t *treeRender) line(size []span, pct []span, frac float64, kind Kind, branch, name string, nameCode string, note []span) {
	p := t.p
	var out []span
	if p.on {
		c := kindCode(kind)
		if c == "" {
			c = "2"
		}
		out = append(out, span{" ", ""}, span{"▌", c}, span{" ", ""})
	} else {
		out = append(out, span{" ", ""})
	}
	out = append(out, padSpans(size, t.sizeW, true)...)
	out = append(out, span{"  ", ""})
	if !t.noPct {
		out = append(out, padSpans(pct, 6, true)...)
		out = append(out, span{"  ", ""})
	}
	if t.barW > 0 {
		out = append(out, t.bar(frac)...)
		out = append(out, span{"  ", ""})
	}
	out = append(out, span{branch, "2"})
	room := 0
	if t.width > 0 {
		room = t.width - t.fixed() - dispWidth(branch)
		if room < 6 {
			room = 6
		}
	}
	if room > 0 {
		name = truncMiddle(name, room)
	}
	out = append(out, span{name, nameCode})
	if len(note) > 0 {
		left := room - dispWidth(name) - 2
		nw := spansWidth(note)
		if room == 0 || nw <= left {
			out = append(out, span{"  ", ""})
			out = append(out, note...)
		} else if left >= 8 {
			flat := ""
			for _, s := range note {
				flat += s.t
			}
			out = append(out, span{"  ", ""}, span{truncMiddle(flat, left), note[0].c})
		}
	}
	fmt.Println(p.render(out))
}

func treeView(path string, o *Options, p *Painter) error {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return &os.PathError{Op: "stat", Path: path, Err: err}
	}
	abs, _ := filepath.Abs(path)
	sc := NewScanner(o.Disk, o.OneFS)

	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		fmt.Printf("%s  %s\n", p.AmberB(safeName(abs)), p.Bold(fmtSize(sc.rawSize(&st), o.Units)))
		return nil
	}

	stop := progress(sc)
	root := sc.scanDir(path, &st, uint64(st.Dev), o.Depth, o.Top)
	stop()
	if root.Denied {
		return &os.PathError{Op: "open", Path: path, Err: syscall.EACCES}
	}
	root.Name = abs

	width := outWidth(o)
	gw := p.gutterW()
	t := &treeRender{o: o, p: p, total: root.Size, sizeW: 9, width: width}
	if w := vlen(fmtSize(root.Size, o.Units)) + 1; w > t.sizeW {
		t.sizeW = w
	}
	switch {
	case width == 0 || width >= 110:
		t.barW = 24
	case width >= 90:
		t.barW = 20
	case width >= 70:
		t.barW = 12
	case width >= 56:
		t.barW = 8
	}

	// title block, same shape as the listing's
	for j, l := range wrapSpans([]span{{safeName(abs), "1;" + p.amber}}, width-gw) {
		lead := []span{{strings.Repeat(" ", gw), ""}}
		if j == 0 && p.on {
			lead = []span{{" ", ""}, {"▌", p.amber}, {" ", ""}}
		}
		fmt.Println(p.render(append(lead, l...)))
	}
	sub := []span{{fmtSize(root.Size, o.Units), "1"}, {" used", ""}, {"\u00a0\u00a0·  ", "2"},
		{commas(root.Items), "1"}, {nb(" items inside"), ""}}
	for _, l := range wrapSpans(sub, width-gw) {
		fmt.Println(p.render(append([]span{{strings.Repeat(" ", gw), ""}}, l...)))
	}
	fmt.Println()

	t.noPct = width > 0 && width < 46
	h := fmt.Sprintf("%s%*s  ", strings.Repeat(" ", gw), t.sizeW, "SIZE")
	if !t.noPct {
		h += fmt.Sprintf("%6s  ", "SHARE")
	}
	if t.barW > 0 {
		h += fmt.Sprintf("%-*s  ", t.barW, "")
	}
	fmt.Println(p.render([]span{{h, "2;1"}, {"NAME", "2;1"}}))
	ruleW := t.fixed() + 32
	if width > 0 {
		ruleW = width - gw
	}
	rule(gw, ruleW, p)

	root.Name = filepath.Base(abs)
	if abs == "/" {
		root.Name = "/"
	}
	t.row(root, root.Size, "", "", 0)

	rule(gw, ruleW, p)

	var foot [][]span
	foot = append(foot, []span{{"SHARE", "2;1"}, {fmt.Sprintf(" is each item's part of the folder it sits in · items under %g%% of the total are folded into \"smaller items\"", o.MinPct), "2"}})
	if d := sc.denied.Load(); d > 0 {
		foot = append(foot, []span{{fmt.Sprintf("⚠ %s folder(s) couldn't be read, so sizes marked ≥ are minimums.", commas(d)), p.amber}, {" Run with sudo for complete numbers.", "2"}})
	}
	if s := sc.skipped.Load(); s > 0 {
		foot = append(foot, []span{{fmt.Sprintf("%d mounted filesystem(s) inside were not counted.", s), "2"}})
	}
	for _, f := range foot {
		for _, l := range wrapSpans(f, width-gw) {
			fmt.Println(p.render(append([]span{{strings.Repeat(" ", gw), ""}}, l...)))
		}
	}
	return nil
}

func (t *treeRender) row(n *Node, parent int64, prefix, branch string, depth int) {
	frac := 0.0
	if parent > 0 {
		frac = float64(n.Size) / float64(parent)
	}

	size := fmtSize(n.Size, t.o.Units)
	if n.Denied {
		size = "?"
	} else if n.Partial {
		size = "≥" + strings.TrimPrefix(size, "<")
	}
	pct := fmt.Sprintf("%5.1f%%", frac*100)

	name := safeName(n.Name)
	switch n.Kind {
	case KFolder:
		if !strings.HasSuffix(name, "/") {
			name += "/"
		}
	case KProgram:
		name += "*"
	}
	branchText := prefix + branch
	if n.Skip != "" {
		t.line([]span{{"—", "2"}}, nil, -1, n.Kind, branchText, name, kindCode(n.Kind), []span{{"(" + n.Skip + ")", "2"}})
		return
	}
	var note []span
	if n.Denied {
		note = []span{{"(you can't open this folder)", t.p.amber}}
	}
	t.line([]span{{size, t.p.sizeCode(n.Size)}}, []span{{pct, "2"}}, frac, n.Kind, branchText, name, kindCode(n.Kind), note)

	if depth >= t.o.Depth {
		return
	}
	minSize := int64(math.Ceil(t.o.MinPct / 100 * float64(t.total)))
	var show []*Node
	restN, restSize := n.RestN, n.RestSize
	for _, c := range n.Children {
		if c.Skip != "" || (c.Size >= minSize && c.Size > 0) {
			show = append(show, c)
		} else {
			restN++
			restSize += c.Size
		}
	}
	childPrefix := prefix
	if depth > 0 {
		if branch == "└─ " {
			childPrefix += "   "
		} else {
			childPrefix += "│  "
		}
	}
	for i, c := range show {
		b := "├─ "
		if i == len(show)-1 && restN == 0 {
			b = "└─ "
		}
		t.row(c, n.Size, childPrefix, b, depth+1)
	}
	if restN > 0 {
		f := 0.0
		if n.Size > 0 {
			f = float64(restSize) / float64(n.Size)
		}
		s := fmtSize(restSize, t.o.Units)
		label := fmt.Sprintf("… %s smaller item", commas(restN))
		if restN != 1 {
			label += "s"
		}
		t.line([]span{{s, "2"}}, []span{{fmt.Sprintf("%5.1f%%", f*100), "2"}}, f, KOther, childPrefix+"└─ ", label, "2", nil)
	}
}

var eighths = []string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}

func (t *treeRender) bar(frac float64) []span {
	if frac < 0 {
		return []span{{strings.Repeat(" ", t.barW), ""}}
	}
	if frac > 1 {
		frac = 1
	}
	units := int(math.Round(frac * float64(t.barW*8)))
	if units == 0 && frac > 0 {
		units = 1 // never let a real item look like nothing
	}
	full, rem := units/8, units%8
	fill := strings.Repeat("█", full) + eighths[rem]
	used := full
	if rem > 0 {
		used++
	}
	return []span{{fill, t.p.amber}, {strings.Repeat("░", t.barW-used), "2"}}
}
