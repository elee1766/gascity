package convtel

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Redacted replaces a credential in exported content.
const Redacted = "<redacted>"

// secretKeyPattern matches JSON object keys whose values are credentials.
var secretKeyPattern = regexp.MustCompile(`(?i)(pass(word|wd|phrase)?|secret|token|api[_-]?key|apikey|auth(orization)?|cookie|credential|private[_-]?key|session[_-]?key|access[_-]?key|client[_-]?secret|bearer)$`)

// secretKeyAllow lists keys that match secretKeyPattern but hold counts or
// identifiers rather than credentials.
var secretKeyAllow = map[string]bool{
	"inputtokens": true, "outputtokens": true, "thoughttokens": true, "totaltokens": true,
	"cachedreadtokens": true, "cachedwritetokens": true, "maxtokens": true, "max_tokens": true,
	"input_tokens": true, "output_tokens": true, "total_tokens": true,
}

type textPattern struct {
	re *regexp.Regexp
	// keep is the number of leading submatch groups to keep, so "Bearer <redacted>"
	// stays readable.
	keep int
}

// textPatterns are credential shapes scrubbed from free text.
var textPatterns = []textPattern{
	{re: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`)},
	{re: regexp.MustCompile(`(?i)(\bauthorization:\s*(?:bearer|basic|token)\s+)[A-Za-z0-9._~+/=-]{8,}`), keep: 1},
	{re: regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/=-]{16,}`), keep: 1},
	{re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{re: regexp.MustCompile(`\b(sk|rk|pk)-(ant-|proj-|live-|test-)?[A-Za-z0-9_-]{20,}`)},
	{re: regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{30,}`)},
	{re: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{30,}`)},
	{re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`)},
	{re: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{re: regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`)},
	{re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{re: regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+:)[^\s@/]+@`), keep: 1},
	// KEY=value and key: value assignments whose name says credential.
	{re: regexp.MustCompile(`(?i)(\b[A-Za-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|private[_-]?key|access[_-]?key|credential)[A-Za-z0-9_.-]*\s*[:=]\s*["']?)[^\s"',;]{6,}`), keep: 1},
}

// Redactor scrubs credential material from content before it is queued.
type Redactor struct {
	// values are exact secret strings (session env secrets), longest first.
	values []string
}

// NewRedactor returns a redactor that also replaces each exact value of at
// least 8 bytes.
func NewRedactor(values []string) *Redactor {
	r := &Redactor{}
	for _, v := range values {
		if len(v) >= 8 {
			r.values = append(r.values, v)
		}
	}
	sort.SliceStable(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
	return r
}

// Text scrubs s and reports how many substitutions were made.
func (r *Redactor) Text(s string) (string, int) {
	n := 0
	if r != nil {
		for _, v := range r.values {
			if c := strings.Count(s, v); c > 0 {
				n += c
				s = strings.ReplaceAll(s, v, Redacted)
			}
		}
	}
	for _, p := range textPatterns {
		s = p.re.ReplaceAllStringFunc(s, func(m string) string {
			n++
			if p.keep == 0 {
				return Redacted
			}
			sub := p.re.FindStringSubmatch(m)
			return sub[p.keep] + Redacted
		})
	}
	return s, n
}

// JSON scrubs a JSON value: values under credential-named keys are replaced,
// and every string is text-scrubbed. It returns compact JSON text. Invalid
// JSON is treated as text.
func (r *Redactor) JSON(raw json.RawMessage) (string, int) {
	if len(raw) == 0 {
		return "", 0
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return r.Text(string(raw))
	}
	n := 0
	v = r.walk(v, &n)
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return Redacted, n + 1
	}
	return strings.TrimRight(buf.String(), "\n"), n
}

func (r *Redactor) walk(v any, n *int) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if isSecretKey(k) {
				if s, ok := child.(string); ok && s == "" {
					continue
				}
				if child == nil {
					continue
				}
				t[k] = Redacted
				*n++
				continue
			}
			t[k] = r.walk(child, n)
		}
		return t
	case []any:
		for i := range t {
			t[i] = r.walk(t[i], n)
		}
		return t
	case string:
		s, c := r.Text(t)
		*n += c
		return s
	default:
		return v
	}
}

func isSecretKey(k string) bool {
	lk := strings.ToLower(k)
	if secretKeyAllow[lk] {
		return false
	}
	return secretKeyPattern.MatchString(lk)
}

// Truncate caps s at max bytes on a rune boundary, appending an explicit
// marker. It reports whether s was cut.
func Truncate(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…[gc truncated " + strconv.Itoa(len(s)-cut) + " bytes]", true
}
