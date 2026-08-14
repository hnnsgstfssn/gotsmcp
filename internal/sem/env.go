package sem

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Toolchain describes whether the Go tooling can actually run here.
type Toolchain struct {
	Go         string `json:"go,omitempty"`
	Version    string `json:"version,omitempty"`
	Cache      string `json:"gocache,omitempty"`
	ModCache   string `json:"gomodcache,omitempty"`
	CanTypeChk bool   `json:"can_type_check"`
	Cause      string `json:"cause,omitempty"`
	Remedy     string `json:"remedy,omitempty"`
}

// InspectToolchain reports whether the semantic tools can work in this process.
//
// The syntactic tools need nothing but the files, so a broken toolchain leaves
// half the server working and half failing, which reads as a bug in the failing
// half. It is worth a subprocess on the error path to say which it is: the
// difference between "your selector is wrong" and "this process cannot reach
// the build cache" is the difference between a two-minute fix and a long hunt.
func InspectToolchain(ctx context.Context, root string) Toolchain {
	t := Toolchain{CanTypeChk: true}

	path, err := exec.LookPath("go")
	if err != nil {
		t.CanTypeChk = false
		t.Cause = "the go command is not on PATH"
		t.Remedy = "start the server with a PATH that includes the Go toolchain"
		return t
	}
	t.Go = path

	env := goEnv(ctx, root, "GOVERSION", "GOCACHE", "GOMODCACHE")
	t.Version, t.Cache, t.ModCache = env["GOVERSION"], env["GOCACHE"], env["GOMODCACHE"]

	switch {
	case t.Cache == "off":
		t.CanTypeChk = false
		t.Cause = "GOCACHE is off, so the go command refuses to build or list packages"
		t.Remedy = "unset GOCACHE or point it at a writable directory in the environment this server is launched with"
		return t

	case t.Cache != "" && !writable(t.Cache):
		t.CanTypeChk = false
		t.Cause = fmt.Sprintf("the build cache at %s is not writable by this process", t.Cache)
		t.Remedy = "launch the server with the same filesystem permissions as the Go toolchain, or set GOCACHE to a writable directory"
		return t
	}

	// Last resort: ask the go command itself and repeat what it says.
	if out, err := runGo(ctx, root, "list", "-e", "./..."); err != nil {
		t.CanTypeChk = false
		t.Cause = "the go command failed in this directory"
		t.Remedy = "run the same command by hand to see the full output"
		if s := firstLine(out); s != "" {
			t.Cause += ": " + s
		}
	}
	return t
}

func goEnv(ctx context.Context, dir string, keys ...string) map[string]string {
	out := make(map[string]string, len(keys))
	res, err := runGo(ctx, dir, append([]string{"env"}, keys...)...)
	if err != nil {
		return out
	}
	lines := strings.Split(strings.TrimSpace(res), "\n")
	for i, k := range keys {
		if i < len(lines) {
			out[k] = strings.TrimSpace(lines[i])
		}
	}
	return out
}

func runGo(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// writable reports whether a directory can be created in and written to.
func writable(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".probe*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
