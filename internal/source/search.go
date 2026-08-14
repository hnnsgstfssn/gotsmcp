package source

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	ts "github.com/odvcencio/gotreesitter"
)

// MatchMode selects how a search pattern is compared against symbol names.
type MatchMode string

const (
	// MatchSubstring is case-insensitive containment, the default.
	MatchSubstring MatchMode = "substring"
	// MatchExact requires the whole name to be equal.
	MatchExact MatchMode = "exact"
	// MatchRegex applies a Go regular expression.
	MatchRegex MatchMode = "regex"
	// MatchFuzzy matches the pattern as a subsequence, so "cfgldr" finds
	// ConfigLoader.
	MatchFuzzy MatchMode = "fuzzy"
)

// Symbol is a declaration found by name.
type Symbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Recv      string `json:"recv,omitempty"`
	Package   string `json:"package,omitempty"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Col       int    `json:"col"`
	Signature string `json:"signature,omitempty"`
	Exported  bool   `json:"exported"`
	score     int
}

// SearchOptions filters and bounds a symbol search.
type SearchOptions struct {
	Mode MatchMode
	// Kinds restricts results to these kinds: func, method, type, const, var,
	// field. Empty means all.
	Kinds []string
	// ExportedOnly drops unexported declarations.
	ExportedOnly bool
	// IncludeFields searches struct field and interface method names too, which
	// is off by default because it roughly triples the result set.
	IncludeFields bool
	MaxResults    int
}

// SearchResult is the outcome of a symbol search.
type SearchResult struct {
	Symbols   []Symbol `json:"symbols"`
	Total     int      `json:"total"`
	FilesSeen int      `json:"files_seen"`
	Truncated bool     `json:"truncated"`
	Hint      string   `json:"hint,omitempty"`
}

// A broad pattern over a large repository can return enough rows to blow a
// client's response limit, which is a worse failure than truncating: the agent
// gets nothing. Bound both the row count and the total size.
const (
	defaultMaxResults = 100
	maxSignature      = 90
	maxResultBytes    = 60_000
)

// declQuery captures every named declaration a Go file can hold. Field and
// interface-method names are captured separately so they can be filtered out.
const declQuery = `
(function_declaration name: (identifier) @func)
(method_declaration name: (field_identifier) @method)
(type_spec name: (type_identifier) @type)
(const_spec name: (identifier) @const)
(var_spec name: (identifier) @var)
(field_declaration name: (field_identifier) @field)
(method_elem name: (field_identifier) @field)
`

// Search finds declarations whose name matches a pattern.
//
// This answers "where is Foo defined?" without knowing its package, which is
// the search an agent reaches for first. It is deliberately tree-sitter based
// rather than type-checked: it is a name lookup, so it should be fast and
// should still work on a repository that does not compile. Follow it with
// go_symbol or go_refs when precision matters.
func (l *Loader) Search(ctx context.Context, pkgs []Package, pattern string, opt SearchOptions) (*SearchResult, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, fmt.Errorf("search pattern is empty")
	}
	if opt.Mode == "" {
		opt.Mode = MatchSubstring
	}
	if opt.MaxResults <= 0 {
		opt.MaxResults = defaultMaxResults
	}
	match, err := matcher(pattern, opt.Mode)
	if err != nil {
		return nil, err
	}
	q, err := l.Compile(declQuery)
	if err != nil {
		return nil, err
	}
	wantKind := func(k string) bool {
		return len(opt.Kinds) == 0 || slices.Contains(opt.Kinds, k)
	}

	files := Files(pkgs)
	pkgOf := make(map[string]string, len(files))
	for _, p := range pkgs {
		for _, f := range p.Files {
			pkgOf[f] = cmpOrStr(p.ImportPath, p.Name)
		}
	}

	perFile, err := l.eachFile(ctx, files, func(f *File) ([]Symbol, error) {
		var found []Symbol
		for _, m := range q.Execute(f.Tree) {
			for _, c := range m.Captures {
				if c.Node == nil {
					continue
				}
				kind := c.Name
				if kind == "field" && !opt.IncludeFields {
					continue
				}
				if !wantKind(kind) {
					continue
				}
				name := c.Node.Text(f.Src)
				score, ok := match(name)
				if !ok {
					continue
				}
				exported := isExported(name)
				if opt.ExportedOnly && !exported {
					continue
				}
				p := c.Node.StartPoint()
				found = append(found, Symbol{
					Name:      name,
					Kind:      kind,
					Recv:      receiverOf(c.Node, f, l.lang),
					Package:   pkgOf[f.Path],
					File:      l.Rel(f.Path),
					Line:      int(p.Row) + 1,
					Col:       int(p.Column) + 1,
					Signature: signatureOf(c.Node, f, l.lang),
					Exported:  exported,
					score:     score,
				})
			}
		}
		return found, nil
	})
	if err != nil {
		return nil, err
	}

	res := &SearchResult{FilesSeen: len(files)}
	for _, syms := range perFile {
		res.Symbols = append(res.Symbols, syms...)
	}
	res.Total = len(res.Symbols)

	// Best score first, then a stable path so repeated calls agree.
	slices.SortStableFunc(res.Symbols, func(a, b Symbol) int {
		if a.score != b.score {
			return b.score - a.score
		}
		if c := strings.Compare(a.File, b.File); c != 0 {
			return c
		}
		return a.Line - b.Line
	})
	if len(res.Symbols) > opt.MaxResults {
		res.Symbols = res.Symbols[:opt.MaxResults]
		res.Truncated = true
	}
	budget := maxResultBytes
	for i := range res.Symbols {
		if len(res.Symbols[i].Signature) > maxSignature {
			res.Symbols[i].Signature = res.Symbols[i].Signature[:maxSignature] + "..."
		}
		budget -= len(res.Symbols[i].Signature) + len(res.Symbols[i].File) + 64
		if budget <= 0 {
			res.Symbols = res.Symbols[:i]
			res.Truncated = true
			break
		}
	}
	if res.Truncated {
		res.Hint = fmt.Sprintf(
			"%d of %d shown; narrow with selector, kinds, exported_only, or a longer pattern",
			len(res.Symbols), res.Total)
	}
	return res, nil
}

// matcher returns a name predicate and a relevance score. Higher is better.
func matcher(pattern string, mode MatchMode) (func(string) (int, bool), error) {
	switch mode {
	case MatchExact:
		return func(name string) (int, bool) {
			if name == pattern {
				return 1000, true
			}
			return 0, false
		}, nil

	case MatchRegex:
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compile regex %q: %w", pattern, err)
		}
		return func(name string) (int, bool) {
			if !re.MatchString(name) {
				return 0, false
			}
			return 500 - len(name), true
		}, nil

	case MatchFuzzy:
		return func(name string) (int, bool) { return fuzzyScore(pattern, name) }, nil

	case MatchSubstring:
		lower := strings.ToLower(pattern)
		return func(name string) (int, bool) {
			i := strings.Index(strings.ToLower(name), lower)
			if i < 0 {
				return 0, false
			}
			// Prefer exact, then prefix, then the shortest container.
			switch {
			case name == pattern:
				return 1000, true
			case i == 0:
				return 800 - len(name), true
			default:
				return 600 - len(name) - i, true
			}
		}, nil
	}
	return nil, fmt.Errorf("unknown match mode %q; use substring, exact, regex, or fuzzy", mode)
}

// fuzzyScore matches pattern as a case-insensitive subsequence of name,
// rewarding matches that land on word boundaries so "cfgldr" ranks
// ConfigLoader above a coincidental scattering of the same letters.
func fuzzyScore(pattern, name string) (int, bool) {
	p := []rune(strings.ToLower(pattern))
	n := []rune(name)
	if len(p) == 0 || len(p) > len(n) {
		return 0, false
	}

	score, pi := 0, 0
	prevMatched := false
	for i := 0; i < len(n) && pi < len(p); i++ {
		if unicode.ToLower(n[i]) != p[pi] {
			prevMatched = false
			continue
		}
		score += 10
		if i == 0 {
			score += 40 // matches the very start
		} else if unicode.IsUpper(n[i]) || n[i-1] == '_' {
			score += 25 // start of a camelCase or snake_case word
		}
		if prevMatched {
			score += 15 // consecutive run
		}
		prevMatched = true
		pi++
	}
	if pi != len(p) {
		return 0, false
	}
	return score - len(n), true // shorter names win ties
}

// receiverOf reports the receiver type for a method declaration.
func receiverOf(n *ts.Node, f *File, lang *ts.Language) string {
	for cur := n.Parent(); cur != nil; cur = cur.Parent() {
		if cur.Type(lang) != "method_declaration" {
			continue
		}
		if recv := cur.ChildByFieldName("receiver", lang); recv != nil {
			return strings.Trim(recv.Text(f.Src), "()")
		}
		return ""
	}
	return ""
}

// signatureOf renders a one-line summary of the declaration containing n.
func signatureOf(n *ts.Node, f *File, lang *ts.Language) string {
	for cur := n.Parent(); cur != nil; cur = cur.Parent() {
		switch cur.Type(lang) {
		case "function_declaration", "method_declaration":
			end := cur.EndByte()
			if body := cur.ChildByFieldName("body", lang); body != nil {
				end = body.StartByte()
			}
			return oneLine(string(f.Src[cur.StartByte():end]))
		case "type_spec", "const_spec", "var_spec", "field_declaration", "method_elem":
			return oneLine(cur.Text(f.Src))
		case "source_file":
			return ""
		}
	}
	return ""
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}

func isExported(name string) bool {
	for _, r := range name {
		return unicode.IsUpper(r)
	}
	return false
}

func cmpOrStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
