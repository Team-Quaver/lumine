package config

import (
	"fmt"
	"strings"
)

// ini is a parsed INI document: an ordered list of sections, each holding an
// ordered list of key/value pairs. Repeated keys are preserved rather than
// collapsed into a map, because the configuration relies on it — [auto-start]
// is a list of exec-once lines, and a section may legitimately repeat a key.
// Lookups therefore scan the pairs in order.
type ini struct {
	sections []*iniSection
}

type iniSection struct {
	name  string
	pairs []iniPair
}

type iniPair struct {
	key   string // lower-cased, for case-insensitive matching
	raw   string // original key spelling, preserved for env vars
	value string
	line  int
}

// parseINI parses a Lumine .conf document. It is deliberately small and
// forgiving, matching how hand-written window-manager configs behave:
//
//   - blank lines are ignored;
//   - '#' and ';' start a comment that runs to end of line, unless the marker
//     is inside a quoted value (so icc-file = "/x#y.icc" keeps the '#');
//   - a section header is "[Name]" and its name is matched case-insensitively;
//   - a key/value line is "key = value"; the "= value" part is optional, so a
//     bare flag ("triple-buffering") parses as an empty value;
//   - keys are matched case-insensitively (refresh-Rate == refresh-rate) but
//     values keep their case;
//   - values may be wrapped in ASCII or typographic quotes; quotes are
//     stripped, which makes the examples in the manual work verbatim.
func parseINI(data string) (*ini, error) {
	doc := &ini{}
	var cur *iniSection
	for i, raw := range strings.Split(data, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(stripINIComment(raw))
		if line == "" {
			continue
		}

		// Section header.
		if strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				return nil, fmt.Errorf("line %d: unterminated section header %q", lineNo, line)
			}
			name := strings.TrimSpace(line[1:end])
			if name == "" {
				return nil, fmt.Errorf("line %d: empty section name", lineNo)
			}
			cur = &iniSection{name: name}
			doc.sections = append(doc.sections, cur)
			continue
		}

		key, value := line, ""
		if eq := strings.IndexByte(line, '='); eq >= 0 {
			key, value = line[:eq], line[eq+1:]
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("line %d: missing key in %q", lineNo, line)
		}
		if cur == nil {
			// Keys before any section land in an unnamed global section.
			cur = &iniSection{}
			doc.sections = append(doc.sections, cur)
		}
		rawKey := unquoteINI(key)
		cur.pairs = append(cur.pairs, iniPair{
			key:   strings.ToLower(rawKey),
			raw:   rawKey,
			value: unquoteINI(strings.TrimSpace(value)),
			line:  lineNo,
		})
	}
	return doc, nil
}

// stripINIComment removes an unquoted '#' or ';' comment.
func stripINIComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' || c == ';':
			return line[:i]
		}
	}
	return line
}

// unquoteINI strips a single pair of wrapping quotes, including the
// typographic quotes (“ ” ‘ ’) that sneak in from copy-pasted examples.
func unquoteINI(v string) string {
	if len(v) < 2 {
		return v
	}
	pairs := [][2]string{
		{`"`, `"`}, {`'`, `'`},
		{"\u201c", "\u201d"}, {"\u2018", "\u2019"},
	}
	for _, p := range pairs {
		if strings.HasPrefix(v, p[0]) && strings.HasSuffix(v, p[1]) {
			return v[len(p[0]) : len(v)-len(p[1])]
		}
	}
	return v
}

// section returns the first section whose name matches, case-insensitively.
func (d *ini) section(name string) *iniSection {
	for _, s := range d.sections {
		if strings.EqualFold(s.name, name) {
			return s
		}
	}
	return nil
}

// sectionPrefix returns every section whose name starts with prefix
// (case-insensitive), in file order. This is how Display-1 / Display-2 are
// collected while keeping their priority ordering.
func (d *ini) sectionPrefix(prefix string) []*iniSection {
	var out []*iniSection
	for _, s := range d.sections {
		if len(s.name) >= len(prefix) && strings.EqualFold(s.name[:len(prefix)], prefix) {
			out = append(out, s)
		}
	}
	return out
}

// get returns the first value for key, or ("", false).
func (s *iniSection) get(key string) (string, bool) {
	key = strings.ToLower(key)
	for _, p := range s.pairs {
		if p.key == key {
			return p.value, true
		}
	}
	return "", false
}

// all returns every value for key, in order. Repeated keys are legal.
func (s *iniSection) all(key string) []string {
	key = strings.ToLower(key)
	var out []string
	for _, p := range s.pairs {
		if p.key == key {
			out = append(out, p.value)
		}
	}
	return out
}

// first returns the first of the given aliases that is present. Aliases let a
// section accept both the canonical spelling and common variants (for example
// color-management and the color-managment typo from the manual).
func (s *iniSection) first(keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := s.get(k); ok {
			return v, true
		}
	}
	return "", false
}
