package main

// listview.go — rendering the listing so it fits whatever terminal it's in.
//
// The table degrades in steps as the terminal narrows, always keeping the
// plain-language permission words as long as possible:
//
//   1. full          LAST CHANGED with time, separate OWNER and GROUP
//   2. short date    2026-10-30
//   3. merged        OWNER:GROUP in one column
//   4. letters       permissions as r/w/x, with a legend under the table
//   5. no group      owner only
//   6. cards         one small block per item, for very narrow terminals
//
// Names and notes never push past the right edge: they wrap with a
// hanging indent so the left-hand columns stay a clean block.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	minNameW = 18 // narrowest NAME column worth having before going to cards
	gap      = "  "
)

type layout struct {
	shortDate, merged, letters, noGroup bool
}

var tiers = []layout{
	{},
	{shortDate: true},
	{shortDate: true, merged: true},
	{shortDate: true, noGroup: true},
	{shortDate: true, merged: true, letters: true},
	{shortDate: true, noGroup: true, letters: true},
}

// row is one entry, pre-digested into the pieces every layout draws from.
type row struct {
	e        *Entry
	kindWord string
	size     []span
	date     time.Time
	dateCode string
	keepTime bool // sorting by time: the short date keeps hours and minutes
	owner    string
	group    string
	name     []span
	notes    [][]span
}

// ---------------------------------------------------------------- notes

func noteSpans(e *Entry, p *Painter) [][]span {
	var out [][]span
	add := func(s string) { out = append(out, []span{{s, "2"}}) }
	warn := func(s string) { out = append(out, []span{{s, p.amber}}) }

	switch e.Kind {
	case KDevice:
		ma, mi := devNums(e.Rdev)
		t := "character device"
		if e.Mode&syscall.S_IFMT == syscall.S_IFBLK {
			t = "block (disk) device"
		}
		add(fmt.Sprintf("%s %d,%d", t, ma, mi))
	case KBrokenLink:
		warn("target is missing")
	}
	if e.Kind != KLink && e.Kind != KBrokenLink {
		isDir := e.Kind == KFolder
		if e.Mode&syscall.S_ISUID != 0 && !isDir {
			warn("runs as " + userName(e.UID) + " no matter who starts it")
		}
		if e.Mode&syscall.S_ISGID != 0 {
			if isDir {
				add("new items inside get group " + groupName(e.GID))
			} else {
				warn("runs with group " + groupName(e.GID))
			}
		}
		if e.Mode&syscall.S_ISVTX != 0 && isDir {
			add("only each item's owner can delete it")
		}
		if e.Mode&0o002 != 0 && !(isDir && e.Mode&syscall.S_ISVTX != 0) {
			warn("anyone can change this")
		}
		if !isDir && e.Nlink > 1 {
			add(fmt.Sprintf("%d hard links share this data", e.Nlink))
		}
		if hasACL(e.Path) {
			add("has extra (ACL) permissions — see getfacl")
		}
	}
	switch {
	case e.Denied:
		warn("you can't open this folder")
	case e.Partial:
		add("some contents unreadable — size is a minimum")
	}
	if (e.Kind == KFile || e.Kind == KProgram) && syscall.Access(e.Path, 4) != nil {
		warn("you can't read this")
	}
	if e.Skip != "" {
		add(e.Skip)
	}
	return out
}

// ---------------------------------------------------------------- cells

func nameSpans(e *Entry) []span {
	name := safeName(e.Name)
	kc := kindCode(e.Kind)
	switch e.Kind {
	case KFolder:
		return []span{{name + "/", kc}}
	case KProgram:
		return []span{{name + "*", kc}}
	case KLink, KBrokenLink:
		t := safeName(e.Target)
		tc := kindCode(e.TargetKind)
		if e.Kind == KBrokenLink {
			tc = kc
		} else if e.TargetKind == KFolder {
			t += "/"
		}
		return []span{{name, kc}, {" → ", "2"}, {t, tc}}
	}
	return []span{{name, kc}}
}

