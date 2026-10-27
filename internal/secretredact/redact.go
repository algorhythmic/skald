// Package secretredact supplies deterministic, count-only secret detection and
// irreversible replacement. Patterns use Go's linear-time RE2 engine. It is a
// best-effort policy, not proof that arbitrary text contains no secrets.
package secretredact

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

type rule struct {
	id    string
	re    *regexp.Regexp
	group int
}

var rules = []rule{
	{"github_token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`), 0},
	{"provider_key", regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}\b`), 0},
	{"aws_access_key", regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), 0},
	{"bearer_token", regexp.MustCompile(`(?i)\bbearer[ \t]+([A-Za-z0-9._~+/=-]{8,})`), 1},
	{"password_assignment", regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|api[_-]?key|access[_-]?token|client[_-]?secret|secret[_-]?key|pihole[_-]?(?:password|webpassword))[ \t]*[=:][ \t]*("[^"\r\n]+"|'[^'\r\n]+'|[^\s"'\\,;}\]]+)`), 1},
	{"private_key", regexp.MustCompile(`(?s)-----BEGIN (?:RSA |EC |OPENSSH |DSA |ENCRYPTED )?PRIVATE KEY-----.*?-----END (?:RSA |EC |OPENSSH |DSA |ENCRYPTED )?PRIVATE KEY-----`), 0},
}
var sensitiveKey = regexp.MustCompile(`(?i)^(?:password|passwd|pwd|api[_-]?key|access[_-]?token|refresh[_-]?token|client[_-]?secret|secret[_-]?key|authorization|webpassword|pihole[_-]?password)$`)

func IDs() []string {
	out := []string{}
	for _, r := range rules {
		out = append(out, r.id)
	}
	return out
}
func Counts() map[string]int64 {
	out := map[string]int64{}
	for _, id := range IDs() {
		out[id] = 0
	}
	return out
}
func marker(id string) string { return "[REDACTED:" + id + "]" }
func marked(s string) bool    { return strings.HasPrefix(s, "[REDACTED:") && strings.HasSuffix(s, "]") }

// Text never returns matches, hashes, locations or diagnostic context.
func Text(s string, counts map[string]int64) string {
	for _, r := range rules {
		indexes := r.re.FindAllStringSubmatchIndex(s, -1)
		if len(indexes) == 0 {
			continue
		}
		var out strings.Builder
		last := 0
		for _, m := range indexes {
			a, b := m[2*r.group], m[2*r.group+1]
			if strings.HasPrefix(s[a:b], "[REDACTED:") {
				continue
			}
			out.WriteString(s[last:a])
			out.WriteString(marker(r.id))
			last = b
			if counts != nil {
				counts[r.id]++
			}
		}
		out.WriteString(s[last:])
		s = out.String()
	}
	return s
}

// JSON walks decoded strings, including escaped credentials and sensitive-key
// values. Unchanged JSON retains its original bytes and numeric lexemes. Invalid
// native records still receive text filtering. No input is included in errors.
func JSON(data []byte, counts map[string]int64) []byte {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value any
	if d.Decode(&value) != nil {
		return []byte(Text(string(data), counts))
	}
	changed := false
	var walk func(any, string) any
	walk = func(v any, key string) any {
		switch x := v.(type) {
		case string:
			y := x
			if sensitiveKey.MatchString(key) && x != "" && !marked(x) {
				y = marker("password_assignment")
				if counts != nil {
					counts["password_assignment"]++
				}
			} else {
				y = Text(x, counts)
			}
			if y != x {
				changed = true
			}
			return y
		case map[string]any:
			for k, v := range x {
				x[k] = walk(v, k)
			}
		case []any:
			for i, v := range x {
				x[i] = walk(v, key)
			}
		}
		return v
	}
	value = walk(value, "")
	if !changed {
		return data
	}
	out, err := json.Marshal(value)
	if err != nil {
		return []byte(marker("invalid_json"))
	}
	return out
}

// Check rejects sensitive input without exposing any match. Call before any
// durable write of capture bytes. Until a versioned sanitized capture contract
// exists, rejection preserves exact source digests and leaves cursors unchanged.
func Check(data []byte) error {
	if !bytes.Equal(JSON(data, nil), data) {
		return ErrSensitive
	}
	return nil
}

var ErrSensitive = errors.New("secret_redaction_required")

// CheckWire additionally inspects base64-encoded raw byte fields emitted by
// encoding/json. Sensitive exact exports are withheld rather than silently
// breaking their digest contract. Ordinary text projections may use JSON.
func CheckWire(data []byte) error {
	if Check(data) != nil {
		return ErrSensitive
	}
	var v any
	if json.Unmarshal(data, &v) != nil {
		return ErrSensitive
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				if k == "raw" {
					if s, ok := v.(string); ok {
						b, e := base64.StdEncoding.DecodeString(s)
						if e != nil || Check(b) != nil {
							return false
						}
					}
				}
				if !walk(v) {
					return false
				}
			}
		case []any:
			for _, v := range x {
				if !walk(v) {
					return false
				}
			}
		}
		return true
	}
	if !walk(v) {
		return ErrSensitive
	}
	return nil
}
