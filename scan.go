package main

// scan.go — the recursive sizing engine.
//
// One walker serves both views. For the listing it only returns totals;
// for the space map it also keeps the biggest children of each folder up
// to the requested depth, folding everything else into a "smaller items"
// summary so memory stays bounded even on trees with millions of files.

import (
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
)

// Node is a measured item.
type Node struct {
	Name     string
	Kind     Kind
	Size     int64 // bytes, including everything inside for folders
	Items    int64 // entries inside (recursive), not counting itself
	Partial  bool  // something inside couldn't be read; Size is a minimum
	Denied   bool  // this folder itself couldn't be opened
	Skip     string
	Children []*Node
	RestN    int64 // children pruned from Children
	RestSize int64
}

type Scanner struct {
	disk   bool
	oneFS  bool
	sem    chan struct{}
	seenMu sync.Mutex
	seen   map[[2]uint64]struct{}

	count   atomic.Int64 // items visited (for the progress line)
	denied  atomic.Int64 // folders that couldn't be opened
	skipped atomic.Int64 // mounts not counted
}

func NewScanner(disk, oneFS bool) *Scanner {
	n := runtime.NumCPU() * 4
	if n < 16 {
		n = 16
	}
	if n > 128 {
		n = 128
	}
	return &Scanner{
		disk:  disk,
		oneFS: oneFS,
		sem:   make(chan struct{}, n),
		seen:  make(map[[2]uint64]struct{}),
	}
}

func (s *Scanner) rawSize(st *syscall.Stat_t) int64 {
	if s.disk {
		return int64(st.Blocks) * 512
	}
	return st.Size
}

// contribution returns what a non-folder adds to its parent's total.
// Hard-linked files are counted once, the way du does.
func (s *Scanner) contribution(st *syscall.Stat_t) int64 {
	if uint64(st.Nlink) > 1 {
		key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
		s.seenMu.Lock()
		_, dup := s.seen[key]
		if !dup {
			s.seen[key] = struct{}{}
		}
		s.seenMu.Unlock()
		if dup {
			return 0
		}
	}
	return s.rawSize(st)
}

// Kernel pseudo-filesystems: never counted (their "sizes" are fiction —
// /proc/kcore alone claims 128 TB).
var pseudoFS = map[uint32]string{
	0x9fa0:     "proc",
	0x62656572: "sysfs",
	0x27e0eb:   "cgroup",
	0x63677270: "cgroup2",
	0x64626720: "debugfs",
	0x74726163: "tracefs",
	0x73636673: "securityfs",
	0x6165676c: "pstore",
	0xcafe4a11: "bpf",
	0x62656570: "configfs",
	0x65735543: "fusectl",
	0x42494e4d: "binfmt_misc",
	0xde5e81e4: "efivarfs",
	0x1cd1:     "devpts",
	0x6e736673: "nsfs",
	0x19800202: "mqueue",
}

// skipMount decides whether a folder that sits on a different device
// than its parent should be left out of the totals.
func (s *Scanner) skipMount(path string, st *syscall.Stat_t, parentDev, rootDev uint64) string {
	dev := uint64(st.Dev)
	if dev == parentDev {
		return ""
	}
	if s.oneFS && dev != rootDev {
		s.skipped.Add(1)
		return "separate filesystem — not counted (-x)"
	}
	var sf syscall.Statfs_t
	if syscall.Statfs(path, &sf) == nil {
		if name, ok := pseudoFS[uint32(sf.Type)]; ok {
			s.skipped.Add(1)
			return "kernel " + name + " filesystem — not counted"
		}
	}
	return ""
}

// scanDir measures a folder. keep = how many more levels of children to
// retain (0 = totals only); top = children retained per folder.
func (s *Scanner) scanDir(path string, st *syscall.Stat_t, rootDev uint64, keep, top int) *Node {
	n := &Node{Kind: KFolder, Size: s.rawSize(st)}
	s.count.Add(1)
	myDev := uint64(st.Dev)

	ents, err := os.ReadDir(path)
	if err != nil {
		n.Partial = true
		s.denied.Add(1)
		if len(ents) == 0 {
			n.Denied = true
			return n
		}
	}

	results := make([]*Node, len(ents))
	var wg sync.WaitGroup
	prefix := path
	if prefix != "/" {
		prefix += "/"
	}

	for i, de := range ents {
		cp := prefix + de.Name()
		var cst syscall.Stat_t
		if err := syscall.Lstat(cp, &cst); err != nil {
			n.Partial = true
			continue
		}
		if cst.Mode&syscall.S_IFMT == syscall.S_IFDIR {
			if why := s.skipMount(cp, &cst, myDev, rootDev); why != "" {
				if keep > 0 {
					results[i] = &Node{Name: de.Name(), Kind: KFolder, Skip: why}
				}
				n.Items++
				continue
			}
			select {
			case s.sem <- struct{}{}:
				wg.Add(1)
				go func(i int, cp, name string, cst syscall.Stat_t) {
					defer wg.Done()
					c := s.scanDir(cp, &cst, rootDev, keep-1, top)
					<-s.sem
					c.Name = name
					results[i] = c
				}(i, cp, de.Name(), cst)
			default:
				c := s.scanDir(cp, &cst, rootDev, keep-1, top)
				c.Name = de.Name()
				results[i] = c
			}
			continue
		}
		// anything that isn't a folder
		s.count.Add(1)
		sz := s.contribution(&cst)
		n.Size += sz
		n.Items++
		if keep > 0 {
			results[i] = &Node{Name: de.Name(), Kind: kindOfMode(cst.Mode), Size: sz}
		}
	}
	wg.Wait()

	var kids []*Node
	for _, c := range results {
		if c == nil {
			continue
		}
		if c.Kind == KFolder && c.Skip == "" {
			n.Size += c.Size
			n.Items += c.Items + 1
			if c.Partial {
				n.Partial = true
			}
		}
		if keep > 0 {
			kids = append(kids, c)
		}
	}
	if keep > 0 {
		sort.Slice(kids, func(a, b int) bool {
			if kids[a].Size != kids[b].Size {
				return kids[a].Size > kids[b].Size
			}
			return kids[a].Name < kids[b].Name
		})
		if top > 0 && len(kids) > top {
			for _, c := range kids[top:] {
				n.RestN++
				n.RestSize += c.Size
			}
			kids = kids[:top]
		}
		n.Children = kids
	}
	return n
}