func makeRow(e *Entry, o *Options, p *Painter) *row {
	r := &row{e: e, kindWord: kindWord[e.Kind], date: e.MTime, keepTime: o.Sort == "time",
		owner: safeName(userName(e.UID)), group: safeName(groupName(e.GID))}
	switch {
	case e.Denied:
		r.size = []span{{"?", p.amber}}
	case e.Sized:
		s := fmtSize(e.Size, o.Units)
		if e.Partial {
			s = "≥" + strings.TrimPrefix(s, "<")
		}
		r.size = []span{{s, p.sizeCode(e.Size)}}
	default:
		r.size = []span{{"—", "2"}}
	}
	if time.Since(e.MTime) < 24*time.Hour {
		r.dateCode = p.amber
	}
	r.name = nameSpans(e)
	r.notes = noteSpans(e, p)
	return r
}

func (r *row) dateText(short bool) string {
	if short && r.keepTime {
		// ls-style: time for this year, year for older items
		if r.date.Year() == time.Now().Year() {
			return r.date.Format("Jan _2 15:04")
		}
		return r.date.Format("Jan _2  2006")
	}
	if short {
		return r.date.Format("2006-01-02")
	}
	return r.date.Format("2006-01-02 15:04")
}

func (r *row) ownerText(l layout) string {
	switch {
	case l.noGroup:
		return r.owner
	case l.merged:
		return r.owner + ":" + r.group
	}
	return r.owner
}

// permSpans: words always sit in the same slot so they scan down the
// column. Everyone-can-write is flagged in amber.
func permSpans(bits uint32, isDir, letters, everyone bool, p *Painter) []span {
	x := "run"
	if isDir {
		x = "enter"
	}
	words := []string{"read", "write", x}
	slotW := []int{4, 5, 5}
	code := func(i int) string {
		if everyone && i == 1 {
			return "1;" + p.amber
		}
		return ""
	}
	var out []span
	if letters {
		for i := 0; i < 3; i++ {
			if bits&(4>>i) != 0 {
				c := code(i)
				if c == "" {
					c = "1"
				}
				out = append(out, span{string("rwx"[i]), c})
			} else {
				out = append(out, span{"-", "2"})
			}
		}
		return out
	}
	if bits&7 == 0 {
		return []span{{"nothing", "2"}}
	}
	for i := 0; i < 3; i++ {
		if i > 0 {
			out = append(out, span{" ", ""})
		}
		if bits&(4>>i) != 0 {
			out = append(out, span{words[i], code(i)}, span{strings.Repeat(" ", slotW[i]-len(words[i])), ""})
		} else {
			out = append(out, span{"·", "2"}, span{strings.Repeat(" ", slotW[i]-1), ""})
		}
	}
	// drop trailing padding; the column pads anyway
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1].t) == "" {
		out = out[:len(out)-1]
	}
	return out
}

func permTriple(e *Entry, letters bool, p *Painter) [3][]span {
	if !e.PermValid {
		na := []span{{"n/a", "2"}}
		return [3][]span{na, na, na}
	}
	isDir := e.PermMode&syscall.S_IFMT == syscall.S_IFDIR
	return [3][]span{
		permSpans(e.PermMode>>6&7, isDir, letters, false, p),
		permSpans(e.PermMode>>3&7, isDir, letters, false, p),
		permSpans(e.PermMode&7, isDir, letters, true, p),
	}
}

// ---------------------------------------------------------------- table

type column struct {
	head  string
	right bool
	cells [][]span
	w     int
}

func buildColumns(rows []*row, l layout, p *Painter) []*column {
	kind := &column{head: "KIND"}
	size := &column{head: "SIZE", right: true}
	date := &column{head: "LAST CHANGED"}
	if l.shortDate {
		date.head = "CHANGED"
	}
	owner := &column{head: "OWNER"}
	if l.merged && !l.noGroup {
		owner.head = "OWNER:GROUP"
	}
	group := &column{head: "GROUP"}
	ph := []string{"OWNER CAN", "GROUP CAN", "EVERYONE CAN"}
	if l.letters {
		ph = []string{"OWN", "GRP", "ALL"}
	}
	perms := []*column{{head: ph[0]}, {head: ph[1]}, {head: ph[2]}}

	for _, r := range rows {
		kind.cells = append(kind.cells, []span{{r.kindWord, kindCode(r.e.Kind)}})
		size.cells = append(size.cells, r.size)
		date.cells = append(date.cells, []span{{r.dateText(l.shortDate), r.dateCode}})
		owner.cells = append(owner.cells, []span{{r.ownerText(l), ""}})
		group.cells = append(group.cells, []span{{r.group, ""}})
		pt := permTriple(r.e, l.letters, p)
		for i := range perms {
			perms[i].cells = append(perms[i].cells, pt[i])
		}
	}
	cols := []*column{kind, size, date, owner}
	if !l.merged && !l.noGroup {
		cols = append(cols, group)
	}
	if l.noGroup {
		owner.head = "OWNER"
	}
	cols = append(cols, perms...)
	for _, c := range cols {
		c.w = dispWidth(c.head)
		for _, cell := range c.cells {
			if w := spansWidth(cell); w > c.w {
				c.w = w
			}
		}
	}
	return cols
}

