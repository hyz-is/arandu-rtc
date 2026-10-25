package unit_test

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The module is imported into other people's builds, and four things about it have to stay true
// without anybody remembering them: it needs no framework, it has one dependency, it runs no
// program and touches no file, and its manifest says what it does.

func productionImports(t *testing.T) map[string][]string {
	t.Helper()
	root := packageRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range f.Imports {
			out[filepath.Base(path)] = append(out[filepath.Base(path)], strings.Trim(spec.Path.Value, `"`))
		}
	}
	if len(out) == 0 {
		t.Fatal("no production file found")
	}
	return out
}

// TestTheModuleNeedsNoFramework: the hub, the container reader and the pass are plain Go, so an
// application of any Framework version takes them and nothing else moves in its go.mod.
func TestTheModuleNeedsNoFramework(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Dir = packageRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.HasPrefix(dep, "github.com/arandu-io/") {
			t.Errorf("the module depends on %s: it must stay framework-free", dep)
		}
	}
}

// TestTheOnlyDependencyIsTheWebSocketLibrary: another dependency is an argument first.
func TestTheOnlyDependencyIsTheWebSocketLibrary(t *testing.T) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{if and (not .Indirect) (not .Main)}}{{.Path}}{{end}}", "all")
	cmd.Dir = packageRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -m: %v\n%s", err, out)
	}
	got := strings.Fields(string(out))
	if len(got) != 1 || got[0] != "github.com/coder/websocket" {
		t.Errorf("direct dependencies = %v, want only github.com/coder/websocket", got)
	}
}

// TestTheManifestSaysWhatTheCodeDoes reads the imports, which are what the capability audit
// compares: the module serves sockets, so it declares the network, and it runs nothing and opens
// no file.
func TestTheManifestSaysWhatTheCodeDoes(t *testing.T) {
	manifest := readReleaseFile(t, packageRoot(t), "arandu.mod.toml")
	declared := func(key string) bool {
		return strings.Contains(manifest, "\n"+key+" = true")
	}
	imports := productionImports(t)
	uses := func(pkg string) bool {
		for _, list := range imports {
			for _, p := range list {
				if p == pkg {
					return true
				}
			}
		}
		return false
	}
	if uses("net/http") && !declared("network") {
		t.Error("the module serves HTTP and arandu.mod.toml does not declare network = true")
	}
	if !declared("network") {
		t.Error("the module answers browsers' sockets: network must be true")
	}
	for _, pkg := range []string{"os/exec"} {
		if uses(pkg) {
			t.Errorf("the module imports %s, which this package does not do", pkg)
		}
	}
	for _, pkg := range []string{"os", "io/ioutil", "io/fs", "path/filepath"} {
		if uses(pkg) {
			t.Errorf("the module imports %s: it keeps nothing outside memory, and its manifest says filesystem = false", pkg)
		}
	}
	if declared("filesystem") || declared("exec") || declared("migrations") {
		t.Error("arandu.mod.toml declares filesystem, exec or migrations as true")
	}
}

func TestEveryFixtureIsReadable(t *testing.T) {
	root := packageRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "testdata"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("testdata: %v (%d entries)", err, len(entries))
	}
}
