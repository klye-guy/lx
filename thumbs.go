package main

// thumbs.go — finding a preview picture for a file.
//
// Sources, cheapest first:
//
//  1. The freedesktop thumbnail cache (~/.cache/thumbnails) that Dolphin,
//     Nautilus, Thunar and friends already fill. Read only, and checked
//     against the file's modification time so a stale preview is never
//     shown. This is also how PDFs, documents and fonts get previews.
//  2. Go's own decoders for PNG, JPEG (honouring EXIF rotation) and GIF.
//  3. ffmpeg, when it's installed, for video frames, album art, and the
//     image formats Go can't read (WebP, HEIC, AVIF, TIFF, BMP, ...).
//
// Nothing is ever written: lx doesn't add to the cache.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type mediaKind int

const (
	mNone mediaKind = iota
	mImage
	mVideo
	mAudio
	mOther // not media by name, but a file manager has thumbnailed it
)

var extKinds = func() map[string]mediaKind {
	m := map[string]mediaKind{}
	add := func(k mediaKind, exts string) {
		for _, e := range strings.Fields(exts) {
			m["."+e] = k
		}
	}
	add(mImage, "png jpg jpeg jpe jfif gif webp bmp dib tif tiff heic heif avif jxl ico tga qoi ppm pgm pbm pnm pcx dds exr hdr psd")
	add(mVideo, "mp4 m4v mkv webm mov avi wmv asf flv mpg mpeg m2v m2ts mts ts 3gp 3g2 ogv vob mxf divx rmvb f4v")
	add(mAudio, "mp3 flac m4a m4b aac ogg oga opus wav wma aiff aif ape wv mka alac dsf")
	return m
}()

// nativeExt are the formats Go's standard library decodes itself.
var nativeExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".jpe": true, ".jfif": true, ".gif": true}

func mediaKindOf(name string) mediaKind {
	return extKinds[strings.ToLower(filepath.Ext(name))]
}

// maxPixels guards against decompression bombs: a tiny PNG that claims to
// be 100,000 × 100,000 would otherwise eat gigabytes of memory.
const maxPixels = 80_000_000

var errTooBig = errors.New("too big to preview")

// ---------------------------------------------------------------- info

// mediaInfo is what the caption under a preview says.
type mediaInfo struct {
	w, h int     // original picture/video dimensions; 0 = unknown
	dur  float64 // seconds; 0 = unknown or not applicable
}

// ---------------------------------------------------------------- freedesktop cache

func thumbCacheDirs() []string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		base = filepath.Join(home, ".cache")
	}
	t := filepath.Join(base, "thumbnails")
	// largest first: we'd rather shrink a preview than enlarge one
	return []string{filepath.Join(t, "x-large"), filepath.Join(t, "large"),
		filepath.Join(t, "xx-large"), filepath.Join(t, "normal")}
}

var cacheDirs = sync.OnceValue(thumbCacheDirs)

// fileURI builds the file:// URI the freedesktop spec hashes, escaped the
// way GLib's g_filename_to_uri does it.
func fileURI(abs string) string {
	const allowed = "-._~!$&'()*+,;=:@/"
	var b strings.Builder
	b.WriteString("file://")
	for i := 0; i < len(abs); i++ {
		c := abs[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(allowed, c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// cachedThumbPath returns the cache file holding an up-to-date preview of
// abs, or "" if no file manager has made one.
func cachedThumbPath(abs string, mtime time.Time) (string, []byte) {
	sum := md5.Sum([]byte(fileURI(abs)))
	name := hex.EncodeToString(sum[:]) + ".png"
	want := strconv.FormatInt(mtime.Unix(), 10)
	for _, d := range cacheDirs() {
		p := filepath.Join(d, name)
		fi, err := os.Stat(p)
		if err != nil || fi.Size() > 16<<20 {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		mt, ok := pngText(data, "Thumb::MTime")
		if !ok {
			continue
		}
		// some thumbnailers write fractional seconds
		if whole, _, _ := strings.Cut(mt, "."); whole != want {
			continue
		}
		return p, data
	}
	return "", nil
}

// pngText reads one tEXt entry from a PNG without decoding the picture.
func pngText(data []byte, key string) (string, bool) {
	if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		return "", false
	}
	for i := 8; i+12 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[i:]))
		typ := string(data[i+4 : i+8])
		if n < 0 || i+12+n > len(data) {
			break
		}
		body := data[i+8 : i+8+n]
		if typ == "tEXt" {
			if k, v, ok := bytes.Cut(body, []byte{0}); ok && string(k) == key {
				return string(v), true
			}
		}
		if typ == "IEND" {
			break
		}
		i += 12 + n
	}
	return "", false
}

// ---------------------------------------------------------------- native decoding

func decodeNative(path string) (image.Image, mediaInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, mediaInfo{}, err
	}
	defer f.Close()
	cfg, format, err := image.DecodeConfig(bufio.NewReader(f))
	if err != nil {
		return nil, mediaInfo{}, err
	}
	info := mediaInfo{w: cfg.Width, h: cfg.Height}
	if int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, info, errTooBig
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, info, err
	}
	img, _, err := image.Decode(bufio.NewReader(f))
	if err != nil {
		return nil, info, err
	}
	if format == "jpeg" {
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			head := make([]byte, 128<<10)
			n, _ := io.ReadFull(f, head)
			if o := jpegOrientation(head[:n]); o != 1 {
				img = orientedImage{img, o}
				if o >= 5 {
					info.w, info.h = info.h, info.w
				}
			}
		}
	}
	return img, info, nil
}

