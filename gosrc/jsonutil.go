package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// marshalJSON mirrors JS JSON.stringify closely enough for the wire:
// HTML escaping is disabled (JS does not escape <, >, &).
func marshalJSON(v interface{}) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// OMap is an insertion-ordered JSON object (GraphQL results preserve
// document field order, like JS objects).
type OMap struct {
	keys []string
	vals map[string]interface{}
}

func newOMap() *OMap { return &OMap{vals: map[string]interface{}{}} }

func (m *OMap) set(key string, value interface{}) {
	if _, ok := m.vals[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.vals[key] = value
}

func (m *OMap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	writeJSONValue(&buf, m)
	return buf.Bytes(), nil
}

func writeJSONValue(buf *bytes.Buffer, v interface{}) {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeJSONString(buf, t)
	case int:
		buf.WriteString(strconv.Itoa(t))
	case int64:
		buf.WriteString(strconv.FormatInt(t, 10))
	case float64:
		buf.WriteString(formatJSNumber(t))
	case json.Number:
		buf.WriteString(string(t))
	case *OMap:
		buf.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONString(buf, k)
			buf.WriteByte(':')
			writeJSONValue(buf, t.vals[k])
		}
		buf.WriteByte('}')
	case []interface{}:
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONValue(buf, item)
		}
		buf.WriteByte(']')
	default:
		buf.Write(marshalJSON(v))
	}
}

func writeJSONString(buf *bytes.Buffer, s string) {
	buf.Write(appendJSONString(nil, s))
}

const hexDigits = "0123456789abcdef"

// appendJSONString appends s as a JSON string literal, byte-identical to
// encoding/json with SetEscapeHTML(false) (which is also what JS
// JSON.stringify produces for the strings this forge emits): only the
// quote, backslash and control characters are escaped, U+2028/U+2029 are
// escaped, invalid UTF-8 bytes become �, everything else is raw.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		if b := s[i]; b < 0x80 {
			if b >= 0x20 && b != '"' && b != '\\' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch b {
			case '"', '\\':
				dst = append(dst, '\\', b)
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[b>>4], hexDigits[b&0x0f])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', 'f', 'f', 'f', 'd')
			i++
			start = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[r&0x0f])
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	dst = append(dst, '"')
	return dst
}

// formatJSNumber formats a float like JS JSON.stringify: integral values
// print without a fraction.
func formatJSNumber(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// jsNumber converts a JSON-ish value to a float like JS Number()/typeof
// checks. ok=false means "not a number" (NaN semantics for our uses).
func jsNumber(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

// jsString mirrors JS String(v) for the scalar values we meet.
func jsString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return formatJSNumber(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return "null"
	}
}

// parseJSNumberString mirrors JS Number(string) for path segments:
// returns NaN (ok=false) when the string is not numeric.
func parseJSNumberString(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, true // Number("") === 0
	}
	if strings.HasPrefix(t, "0x") || strings.HasPrefix(t, "0X") {
		n, err := strconv.ParseInt(t[2:], 16, 64)
		if err != nil {
			return 0, false
		}
		return float64(n), true
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// segmentNumberToInt mirrors `const n = Number(seg); Number.isInteger(n)`
// lookups: returns (value, true) only for integer-valued numerics.
func segmentNumberToInt(s string) (int, bool) {
	f, ok := parseJSNumberString(s)
	if !ok || f != math.Trunc(f) {
		return 0, false
	}
	return int(f), true
}

// lenientBase64Decode mirrors Node's Buffer.from(s, 'base64'), which
// ignores characters outside the alphabet and tolerates missing padding.
func lenientBase64Decode(s string) []byte {
	var clean strings.Builder
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/' || r == '=' {
			clean.WriteRune(r)
		}
	}
	t := clean.String()
	if out, err := base64.StdEncoding.DecodeString(t); err == nil {
		return out
	}
	if out, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(t, "=")); err == nil {
		return out
	}
	// Last resort: decode what we can with padding added.
	for len(t)%4 != 0 {
		t += "="
	}
	out, _ := base64.StdEncoding.DecodeString(t)
	return out
}

func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