func fixedWidth(cols []*column) int {
	w := 0
	for _, c := range cols {
		w += c.w + len(gap)
	}
	return w
}

// gutter is the colored edge that marks each row's kind at a glance.
func (p *Painter) gutter(k Kind, first bool) []span {
	if !p.on {
		return []span{{" ", ""}}
	}
	if !first {
		return []span{{"   ", ""}}
	}
	c := kindCode(k)
	if c == "" {
		c = "2"
	}
	return []span{{" ", ""}, {"▌", c}, {" ", ""}}
}

func (p *Painter) gutterW() int {
	if p.on {
		return 3
	}
	return 1
}

func printTable(rows []*row, cols []*column, nameW int, p *Painter) {
	gw := p.gutterW()
	fixed := fixedWidth(cols)
	indent := strings.Repeat(" ", gw+fixed)

	// widest name line actually used, for the rules
	used := dispWidth("NAME")
	type laidOut struct{ lines [][]span }
	outs := make([]laidOut, len(rows))
	for i, r := range rows {
		outs[i].lines = layoutName(r, nameW)
		for _, l := range outs[i].lines {
			if w := spansWidth(l); w > used {
				used = w
			}
		}
	}
	total := fixed + used

	// header
	var h []span
	h = append(h, span{strings.Repeat(" ", gw), ""})
	for _, c := range cols {
		h = append(h, padSpans([]span{{c.head, "2;1"}}, c.w, c.right)...)
		h = append(h, span{gap, ""})
	}
	h = append(h, span{"NAME", "2;1"})
	fmt.Println(p.render(h))
	rule(gw, total, p)

	for i, r := range rows {
		var line []span
		line = append(line, p.gutter(r.e.Kind, true)...)
		for _, c := range cols {
			line = append(line, padSpans(c.cells[i], c.w, c.right)...)
			line = append(line, span{gap, ""})
		}
		lines := outs[i].lines
		line = append(line, lines[0]...)
		fmt.Println(strings.TrimRight(p.render(line), " "))
		for _, l := range lines[1:] {
			fmt.Println(indent + p.render(l))
		}
	}
	rule(gw, total, p)
}

// layoutName wraps the name and fits notes either inline (when they fit)
// or as their own indented lines underneath.
func layoutName(r *row, width int) [][]span {
	lines := wrapSpans(r.name, width)
	if len(r.notes) == 0 {
		return lines
	}
	var inline []span
	inline = append(inline, span{"  (", "2"})
	for i, n := range r.notes {
		if i > 0 {
			inline = append(inline, span{"; ", "2"})
		}
		inline = append(inline, n...)
	}
	inline = append(inline, span{")", "2"})

	last := len(lines) - 1
	if width <= 0 || spansWidth(lines[last])+spansWidth(inline) <= width {
		lines[last] = append(append([]span{}, lines[last]...), inline...)
		return lines
	}
	for _, n := range r.notes {
		for j, l := range wrapSpans(n, width-2) {
			lead := span{"  ", ""}
			if j == 0 {
				lead = span{"↳ ", "2"}
			}
			lines = append(lines, append([]span{lead}, l...))
		}
	}
	return lines
}

func rule(indent, width int, p *Painter) {
	fmt.Println(strings.Repeat(" ", indent) + p.Dim(strings.Repeat("─", width)))
}

// ---------------------------------------------------------------- cards