// nativeConfig reads just the dimensions (for captions when the picture
// itself came from the cache).
func nativeConfig(path string) mediaInfo {
	f, err := os.Open(path)
	if err != nil {
		return mediaInfo{}
	}
	defer f.Close()
	cfg, format, err := image.DecodeConfig(bufio.NewReader(f))
	if err != nil {
		return mediaInfo{}
	}
	info := mediaInfo{w: cfg.Width, h: cfg.Height}
	if format == "jpeg" {
		f.Seek(0, io.SeekStart)
		head := make([]byte, 128<<10)
		n, _ := io.ReadFull(f, head)
		if jpegOrientation(head[:n]) >= 5 {
			info.w, info.h = info.h, info.w
		}
	}
	return info
}

// jpegOrientation finds the EXIF orientation tag (1–8) so phone photos
// come out the right way up. 1 = as stored.
func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		if marker == 0xFF { // fill byte
			i++
			continue
		}
		if marker == 0x01 || marker >= 0xD0 && marker <= 0xD7 {
			i += 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 {
			return 1 // reached the picture data: no EXIF before it
		}
		l := int(b[i+2])<<8 | int(b[i+3])
		if l < 2 {
			return 1
		}
		end := i + 2 + l
		if end > len(b) {
			end = len(b)
		}
		seg := b[i+4 : end]
		if marker == 0xE1 && len(seg) >= 14 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		i += 2 + l
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:]))
	if off < 8 || off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off:]))
	for k := 0; k < n; k++ {
		e := off + 2 + 12*k
		if e+12 > len(t) {
			break
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// orientedImage presents an image turned or flipped by an EXIF
// orientation, without copying it. Scaling reads it through At, which is
// fine because the scaler only samples a bounded number of pixels.
type orientedImage struct {
	src image.Image
	o   int
}

func (m orientedImage) ColorModel() color.Model { return m.src.ColorModel() }

func (m orientedImage) Bounds() image.Rectangle {
	b := m.src.Bounds()
	if m.o >= 5 {
		return image.Rect(0, 0, b.Dy(), b.Dx())
	}
	return image.Rect(0, 0, b.Dx(), b.Dy())
}

func (m orientedImage) At(x, y int) color.Color {
	b := m.src.Bounds()
	w, h := b.Dx(), b.Dy()
	var sx, sy int
	switch m.o {
	case 2:
		sx, sy = w-1-x, y
	case 3:
		sx, sy = w-1-x, h-1-y
	case 4:
		sx, sy = x, h-1-y
	case 5:
		sx, sy = y, x
	case 6:
		sx, sy = y, h-1-x
	case 7:
		sx, sy = w-1-y, h-1-x
	case 8:
		sx, sy = w-1-y, x
	default:
		sx, sy = x, y
	}
	return m.src.At(b.Min.X+sx, b.Min.Y+sy)
}

// ---------------------------------------------------------------- ffmpeg

var (
	ffmpegPath  = sync.OnceValue(func() string { p, _ := exec.LookPath("ffmpeg"); return p })
	ffprobePath = sync.OnceValue(func() string { p, _ := exec.LookPath("ffprobe"); return p })
)

// ffmpeg reads only local files (-protocol_whitelist file), so a playlist
// or .m3u8 can't make it fetch anything from the network, and the path is
// passed as file:… so names like "concat:x" or "http:y" aren't treated as
// instructions. (For images, -pattern_type none also stops "%d" in a
// name being expanded into a numbered sequence.)
func ffInput(abs string) []string {
	return []string{"-protocol_whitelist", "file", "-i", "file:" + abs}
}

func runLimited(ctx context.Context, limit int64, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = nil
	cmd.Stderr = nil
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	data, rerr := io.ReadAll(io.LimitReader(out, limit))
	io.Copy(io.Discard, out)
	werr := cmd.Wait()
	if rerr != nil {
		return nil, rerr
	}
	if len(data) == 0 && werr != nil {
		return nil, werr
	}
	return data, nil
}

func probe(abs string) mediaInfo {
	if ffprobePath() == "" {
		return mediaInfo{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{"-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height:stream_side_data=rotation:format=duration",
		"-of", "default=noprint_wrappers=1"}
	args = append(args, "-protocol_whitelist", "file", "file:"+abs)
	out, err := runLimited(ctx, 64<<10, ffprobePath(), args...)
	if err != nil && len(out) == 0 {
		return mediaInfo{}
	}
	var info mediaInfo
	rot := 0
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "width":
			info.w, _ = strconv.Atoi(v)
		case "height":
			info.h, _ = strconv.Atoi(v)
		case "duration":
			if d, err := strconv.ParseFloat(v, 64); err == nil && d > info.dur {
				info.dur = d
			}
		case "rotation":
			rot, _ = strconv.Atoi(v)
		}
	}
	if rot == 90 || rot == -90 || rot == 270 || rot == -270 {
		info.w, info.h = info.h, info.w // phone videos shot upright
	}
	return info
}

// ffmpegFrame grabs one frame (a video still, album art, or an image Go
// can't decode) scaled to fit within maxW × maxH pixels.
func ffmpegFrame(abs string, mk mediaKind, dur float64, maxW, maxH int) (image.Image, error) {
	if ffmpegPath() == "" {
		return nil, errNoFFmpeg
	}
	grab := func(at float64) (image.Image, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		args := []string{"-nostdin", "-hide_banner", "-v", "error"}
		if at > 0 {
			args = append(args, "-ss", strconv.FormatFloat(at, 'f', 2, 64))
		}
		if mk == mImage && strings.Contains(filepath.Base(abs), "%") {
			// otherwise "photo %d.png" is read as a numbered sequence;
			// only the image2 reader understands this option
			args = append(args, "-f", "image2", "-pattern_type", "none")
		}
		args = append(args, ffInput(abs)...)
		args = append(args, "-map", "0:v:0", "-frames:v", "1",
			"-vf", "scale="+strconv.Itoa(maxW)+":"+strconv.Itoa(maxH)+":force_original_aspect_ratio=decrease:flags=area",
			"-f", "image2pipe", "-c:v", "png", "-")
		data, err := runLimited(ctx, 64<<20, ffmpegPath(), args...)
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			return nil, errNoFrame
		}
		return png.Decode(bytes.NewReader(data))
	}
	at := 0.0
	if mk == mVideo && dur > 0 {
		// a tenth of the way in skips black intros and studio logos
		at = dur * 0.1
		if at > 300 {
			at = 300
		}
	}
	img, err := grab(at)
	if err != nil && at > 0 {
		img, err = grab(0)
	}
	return img, err
}

var (
	errNoFFmpeg = errors.New("needs ffmpeg")
	errNoFrame  = errors.New("no picture inside")
)

// ---------------------------------------------------------------- the chain

// findPreview returns a picture for path (any size; the caller scales it)
// plus caption info. maxW/maxH are the pixel box it will be shown in, so
// ffmpeg can scale while decoding.
func findPreview(path string, mk mediaKind, mtime time.Time, maxW, maxH int) (image.Image, mediaInfo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	// resolve links so the cache key matches what a file manager hashed
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	ext := strings.ToLower(filepath.Ext(abs))
	native := nativeExt[ext]

	var info mediaInfo
	needProbe := mk == mVideo || mk == mAudio || (mk == mImage && !native)

	if _, data := cachedThumbPath(abs, mtime); data != nil {
		if img, err := png.Decode(bytes.NewReader(data)); err == nil {
			switch {
			case native:
				info = nativeConfig(abs)
			case needProbe:
				info = probe(abs)
			}
			return img, info, nil
		}
	}

	if native {
		img, ninfo, err := decodeNative(abs)
		info = ninfo
		if err == nil {
			return img, info, nil
		}
		if !errors.Is(err, errTooBig) && ffmpegPath() == "" {
			return nil, info, errors.New("can't be read")
		}
		// fall through: ffmpeg can scale a huge image while decoding it
		needProbe = info.w == 0
	}
	if mk == mOther {
		return nil, info, errors.New("no preview")
	}
	if needProbe {
		info = probe(abs)
	}
	img, err := ffmpegFrame(abs, mk, info.dur, maxW, maxH)
	if err != nil {
		if errors.Is(err, errNoFFmpeg) {
			return nil, info, err
		}
		if mk == mAudio {
			return nil, info, errNoFrame // no album art
		}
		return nil, info, errors.New("can't be read")
	}
	return img, info, nil
}
