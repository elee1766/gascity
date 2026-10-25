package convtel

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// Attr is one OTLP attribute. Str is used unless IsInt or IsBool is set.
type Attr struct {
	Key    string
	Str    string
	Int    int64
	IsInt  bool
	Bool   bool
	IsBool bool
}

func strAttr(k, v string) Attr       { return Attr{Key: k, Str: v} }
func intAttr(k string, v int64) Attr { return Attr{Key: k, Int: v, IsInt: true} }
func boolAttr(k string, v bool) Attr { return Attr{Key: k, Bool: v, IsBool: true} }
func appendStr(a []Attr, k, v string) []Attr {
	if v == "" {
		return a
	}
	return append(a, strAttr(k, v))
}

// LogRecord is one exported conversation event.
type LogRecord struct {
	Time     time.Time
	Severity int // OTLP severity number: 9 INFO, 13 WARN, 17 ERROR
	Event    string
	Body     string
	TraceID  string
	SpanID   string
	Attrs    []Attr
}

// Span is one finished span.
type Span struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	Name         string
	Start, End   time.Time
	Attrs        []Attr
	// StatusCode is the OTLP status: 0 unset, 1 ok, 2 error.
	StatusCode int
	StatusMsg  string
}

const (
	sevInfo  = 9
	sevWarn  = 13
	sevError = 17
)

// stableID derives a hex ID of n bytes from parts, so a replay of the same
// wire stream yields the same IDs and duplicates can be recognized.
func stableID(n int, parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(strconv.Itoa(len(p))))
		h.Write([]byte{':'})
		h.Write([]byte(p))
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:n])
}

func traceID(parts ...string) string { return stableID(16, parts...) }
func spanID(parts ...string) string  { return stableID(8, parts...) }
