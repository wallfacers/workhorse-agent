package prompt_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageBoundary enforces that the prompt package stays a pure
// rendering layer — no IO, no project-internal imports, and no signature
// drift on the public surface. Tracked as add-memory-l1-l2 §4.5; the
// reason a CI-level grep guard exists at all is that the memory subsystem
// deliberately pushed all IO concerns OUT of this package, and a future
// refactor could silently undo that.
func TestPackageBoundary(t *testing.T) {
	pkgDir := "."
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	allowedImports := map[string]bool{
		"bytes":         true,
		"fmt":           true,
		"runtime":       true, // DetectOS uses runtime.GOOS
		"sort":          true, // stable sort for byte-stable output
		"strings":       true,
		"text/template": true,
	}

	var buildSystemPromptFound bool
	fset := token.NewFileSet()

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(pkgDir, name)
		af, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		for _, imp := range af.Imports {
			ip := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(ip, "github.com/wallfacers/workhorse-agent/") {
				t.Errorf("%s imports project-internal package %q — prompt must stay IO-free; "+
					"if you need data from elsewhere, the call site should compute it and pass it in",
					name, ip)
				continue
			}
			if !allowedImports[ip] {
				t.Errorf("%s imports %q which is not on the allowed list %v — "+
					"if this import is intentional, update the test's allowlist with a justification",
					name, ip, sortedKeys(allowedImports))
			}
		}

		for _, decl := range af.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name == nil {
				continue
			}
			if fn.Name.Name != "BuildSystemPrompt" {
				continue
			}
			buildSystemPromptFound = true
			assertBuildSystemPromptSignature(t, fn)
		}
	}

	if !buildSystemPromptFound {
		t.Fatal("BuildSystemPrompt function not found in package — it is part of the load-bearing " +
			"public surface and must not be renamed or removed without a deliberate spec change")
	}
}

// assertBuildSystemPromptSignature pins the signature to
// `func(SystemPromptInput) string`. The struct is the single structured input
// for the converged assembly path (optimize-prompt-cache-order): callers hand
// the prompt package the three raw segments and it owns ordering + delimiters.
func assertBuildSystemPromptSignature(t *testing.T, fn *ast.FuncDecl) {
	t.Helper()
	params := fn.Type.Params.List
	if len(params) != 1 || len(params[0].Names) != 1 {
		t.Errorf("BuildSystemPrompt should have exactly one parameter, got %d field(s)", len(params))
		return
	}
	if id, ok := params[0].Type.(*ast.Ident); !ok || id.Name != "SystemPromptInput" {
		t.Errorf("BuildSystemPrompt parameter type drifted from SystemPromptInput")
	}

	results := fn.Type.Results
	if results == nil || len(results.List) != 1 || len(results.List[0].Names) > 0 {
		t.Errorf("BuildSystemPrompt should have exactly one anonymous return value")
		return
	}
	if id, ok := results.List[0].Type.(*ast.Ident); !ok || id.Name != "string" {
		t.Errorf("BuildSystemPrompt return type drifted from string")
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