func printCards(rows []*row, width int, p *Painter) {
	gw := p.gutterW()
	inner := width - gw
	if inner < 16 {
		inner = 16
	}
	for i, r := range rows {
		if i > 0 {
			fmt.Println()
		}
		var block [][]span

		// name ··· size
		sw := spansWidth(r.size)
		nameLines := wrapSpans(r.name, inner-sw-2)
		first := nameLines[0]
		pad := inner - spansWidth(first) - sw
		if pad < 2 {
			pad = 2
		}
		block = append(block, append(append(append([]span{}, first...), span{strings.Repeat(" ", pad), ""}), r.size...))
		block = append(block, nameLines[1:]...)

		// kind · changed · owner
		meta := []span{
			{nb(r.kindWord), kindCode(r.e.Kind)}, {"\u00a0· ", "2"}, {nb("changed "), "2"},
			{nb(r.dateText(false)), r.dateCode}, {"\u00a0· ", "2"},
			{nb(r.owner), ""}, {":", "2"}, {nb(r.group), ""},
		}
		block = append(block, wrapSpans(meta, inner)...)

		// who can do what
		if r.e.PermValid {
			isDir := r.e.PermMode&syscall.S_IFMT == syscall.S_IFDIR
			var perm []span
			for j, who := range []string{"owner ", "group ", "everyone "} {
				if j > 0 {
					perm = append(perm, span{"\u00a0· ", "2"})
				}
				perm = append(perm, span{nb(who), "2"})
				for _, ps := range cardPerm(r.e.PermMode>>(6-3*j)&7, isDir, j == 2, p) {
					perm = append(perm, span{nb(ps.t), ps.c})
				}
			}
			block = append(block, wrapSpans(perm, inner)...)
		}
		for _, n := range r.notes {
			for j, l := range wrapSpans(n, inner-2) {
				lead := span{"  ", ""}
				if j == 0 {
					lead = span{"↳ ", "2"}
				}
				block = append(block, append([]span{lead}, l...))
			}
		}
		for _, l := range block {
			fmt.Println(strings.TrimRight(p.render(append(p.gutter(r.e.Kind, true), l...)), " "))
		}
	}
}

// nb glues words together so wrapping only happens at the " · " separators.
func nb(s string) string { return strings.ReplaceAll(s, " ", "\u00a0") }

// cardPerm lists only the rights that are present, e.g. "read enter".
func cardPerm(bits uint32, isDir, everyone bool, p *Painter) []span {
	if bits&7 == 0 {
		return []span{{"nothing", "2"}}
	}
	x := "run"
	if isDir {
		x = "enter"
	}
	var out []span
	for i, w := range []string{"read", "write", x} {
		if bits&(4>>i) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, span{" ", ""})
		}
		c := ""
		if everyone && i == 1 {
			c = "1;" + p.amber
		}
		out = append(out, span{w, c})
	}
	return out
}

// ---------------------------------------------------------------- view

func outWidth(o *Options) int {
	if o.Width > 0 {
		return o.Width
	}
	if isTTY(os.Stdout) {
		return termWidth()
	}
	return 0 // piped and no --width: one line per item, never wrapped
}

