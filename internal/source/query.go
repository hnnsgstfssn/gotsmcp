package source

import (
	"context"
	"fmt"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// Capture is one named node captured by a query pattern.
type Capture struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Line      int    `json:"line"`
	Col       int    `json:"col"`
	EndLine   int    `json:"end_line"`
	EndCol    int    `json:"end_col"`
	StartByte uint32 `json:"start_byte"`
	EndByte   uint32 `json:"end_byte"`
	Text      string `json:"text"`
}

// Match is one query pattern matched at one site.
type Match struct {
	File     string    `json:"file"`
	Line     int       `json:"line"`
	Pattern  int       `json:"pattern"`
	Captures []Capture `json:"captures"`
}

// QueryOptions controls query execution.
type QueryOptions struct {
	// MaxMatches caps returned matches. Zero applies defaultMaxMatches.
	MaxMatches int
	// CountOnly skips materialising matches, for checking that a query hits
	// what the author expects before rewriting with it.
	CountOnly bool
	// MaxTextBytes truncates captured text. Zero applies defaultCaptureText.
	MaxTextBytes int
}

// QueryResult is the outcome of running one query across a file set.
type QueryResult struct {
	Matches     []Match  `json:"matches,omitempty"`
	Total       int      `json:"total"`
	FilesSeen   int      `json:"files_seen"`
	FilesHit    int      `json:"files_hit"`
	Truncated   bool     `json:"truncated"`
	ParseErrors []string `json:"parse_errors,omitempty"`
}

// A project-wide query on a real 826-file repository returns about 6000
// function declarations, so any cap truncates; Total stays honest either way.
// 1000 matches at 300 bytes of captured text is roughly 300 KB.
const (
	defaultMaxMatches  = 1000
	defaultCaptureText = 300
)

// Compile compiles and caches a tree-sitter query. Compiled queries are safe
// for concurrent execution, so sharing them across calls is sound.
func (l *Loader) Compile(src string) (*ts.Query, error) {
	l.qmu.Lock()
	if q, ok := l.queries[src]; ok {
		l.qmu.Unlock()
		return q, nil
	}
	l.qmu.Unlock()

	q, err := ts.NewQuery(src, l.lang)
	if err != nil {
		return nil, fmt.Errorf("compile query: %w", err)
	}

	l.qmu.Lock()
	defer l.qmu.Unlock()
	if len(l.queries) > maxCachedQueries {
		clear(l.queries)
	}
	l.queries[src] = q
	return q, nil
}

const maxCachedQueries = 64

// Query runs a tree-sitter query over every file in pkgs.
func (l *Loader) Query(ctx context.Context, pkgs []Package, src string, opt QueryOptions) (*QueryResult, error) {
	q, err := l.Compile(src)
	if err != nil {
		return nil, err
	}
	if opt.MaxMatches <= 0 {
		opt.MaxMatches = defaultMaxMatches
	}
	if opt.MaxTextBytes <= 0 {
		opt.MaxTextBytes = defaultCaptureText
	}

	// Per-file query execution is independent, so parse and match in parallel
	// and reassemble in file order. Compiled queries are safe for concurrent
	// Execute; each worker gets its own parser from the pool.
	files := Files(pkgs)
	type fileHits struct {
		count   int
		matches []Match
	}
	hits, fileErrs, err := mapFiles(ctx, l, files, func(f *File) (fileHits, error) {
		matches := q.Execute(f.Tree)
		out := fileHits{count: len(matches)}
		if opt.CountOnly || len(matches) == 0 {
			return out, nil
		}
		rel := l.Rel(f.Path)
		out.matches = make([]Match, 0, len(matches))
		for _, m := range matches {
			out.matches = append(out.matches, toMatch(rel, f, l.lang, m, opt.MaxTextBytes))
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}

	res := &QueryResult{}
	for i, h := range hits {
		if fileErrs[i] != nil {
			res.ParseErrors = append(res.ParseErrors, fileErrs[i].Error())
			continue
		}
		res.FilesSeen++
		if h.count == 0 {
			continue
		}
		res.FilesHit++
		res.Total += h.count
		for _, m := range h.matches {
			if len(res.Matches) >= opt.MaxMatches {
				res.Truncated = true
				break
			}
			res.Matches = append(res.Matches, m)
		}
	}
	return res, nil
}

func toMatch(rel string, f *File, lang *ts.Language, m ts.QueryMatch, maxText int) Match {
	out := Match{File: rel, Pattern: m.PatternIndex, Captures: make([]Capture, 0, len(m.Captures))}
	for i, c := range m.Captures {
		if c.Node == nil {
			continue
		}
		s, e := c.Node.StartPoint(), c.Node.EndPoint()
		if i == 0 {
			out.Line = int(s.Row) + 1
		}
		text := c.Text(f.Src)
		if len(text) > maxText {
			text = text[:maxText] + "..."
		}
		out.Captures = append(out.Captures, Capture{
			Name:      c.Name,
			Kind:      c.Node.Type(lang),
			Line:      int(s.Row) + 1,
			Col:       int(s.Column) + 1,
			EndLine:   int(e.Row) + 1,
			EndCol:    int(e.Column) + 1,
			StartByte: c.Node.StartByte(),
			EndByte:   c.Node.EndByte(),
			Text:      text,
		})
	}
	return out
}

// Lines returns the 1-based inclusive line range of src as a string.
func Lines(src []byte, start, end int) string {
	all := strings.Split(string(src), "\n")
	if start < 1 {
		start = 1
	}
	if end > len(all) {
		end = len(all)
	}
	if start > end {
		return ""
	}
	return strings.Join(all[start-1:end], "\n")
}
