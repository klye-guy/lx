// lx — a plain-language evolution of `ls -l`.
//
// Same information as ls -l, but every column says what it means: sizes
// in GB, folder sizes that include everything inside them, kinds of item
// that are distinct at a glance, and thumbnails beside pictures, videos
// and songs. `-l` spells permissions out in words; `-T` turns it into a
// TreeSize-style map of what's using the space.
//
// Standard library only; build with CGO_ENABLED=0 for a static binary that
// runs on any Linux distribution.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const version = "1.3.0"

type Kind int

const (
	KFile Kind = iota
	KProgram
	KFolder
	KLink
	KBrokenLink
	KDevice
	KPipe
	KSocket
	KOther
)

var kindWord = map[Kind]string{
	KFile: "file", KProgram: "program", KFolder: "folder", KLink: "link",
	KBrokenLink: "broken link", KDevice: "device", KPipe: "pipe",
	KSocket: "socket", KOther: "other",
}

type Options struct {
	All, Tree, Reverse, OneFS, Disk, NoSize, Wide bool
	Long, NoThumbs, ForceThumbs                   bool
	Sort, Units, Color, Graphics                  string
	Depth, Top, Width, ThumbW                     int
	MinPct                                        float64
	Paths                                         []string
}

const helpText = `lx — ls, explained: a plain-language ls with real folder sizes

Usage: lx [options] [path ...]

LISTING (the default)
  Every item with its kind, size, last change, owner, and who can do what.
  Folder sizes include everything inside them. Pictures, videos and songs
  get a thumbnail beside them; permissions are shown as OWN GRP ALL letters.

  -l, --long         the full view: OWNER CAN / GROUP CAN / EVERYONE CAN
                     in words, no thumbnails
  -a, --all          also show hidden items (names starting with ".")
  -S                 sort by size, biggest first
  -t                 sort by last change, newest first
      --sort KEY     name | size | time | kind     (default: name, folders first)
  -r, --reverse      reverse the order
  -h, --human        pick a readable unit per item (KB, MB, GB...)
      --units U      gb | mb | kb | bytes | auto   (default: gb)
      --no-size      don't measure folder contents (instant, sizes shown as —)
      --wide         with -l, never fall back to r/w/x letters when narrow
      --width N      lay out for N columns (e.g. when piping into less -R);
                     piped output is otherwise one unwrapped line per item

THUMBNAILS
      --no-thumbs    plain text listing, no pictures (also: -l)
      --thumbs       show them even when piped (e.g. into less -R)
      --thumb-size N thumbnail width in character cells   (default: 8)
      --graphics G   auto | kitty | sixel | blocks        (default: auto)
                     auto asks the terminal: kitty and sixel draw real
                     pixels; blocks draws with colored ▀ characters and
                     works everywhere, including over ssh and in tmux.
  Previews come from your file manager's thumbnail cache when it has one,
  Go's own decoders for PNG/JPEG/GIF, and ffmpeg (if installed) for video,
  album art, WebP, HEIC, AVIF and the rest. lx never writes to the cache.

SPACE MAP
  -T, --tree         show what is using the space, biggest first, as a tree
  -L, --depth N      levels to show                         (default: 3)
      --top N        most items shown inside each folder    (default: 10)
      --min PCT      fold away items under PCT% of the total (default: 1)
                     The space map always includes hidden items.

MEASURING
  -x, --one-fs       don't count other drives mounted inside the path
      --disk         count space used on disk instead of file length
      --color WHEN   auto | always | never   (NO_COLOR is respected)
      --help, --version

READING THE PERMISSION COLUMNS (OWN GRP ALL by default; words with -l)
  read    open and view a file  /  see what is in a folder
  write   change a file         /  add, rename or delete things in a folder
  run     start a file as a program
  enter   go into a folder and reach the things inside it
  OWNER CAN = the item's owner, GROUP CAN = members of its group,
  EVERYONE CAN = every other user. For links, the target's permissions are shown.

FITTING THE TERMINAL
  As the terminal narrows the listing shortens the date, merges OWNER:GROUP,
  then drops the group (and with -l, switches to r/w/x letters first).
  Thumbnails go next, and below that it shows one card per item. Long names
  and notes wrap under the NAME column.

KINDS AT A GLANCE
  folder/  blue          program*  green        link → target  cyan
  file     plain         broken link  red       device / pipe / socket  yellow, magenta

NOTES
  1 GB here is 1024³ bytes (what ls -h, du and most file managers use).
  Hard-linked files are counted once. Links are never followed while measuring.
  /proc, /sys and other kernel filesystems are never counted.
  Sizes marked ≥ are minimums: something inside couldn't be read (try sudo).
`

func main() {
	o, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "lx:", err)
		fmt.Fprintln(os.Stderr, "Try 'lx --help'.")
		os.Exit(2)
	}
	p := NewPainter(o.Color)
	exit := 0
	for i, path := range o.Paths {
		if i > 0 {
			fmt.Println()
		}
		var e error
		if o.Tree {
			e = treeView(path, o, p)
		} else {
			e = listView(path, o, p)
		}
		if e != nil {
			fmt.Fprintf(os.Stderr, "lx: %s: %s\n", safeName(path), plainErr(e))
			exit = 2
		}
	}
	os.Exit(exit)
}