func listView(path string, o *Options, p *Painter) error {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return &os.PathError{Op: "stat", Path: path, Err: err}
	}
	sc := NewScanner(o.Disk, o.OneFS)

	var entries []*Entry
	hidden := 0
	isDir := st.Mode&syscall.S_IFMT == syscall.S_IFDIR
	if isDir {
		des, err := os.ReadDir(path)
		if err != nil && len(des) == 0 {
			return err
		}
		for _, de := range des {
			if !o.All && strings.HasPrefix(de.Name(), ".") {
				hidden++
				continue
			}
			cp := filepath.Join(path, de.Name())
			var cst syscall.Stat_t
			if syscall.Lstat(cp, &cst) != nil {
				continue
			}
			entries = append(entries, newEntry(cp, de.Name(), &cst, o.Disk))
		}
	} else {
		var lst syscall.Stat_t
		if err := syscall.Lstat(path, &lst); err != nil {
			return err
		}
		entries = append(entries, newEntry(path, path, &lst, o.Disk))
	}

	if !o.NoSize {
		stop := progress(sc)
		var wg sync.WaitGroup
		dev := uint64(st.Dev)
		for _, e := range entries {
			if e.Kind != KFolder {
				continue
			}
			if why := sc.skipMount(e.Path, &e.st, dev, dev); why != "" {
				e.Skip = why
				continue
			}
			wg.Add(1)
			go func(e *Entry) {
				defer wg.Done()
				sc.sem <- struct{}{}
				n := sc.scanDir(e.Path, &e.st, dev, 0, 0)
				<-sc.sem
				e.Size, e.Sized, e.Partial, e.Denied = n.Size, true, n.Partial, n.Denied
			}(e)
		}
		wg.Wait()
		stop()
	}
	sortEntries(entries, o)

	width := outWidth(o)
	gw := p.gutterW()

	// ---- title block
	abs, _ := filepath.Abs(path)
	titleW := width - gw
	for j, l := range wrapSpans([]span{{safeName(abs), "1;" + p.amber}}, titleW) {
		lead := []span{{strings.Repeat(" ", gw), ""}}
		if j == 0 && p.on {
			lead = []span{{" ", ""}, {"▌", p.amber}, {" ", ""}}
		}
		fmt.Println(p.render(append(lead, l...)))
	}
	if isDir {
		var total int64
		for _, e := range entries {
			if e.Sized {
				total += e.Size
			}
		}
		sub := []span{{commas(int64(len(entries))), "1"}, {" items", ""}}
		if !o.NoSize {
			sub = append(sub, span{"\u00a0\u00a0·  ", "2"}, span{fmtSize(total, o.Units), "1"}, span{nb(" total"), ""})
		}
		if hidden > 0 {
			sub = append(sub, span{"\u00a0\u00a0·  ", "2"}, span{nb(fmt.Sprintf("%d hidden — show with -a", hidden)), "2"})
		}
		for _, l := range wrapSpans(sub, titleW) {
			fmt.Println(p.render(append([]span{{strings.Repeat(" ", gw), ""}}, l...)))
		}
	}
	fmt.Println()
	if len(entries) == 0 {
		fmt.Println(strings.Repeat(" ", gw) + p.Dim("(empty)"))
		return nil
	}

	rows := make([]*row, len(entries))
	for i, e := range entries {
		rows[i] = makeRow(e, o, p)
	}

	// ---- pick the richest layout that fits
	var chosen *layout
	var cols []*column
	for i := range tiers {
		l := tiers[i]
		if o.Wide && l.letters {
			continue
		}
		c := buildColumns(rows, l, p)
		if width == 0 || gw+fixedWidth(c)+minNameW <= width {
			chosen, cols = &l, c
			break
		}
	}

	if chosen == nil {
		printCards(rows, width, p)
		fmt.Println()
		rule(gw, width-gw, p)
	} else {
		nameW := 0
		if width > 0 {
			nameW = width - gw - fixedWidth(cols)
		}
		printTable(rows, cols, nameW, p)
	}

	// ---- footer
	foot := summarySpans(entries)
	for _, l := range wrapSpans(foot, width-gw) {
		fmt.Println(p.render(append([]span{{strings.Repeat(" ", gw), ""}}, l...)))
	}
	if chosen != nil && chosen.noGroup {
		for _, r := range rows {
			if r.group != r.owner {
				note := []span{{"group names hidden to fit this width — widen the terminal to see them", "2"}}
				for _, l := range wrapSpans(note, width-gw) {
					fmt.Println(p.render(append([]span{{strings.Repeat(" ", gw), ""}}, l...)))
				}
				break
			}
		}
	}
	if chosen != nil && chosen.letters {
		legend := []span{{"OWN / GRP / ALL", "2;1"}, {"  r read · w write · x run (files) or enter (folders) · --wide for words", "2"}}
		for _, l := range wrapSpans(legend, width-gw) {
			fmt.Println(p.render(append([]span{{strings.Repeat(" ", gw), ""}}, l...)))
		}
	}
	if d := sc.denied.Load(); d > 0 {
		warn := []span{{"⚠ ", p.amber}, {fmt.Sprintf("%s folder(s) inside couldn't be read, so sizes marked ≥ are minimums.", commas(d)), p.amber},
			{" Run with sudo for complete numbers.", "2"}}
		for _, l := range wrapSpans(warn, width-gw) {
			fmt.Println(p.render(append([]span{{strings.Repeat(" ", gw), ""}}, l...)))
		}
	}
	return nil
}

// summarySpans counts each kind: "4 folders · 6 files · 2 programs · 1 link".
func summarySpans(es []*Entry) []span {
	order := []Kind{KFolder, KFile, KProgram, KLink, KBrokenLink, KDevice, KPipe, KSocket, KOther}
	count := map[Kind]int{}
	for _, e := range es {
		count[e.Kind]++
	}
	var out []span
	for _, k := range order {
		n := count[k]
		if n == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, span{"\u00a0\u00a0·  ", "2"})
		}
		w := kindWord[k]
		if n != 1 {
			w += "s"
		}
		out = append(out, span{fmt.Sprintf("%d ", n), "1"}, span{w, kindCode(k)})
	}
	return out
}
