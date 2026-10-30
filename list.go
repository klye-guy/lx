package main

// list.go — the default view: an ls -l replacement that explains itself.

import (
	"os"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Entry struct {
	Name       string
	Path       string
	Kind       Kind
	Target     string // link destination
	TargetKind Kind
	PermMode   uint32 // mode bits whose permissions are displayed
	PermValid  bool
	Mode       uint32 // the item's own mode
	UID, GID   uint32
	Nlink      uint64
	Rdev       uint64
	MTime      time.Time
	Size       int64
	Sized      bool
	Partial    bool
	Denied     bool
	Skip       string
	st         syscall.Stat_t
}

func kindOfMode(m uint32) Kind {
	switch m & syscall.S_IFMT {
	case syscall.S_IFDIR:
		return KFolder
	case syscall.S_IFLNK:
		return KLink
	case syscall.S_IFREG:
		if m&0o111 != 0 {
			return KProgram
		}
		return KFile
	case syscall.S_IFCHR, syscall.S_IFBLK:
		return KDevice
	case syscall.S_IFIFO:
		return KPipe
	case syscall.S_IFSOCK:
		return KSocket
	}
	return KOther
}

func newEntry(path, name string, st *syscall.Stat_t, disk bool) *Entry {
	e := &Entry{
		Name: name, Path: path, Kind: kindOfMode(st.Mode),
		Mode: st.Mode, PermMode: st.Mode, PermValid: true,
		UID: st.Uid, GID: st.Gid, Nlink: uint64(st.Nlink), Rdev: uint64(st.Rdev),
		MTime: time.Unix(st.Mtim.Unix()), st: *st,
	}
	switch e.Kind {
	case KLink:
		e.Target, _ = os.Readlink(path)
		var tst syscall.Stat_t
		if syscall.Stat(path, &tst) == nil {
			e.TargetKind = kindOfMode(tst.Mode)
			e.PermMode = tst.Mode
		} else {
			e.Kind = KBrokenLink
			e.PermValid = false
		}
	case KFile, KProgram:
		e.Size, e.Sized = st.Size, true
		if disk {
			e.Size = int64(st.Blocks) * 512
		}
	}
	return e
}

// ---------------------------------------------------------------- owners

var (
	nameMu     sync.Mutex
	userCache  = map[uint32]string{}
	groupCache = map[uint32]string{}
)

func userName(id uint32) string {
	nameMu.Lock()
	defer nameMu.Unlock()
	if n, ok := userCache[id]; ok {
		return n
	}
	n := strconv.FormatUint(uint64(id), 10)
	if u, err := user.LookupId(n); err == nil {
		n = u.Username
	}
	userCache[id] = n
	return n
}

func groupName(id uint32) string {
	nameMu.Lock()
	defer nameMu.Unlock()
	if n, ok := groupCache[id]; ok {
		return n
	}
	n := strconv.FormatUint(uint64(id), 10)
	if g, err := user.LookupGroupId(n); err == nil {
		n = g.Name
	}
	groupCache[id] = n
	return n
}

// ---------------------------------------------------------------- helpers

func devNums(rdev uint64) (uint64, uint64) {
	major := (rdev>>8)&0xfff | (rdev>>32)&^uint64(0xfff)
	minor := rdev&0xff | (rdev>>12)&^uint64(0xff)
	return major, minor
}

func hasACL(path string) bool {
	n, err := syscall.Getxattr(path, "system.posix_acl_access", nil)
	return err == nil && n > 0
}

func sortKey(name string) string { return strings.ToLower(strings.TrimLeft(name, ".")) }

func sortEntries(es []*Entry, o *Options) {
	rank := func(k Kind) int {
		if k == KFolder {
			return 0
		}
		return 1
	}
	less := func(a, b *Entry) bool {
		switch o.Sort {
		case "size":
			if a.Size != b.Size {
				return a.Size > b.Size
			}
		case "time":
			if !a.MTime.Equal(b.MTime) {
				return a.MTime.After(b.MTime)
			}
		case "kind":
			if a.Kind != b.Kind {
				return kindWord[a.Kind] < kindWord[b.Kind]
			}
		default:
			if rank(a.Kind) != rank(b.Kind) {
				return rank(a.Kind) < rank(b.Kind)
			}
		}
		ka, kb := sortKey(a.Name), sortKey(b.Name)
		if ka != kb {
			return ka < kb
		}
		return a.Name < b.Name
	}
	sort.SliceStable(es, func(i, j int) bool {
		if o.Reverse {
			return less(es[j], es[i])
		}
		return less(es[i], es[j])
	})
}