func parseArgs(args []string) (*Options, error) {
	o := &Options{Sort: "name", Units: "gb", Color: "auto", Graphics: "auto", Depth: 3, Top: 10, MinPct: 1, ThumbW: 8}
	needInt := func(name, v string) (int, error) {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%s needs a whole number, got %q", name, v)
		}
		return n, nil
	}
	setLong := func(name, val string, has bool, next func() (string, error)) error {
		get := func() (string, error) {
			if has {
				return val, nil
			}
			return next()
		}
		var err error
		var v string
		switch name {
		case "all":
			o.All = true
		case "tree":
			o.Tree = true
		case "reverse":
			o.Reverse = true
		case "one-fs", "one-file-system":
			o.OneFS = true
		case "disk":
			o.Disk = true
		case "no-size":
			o.NoSize = true
		case "wide":
			o.Wide = true
		case "long":
			o.Long = true
		case "no-thumbs", "no-thumbnails":
			o.NoThumbs = true
		case "thumbs", "thumbnails":
			o.ForceThumbs = true
		case "thumb-size":
			if v, err = get(); err == nil {
				o.ThumbW, err = needInt("--thumb-size", v)
				if err == nil && (o.ThumbW < 2 || o.ThumbW > 80) {
					err = fmt.Errorf("--thumb-size must be between 2 and 80 cells")
				}
			}
		case "graphics":
			if v, err = get(); err == nil {
				switch v {
				case "auto", "kitty", "sixel", "blocks":
					o.Graphics = v
				default:
					err = fmt.Errorf("--graphics must be auto, kitty, sixel or blocks")
				}
			}
		case "human", "human-readable":
			o.Units = "auto"
		case "help":
			fmt.Print(helpText)
			os.Exit(0)
		case "version":
			fmt.Println("lx", version)
			os.Exit(0)
		case "sort":
			if v, err = get(); err == nil {
				switch v {
				case "name", "size", "time", "kind":
					o.Sort = v
				default:
					err = fmt.Errorf("--sort must be name, size, time or kind")
				}
			}
		case "units":
			if v, err = get(); err == nil {
				v = strings.ToLower(v)
				switch v {
				case "gb", "mb", "kb", "bytes", "auto":
					o.Units = v
				default:
					err = fmt.Errorf("--units must be gb, mb, kb, bytes or auto")
				}
			}
		case "color", "colour":
			if v, err = get(); err == nil {
				switch v {
				case "auto", "always", "never":
					o.Color = v
				default:
					err = fmt.Errorf("--color must be auto, always or never")
				}
			}
		case "depth":
			if v, err = get(); err == nil {
				o.Depth, err = needInt("--depth", v)
			}
		case "width":
			if v, err = get(); err == nil {
				o.Width, err = needInt("--width", v)
			}
		case "top":
			if v, err = get(); err == nil {
				o.Top, err = needInt("--top", v)
			}
		case "min":
			if v, err = get(); err == nil {
				o.MinPct, err = strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64)
				if err != nil || o.MinPct < 0 {
					err = fmt.Errorf("--min needs a percentage, got %q", v)
				}
			}
		default:
			err = fmt.Errorf("unknown option --%s", name)
		}
		return err
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--":
			o.Paths = append(o.Paths, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "--"):
			name, val, has := strings.Cut(a[2:], "=")
			if err := setLong(name, val, has, next); err != nil {
				return nil, err
			}
		case len(a) > 1 && a[0] == '-':
			flags := a[1:]
			for j := 0; j < len(flags); j++ {
				ch := flags[j]
				switch ch {
				case 'a':
					o.All = true
				case 'T':
					o.Tree = true
				case 'r':
					o.Reverse = true
				case 'x':
					o.OneFS = true
				case 'h':
					o.Units = "auto"
				case 'S':
					o.Sort = "size"
				case 't':
					o.Sort = "time"
				case 'l':
					o.Long = true
				case '1':
					// ls habit; nothing to do
				case 'L':
					var v string
					if j+1 < len(flags) {
						v = flags[j+1:]
					} else {
						var err error
						if v, err = next(); err != nil {
							return nil, err
						}
					}
					n, err := needInt("-L", v)
					if err != nil {
						return nil, err
					}
					o.Depth = n
					j = len(flags)
				default:
					return nil, fmt.Errorf("unknown option -%c", ch)
				}
			}
		default:
			o.Paths = append(o.Paths, a)
		}
	}
	if len(o.Paths) == 0 {
		o.Paths = []string{"."}
	}
	return o, nil
}

func plainErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "permission denied"):
		return "permission denied (you don't have access)"
	case strings.Contains(s, "no such file"):
		return "doesn't exist"
	case strings.Contains(s, "not a directory"):
		return "part of the path isn't a folder"
	}
	if i := strings.LastIndex(s, ": "); i >= 0 {
		return s[i+2:]
	}
	return s
}

// progress shows a live counter on stderr while a big tree is measured.
func progress(sc *Scanner) (stop func()) {
	if !isTTY(os.Stderr) {
		return func() {}
	}
	done := make(chan struct{})
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
			case <-done:
				if shown {
					fmt.Fprint(os.Stderr, "\r\x1b[K")
				}
				return
			case <-t.C:
				if time.Since(start) < 300*time.Millisecond {
					continue // don't flash for quick scans
				}
				shown = true
				fmt.Fprintf(os.Stderr, "\r\x1b[K%s measuring… %s items", frames[i%len(frames)], commas(sc.count.Load()))
			}
		}
	}()
	return func() { close(done); <-finished }
}
