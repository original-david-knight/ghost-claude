package diagnostics

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strings"
	"sync"
	"unicode/utf8"
)

const promptElisionMarker = "\n...[vibedrive diagnostics truncated]...\n"
const titleElisionMarker = "...[truncated]"

type TailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
	total int64
	hash  hash.Hash
}

type TailSnapshot struct {
	Data          []byte
	OriginalBytes int64
	SHA256        string
}

func NewTailBuffer(limit int) *TailBuffer {
	if limit < 0 {
		limit = 0
	}
	return &TailBuffer{
		limit: limit,
		hash:  sha256.New(),
	}
}

func (b *TailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.hash != nil {
		_, _ = b.hash.Write(p)
	}
	b.total += int64(len(p))

	if b.limit == 0 {
		return len(p), nil
	}
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}
	if len(b.buf)+len(p) > b.limit {
		drop := len(b.buf) + len(p) - b.limit
		copy(b.buf, b.buf[drop:])
		b.buf = b.buf[:len(b.buf)-drop]
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *TailBuffer) Snapshot() TailSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()

	var digest string
	if b.hash != nil {
		digest = hex.EncodeToString(b.hash.Sum(nil))
	}
	return TailSnapshot{
		Data:          append([]byte(nil), b.buf...),
		OriginalBytes: b.total,
		SHA256:        digest,
	}
}

func (s TailSnapshot) Bytes() ByteArtifact {
	return ByteArtifact{
		Available:     true,
		Data:          append([]byte(nil), s.Data...),
		OriginalBytes: s.OriginalBytes,
		SHA256:        s.SHA256,
	}
}

type boundedBytes struct {
	data          []byte
	truncated     bool
	originalBytes int64
	originalLines int
	sha256        string
	limitBytes    int64
	limitLines    int
}

func boundTailBytes(src ByteArtifact, limit int) boundedBytes {
	return boundBytes(src, limit, true)
}

func boundPromptBytes(src ByteArtifact, limit int) boundedBytes {
	return boundBytes(src, limit, false)
}

func boundBytes(src ByteArtifact, limit int, tailPreferred bool) boundedBytes {
	originalBytes := sourceOriginalBytes(src)
	digest := sourceDigest(src)
	data := append([]byte(nil), src.Data...)
	truncated := originalBytes > int64(len(data))

	if len(data) > limit {
		if digest == "" && originalBytes == int64(len(data)) {
			digest = sha256Hex(data)
		}
		if tailPreferred {
			data = tailBytes(data, limit)
		} else {
			data = middleBytes(data, limit)
		}
		truncated = true
	}

	return boundedBytes{
		data:          data,
		truncated:     truncated,
		originalBytes: originalBytes,
		sha256:        digest,
		limitBytes:    int64(limit),
	}
}

func boundPaneBytes(src ByteArtifact) boundedBytes {
	originalBytes := sourceOriginalBytes(src)
	digest := sourceDigest(src)
	data := append([]byte(nil), src.Data...)
	originalLines := src.OriginalLines
	if originalLines <= 0 {
		originalLines = countLines(data)
	}

	truncated := originalBytes > int64(len(data))
	if originalLines > PaneLineLimit {
		if digest == "" && originalBytes == int64(len(data)) {
			digest = sha256Hex(data)
		}
		data = tailLines(data, PaneLineLimit)
		truncated = true
	}
	if len(data) > PaneByteLimit {
		if digest == "" && originalBytes == int64(len(src.Data)) {
			digest = sha256Hex(src.Data)
		}
		data = tailBytes(data, PaneByteLimit)
		truncated = true
	}

	return boundedBytes{
		data:          data,
		truncated:     truncated,
		originalBytes: originalBytes,
		originalLines: originalLines,
		sha256:        digest,
		limitBytes:    PaneByteLimit,
		limitLines:    PaneLineLimit,
	}
}

func NormalizePromptText(data []byte) []byte {
	text := strings.ToValidUTF8(string(data), "\uFFFD")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return []byte(text)
}

func sourceOriginalBytes(src ByteArtifact) int64 {
	if src.OriginalBytes > 0 {
		if src.OriginalBytes < int64(len(src.Data)) {
			return int64(len(src.Data))
		}
		return src.OriginalBytes
	}
	return int64(len(src.Data))
}

func sourceDigest(src ByteArtifact) string {
	if src.SHA256 != "" {
		return src.SHA256
	}
	if sourceOriginalBytes(src) == int64(len(src.Data)) {
		return sha256Hex(src.Data)
	}
	return ""
}

func tailBytes(data []byte, limit int) []byte {
	if limit <= 0 {
		return nil
	}
	if len(data) <= limit {
		return append([]byte(nil), data...)
	}
	return append([]byte(nil), data[len(data)-limit:]...)
}

func middleBytes(data []byte, limit int) []byte {
	if limit <= 0 {
		return nil
	}
	if len(data) <= limit {
		return append([]byte(nil), data...)
	}

	marker := []byte(promptElisionMarker)
	if len(marker) >= limit {
		return append([]byte(nil), marker[:limit]...)
	}

	remaining := limit - len(marker)
	front := remaining / 2
	back := remaining - front

	out := make([]byte, 0, limit)
	out = append(out, data[:front]...)
	out = append(out, marker...)
	out = append(out, data[len(data)-back:]...)
	return out
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	lines := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}

func tailLines(data []byte, limit int) []byte {
	if limit <= 0 || len(data) == 0 {
		return nil
	}
	originalLines := countLines(data)
	if originalLines <= limit {
		return append([]byte(nil), data...)
	}

	skip := originalLines - limit
	start := 0
	for range skip {
		idx := bytes.IndexByte(data[start:], '\n')
		if idx < 0 {
			return append([]byte(nil), data...)
		}
		start += idx + 1
	}
	return append([]byte(nil), data[start:]...)
}

func truncateStringBytes(s string, limit int) (string, bool, int) {
	original := len([]byte(s))
	if original <= limit {
		return s, false, original
	}
	if limit <= 0 {
		return "", true, original
	}
	marker := titleElisionMarker
	if len(marker) >= limit {
		return marker[:limit], true, original
	}

	var b strings.Builder
	b.Grow(limit)
	for _, r := range s {
		if !utf8.ValidRune(r) {
			r = utf8.RuneError
		}
		if b.Len()+utf8.RuneLen(r)+len(marker) > limit {
			break
		}
		b.WriteRune(r)
	}
	b.WriteString(marker)
	return b.String(), true, original
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
