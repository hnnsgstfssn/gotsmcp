package main_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestBinaryServesOverStdio builds the real gotsmcp binary and drives it
// through its actual entry point: flag parsing, server construction, and the
// stdio transport. The unit tests in internal/tools cover every tool's
// behaviour through an in-memory transport, which never exercises main, run,
// or the command-line wiring a user's editor actually launches.
func TestBinaryServesOverStdio(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "gotsmcp")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	root, err := filepath.Abs(filepath.Join("..", "..", "testdata", "proj"))
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	cmd := exec.Command(bin, "-default-root", root)
	client := mcp.NewClient(&mcp.Implementation{Name: "smoke-test", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to subprocess: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "go_workspace", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool(go_workspace): %v", err)
	}
	if res.IsError {
		t.Fatalf("go_workspace returned an error: %+v", res.Content)
	}
}
