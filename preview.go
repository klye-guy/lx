package main

// preview.go — the thumbnail column in the default listing.
//
// Each picture, video, song (album art) or file manager–thumbnailed item
// gets a small picture to the left of its row; the row grows to the
// thumbnail's height. Previews are made in parallel before the table is
// drawn, since the table needs every row to size its columns.

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// thumbCell is one row's preview, already encoded for the terminal.
type thumbCell struct {
	mk     mediaKind
	cw, ch int      // cells the picture covers
	off    int      // cells from the left of the thumbnail column
	lines  []string // blocks mode: one string per row of cells
	esc    string   // kitty/sixel: the whole picture as one escape sequence
	icon   string   // shown instead when there's no picture
	info   mediaInfo
	noFF   bool // needed ffmpeg, which isn't installed
}

type thumbPlan struct {
	g          termGfx
	cols, rows int
}

// planThumbs decides whether the listing shows previews, and how.
func planThumbs(o *Options, p *Painter) *thumbPlan {
	if o.Long || o.NoThumbs {
		return nil
	}
	if !o.ForceThumbs && !isTTY(os.Stdout) {
		return nil // piped: keep it text
	}
	g := detectGfx(o.Graphics)
	if g.mode == gBlocks && !p.on {
		return nil // half-blocks are made of color; NO_COLOR / --color=never
	}
	cols := o.ThumbW
	rows := int(float64(cols*g.cellW)/float64(g.cellH) + 0.5)
	return &thumbPlan{g: g, cols: cols, rows: max(rows, 1)}
}

func previewCandidate(e *Entry) bool {
	switch e.Kind {
	case KFile, KProgram:
		return true
	case KLink:
		return e.TargetKind == KFile || e.TargetKind == KProgram
	}
	return false
}

// makeThumbs builds previews for every entry that can have one, with a
// progress line on stderr when it takes a while.
func makeThumbs(entries []*Entry, tp *thumbPlan) map[*Entry]*thumbCell {
	var todo []*Entry
	for _, e := range entries {
		if previewCandidate(e) {
			todo = append(todo, e)
		}
	}
	out := make(map[*Entry]*thumbCell, len(todo))
	if len(todo) == 0 {
		return out
	}
	var mu sync.Mutex
	var done atomic.Int64
	stop := countProgress("making previews", &done, int64(len(todo)))
	jobs := make(chan *Entry)
	var wg sync.WaitGroup
	for w := 0; w < min(runtime.NumCPU(), 8); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range jobs {
				if c := tp.thumbFor(e); c != nil {
					mu.Lock()
					out[e] = c
					mu.Unlock()
				}
				done.Add(1)
			}
		}()
	}
	for _, e := range todo {
		jobs <- e
	}
	close(jobs)
	wg.Wait()
	stop()
	return out
}

var mediaIcon = map[mediaKind]string{mImage: "▣", mVideo: "▶", mAudio: "♪", mOther: "▤"}

func (tp *thumbPlan) thumbFor(e *Entry) *thumbCell {
	fi, err := os.Stat(e.Path) // follows links: the target's time is what's cached
	if err != nil {
		return nil
	}
	mk := mediaKindOf(e.Name)
	if mk == mNone && e.Kind == KLink {
		mk = mediaKindOf(e.Target)
	}
	quiet := mk == mNone // not media by name: only a cached preview counts
	if quiet {
		mk = mOther
	}
	pbw, pbh := tp.g.pixelBox(tp.cols, tp.rows)
	img, info, err := findPreview(e.Path, mk, fi.ModTime(), pbw, pbh)
	if quiet && img == nil {
		return nil
	}
	c := &thumbCell{mk: mk, info: info}
	if img == nil {
		c.icon = mediaIcon[mk]
		c.noFF = errors.Is(err, errNoFFmpeg)
		return c
	}
	b := img.Bounds()
	pw, ph, cw, ch := tp.g.fitBox(b.Dx(), b.Dy(), tp.cols, tp.rows)
	if pw == 0 {
		c.icon = mediaIcon[mk]
		return c
	}
	small := scaleTo(img, pw, ph)
	c.cw, c.ch, c.off = cw, ch, (tp.cols-cw)/2
	switch tp.g.mode {
	case gBlocks:
		c.lines = tp.g.blockLines(small)
	case gKitty:
		c.esc = kittyImage(small)
	case gSixel:
		c.esc = sixelImage(small)
	}
	return c
}

// height is how many lines the row needs for this preview.
func (c *thumbCell) height() int {
	if c == nil {
		return 1
	}
	return max(c.ch, 1)
}

// line returns line k of the thumbnail column, exactly width cells wide.
func (c *thumbCell) line(k, width int, p *Painter) string {
	blank := strings.Repeat(" ", width)
	if c == nil {
		return blank
	}
	if c.icon != "" {
		if k != 0 {
			return blank
		}
		pad := (width - 1) / 2
		return strings.Repeat(" ", pad) + p.Dim(c.icon) + strings.Repeat(" ", width-pad-1)
	}
	if c.lines != nil && k < len(c.lines) {
		return strings.Repeat(" ", c.off) + c.lines[k] + strings.Repeat(" ", width-c.off-c.cw)
	}
	return blank // kitty/sixel draw over the blank area afterwards
}

// infoNote is the dim "1920×1080 · 4:32" after a media file's name.
func (c *thumbCell) infoNote() string {
	if c == nil {
		return ""
	}
	var parts []string
	if c.info.w > 0 && c.info.h > 0 && c.mk != mAudio {
		parts = append(parts, fmt.Sprintf("%d×%d", c.info.w, c.info.h))
	}
	if c.info.dur >= 1 && (c.mk == mVideo || c.mk == mAudio) {
		parts = append(parts, fmtDuration(c.info.dur))
	}
	return strings.Join(parts, " · ")
}

func fmtDuration(sec float64) string {
	s := int(sec + 0.5)
	h, m, ss := s/3600, s/60%60, s%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, ss)
	}
	return fmt.Sprintf("%d:%02d", m, ss)
}

// countProgress shows "label… n of total" on stderr for slow jobs.
func countProgress(label string, done *atomic.Int64, total int64) (stop func()) {
	if !isTTY(os.Stderr) {
		return func() {}
	}
	quit := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		t := time.NewTicker(120 * time.Millisecond)
		defer t.Stop()
		shown := false
		start := time.Now()
		for i := 0; ; i++ {
			select {
			case <-quit:
				if shown {
					fmt.Fprint(os.Stderr, "\r\x1b[K")
				}
				return
			case <-t.C:
				if time.Since(start) < 300*time.Millisecond {
					continue
				}
				shown = true
				fmt.Fprintf(os.Stderr, "\r\x1b[K%s %s… %s of %s", frames[i%len(frames)], label,
					commas(done.Load()), commas(total))
			}
		}
	}()
	return func() { close(quit); <-finished }
}

// placeImage draws a kitty/sixel picture over the blank thumbnail area of
// the row just printed (lines tall), then puts the cursor back.
func placeImage(c *thumbCell, lines, col int) {
	if c == nil || c.esc == "" {
		return
	}
	fmt.Print("\x1b7" + "\x1b[" + strconv.Itoa(lines) + "A" + "\x1b[" + strconv.Itoa(col+c.off) + "G" + c.esc + "\x1b8")
}
