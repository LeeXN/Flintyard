// Package contract validates the project's embedded v1alpha1 JSON contracts.
package contract

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

//go:embed *schema.json
var schemas embed.FS

const MaxConfig = 1 << 20

// Decode rejects duplicate keys, invalid UTF-8, trailing JSON and oversized input.
func Decode(b []byte, dst any) error {
	return DecodeLimit(b, dst, MaxConfig)
}

// DecodeLimit is for bounded internal state (journals can exceed a user config
// with 128 nodes). Public configuration decoding keeps the 1 MiB default.
func DecodeLimit(b []byte, dst any, limit int) error {
	if limit < 1 || len(b) > limit || !utf8.Valid(b) {
		return fmt.Errorf("invalid UTF-8 or JSON exceeds %d bytes", limit)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := walk(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}
func walk(d *json.Decoder, depth int) error {
	if depth > 128 {
		return fmt.Errorf("JSON nesting exceeds 128 levels")
	}
	t, e := d.Token()
	if e != nil {
		return e
	}
	if v, ok := t.(json.Delim); ok {
		switch v {
		case '{':
			keys := map[string]bool{}
			for d.More() {
				t, e = d.Token()
				if e != nil {
					return e
				}
				k, ok := t.(string)
				if !ok {
					return fmt.Errorf("object key expected")
				}
				if keys[k] {
					return fmt.Errorf("duplicate key %q", k)
				}
				keys[k] = true
				if e = walk(d, depth+1); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e = walk(d, depth+1); e != nil {
					return e
				}
			}
		default:
			return fmt.Errorf("unexpected delimiter")
		}
		_, e = d.Token()
		return e
	}
	return nil
}
func Read(r io.Reader, dst any) error {
	b, e := io.ReadAll(io.LimitReader(r, MaxConfig+1))
	if e != nil {
		return e
	}
	return Decode(b, dst)
}
func Validate(kind string, b []byte) error {
	var v any
	if e := Decode(b, &v); e != nil {
		return e
	}
	raw, e := schemas.ReadFile(kind + "-v1.schema.json")
	if e != nil {
		return e
	}
	var s map[string]any
	if e = json.Unmarshal(raw, &s); e != nil {
		return e
	}
	return check(s, v, "$")
}
func check(s map[string]any, v any, p string) error {
	fail := func(m string) error { return fmt.Errorf("%s: %s", p, m) }
	if x, ok := s["const"]; ok && !equal(x, v) {
		return fail("incorrect constant")
	}
	if xs, ok := s["enum"].([]any); ok {
		found := false
		for _, x := range xs {
			found = found || equal(x, v)
		}
		if !found {
			return fail("value outside enum")
		}
	}
	if xs, ok := s["oneOf"].([]any); ok {
		n := 0
		for _, x := range xs {
			if check(x.(map[string]any), v, p) == nil {
				n++
			}
		}
		if n != 1 {
			return fail("must match exactly one alternative")
		}
	}
	if x, ok := s["not"].(map[string]any); ok && check(x, v, p) == nil {
		return fail("forbidden field combination")
	}
	if types, ok := s["type"].([]any); ok {
		matched := false
		for _, typ := range types {
			sub := map[string]any{"type": typ}
			if check(sub, v, p) == nil {
				matched = true
			}
		}
		if !matched {
			return fail("incorrect union type")
		}
	}
	switch s["type"] {
	case "null":
		if v != nil {
			return fail("expected null")
		}
	case "object":
		if _, ok := v.(map[string]any); !ok {
			return fail("expected object")
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return fail("expected array")
		}
	case "string":
		if _, ok := v.(string); !ok {
			return fail("expected string")
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fail("expected boolean")
		}
	case "integer":
		if n, ok := v.(float64); !ok || n != float64(int64(n)) {
			return fail("expected integer")
		}
	}
	if m, ok := v.(map[string]any); ok {
		if req, ok := s["required"].([]any); ok {
			for _, k := range req {
				if _, ok := m[k.(string)]; !ok {
					return fail("missing " + k.(string))
				}
			}
		}
		props, _ := s["properties"].(map[string]any)
		for k, v := range m {
			if x, ok := props[k]; ok {
				if e := check(x.(map[string]any), v, p+"."+k); e != nil {
					return e
				}
			} else if a, ok := s["additionalProperties"]; ok {
				if a == false {
					return fail("unknown field " + k)
				}
				if x, ok := a.(map[string]any); ok {
					if e := check(x, v, p+"."+k); e != nil {
						return e
					}
				}
			}
		}
	}
	if a, ok := v.([]any); ok {
		if !bounds(s, "minItems", "maxItems", float64(len(a))) {
			return fail("array size outside bounds")
		}
		if x, ok := s["items"].(map[string]any); ok {
			for i, v := range a {
				if e := check(x, v, fmt.Sprintf("%s[%d]", p, i)); e != nil {
					return e
				}
			}
		}
	}
	if t, ok := v.(string); ok {
		if !bounds(s, "minLength", "maxLength", float64(utf8.RuneCountInString(t))) {
			return fail("string size outside bounds")
		}
		if pat, ok := s["pattern"].(string); ok {
			match, e := regexp.MatchString(pat, t)
			if e != nil || !match {
				return fail("pattern mismatch")
			}
		}
		switch s["format"] {
		case "ipv4":
			a, e := netip.ParseAddr(t)
			if e != nil || !a.Is4() {
				return fail("invalid IPv4")
			}
		case "date-time":
			if _, e := time.Parse(time.RFC3339, t); e != nil {
				return fail("invalid timestamp")
			}
		}
	}
	if n, ok := v.(float64); ok && !bounds(s, "minimum", "maximum", n) {
		return fail("number outside bounds")
	}
	return nil
}
func equal(a, b any) bool { x, _ := json.Marshal(a); y, _ := json.Marshal(b); return bytes.Equal(x, y) }
func bounds(s map[string]any, min, max string, n float64) bool {
	if x, ok := s[min].(float64); ok && n < x {
		return false
	}
	if x, ok := s[max].(float64); ok && n > x {
		return false
	}
	return true
}
func Path(p string) error {
	if p == "" || len(p) > 4096 || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") {
		return fmt.Errorf("unsafe archive path %q", p)
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." {
			return fmt.Errorf("unsafe archive path %q", p)
		}
	}
	return nil
}
