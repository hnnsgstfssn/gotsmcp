package rewrite

import (
	"fmt"
	"strings"
)

// ErrUnknownCapture reports a template referring to a capture the query never bound.
const ErrUnknownCapture Error = "template references an unknown capture"

// Expand substitutes ${name} references in tmpl with captured node text.
// A literal dollar sign is written $$.
//
// An unresolved reference is an error rather than an empty string. Silently
// expanding ${arsg} to nothing is exactly the class of bug that makes bulk sed
// edits dangerous, and it is cheap to refuse instead.
func Expand(tmpl string, caps map[string]string) (string, error) {
	var b strings.Builder
	b.Grow(len(tmpl))

	for i := 0; i < len(tmpl); {
		c := tmpl[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 < len(tmpl) && tmpl[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		if i+1 >= len(tmpl) || tmpl[i+1] != '{' {
			return "", fmt.Errorf("template offset %d: bare $ must be written $$ or ${name}", i)
		}
		end := strings.IndexByte(tmpl[i+2:], '}')
		if end < 0 {
			return "", fmt.Errorf("template offset %d: unterminated ${", i)
		}
		name := tmpl[i+2 : i+2+end]
		val, ok := caps[name]
		if !ok {
			return "", fmt.Errorf("%w: ${%s}; query bound %s", ErrUnknownCapture, name, captureNames(caps))
		}
		b.WriteString(val)
		i += 2 + end + 1
	}
	return b.String(), nil
}

func captureNames(caps map[string]string) string {
	if len(caps) == 0 {
		return "no captures"
	}
	names := make([]string, 0, len(caps))
	for k := range caps {
		names = append(names, "@"+k)
	}
	return strings.Join(names, ", ")
}
