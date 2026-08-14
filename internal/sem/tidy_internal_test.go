package sem

import "testing"

func TestGuessModule(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"exactly three segments", "github.com/foo/bar", "github.com/foo/bar"},
		{"more than three segments truncates", "github.com/foo/bar/baz", "github.com/foo/bar"},
		{"deep subpackage", "golang.org/x/tools/go/packages", "golang.org/x/tools"},
		{"single segment returned as-is", "fmt", "fmt"},
		{"two segments returned as-is", "a/b", "a/b"},
		{"empty path", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := guessModule(tt.path); got != tt.want {
				t.Errorf("guessModule(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
