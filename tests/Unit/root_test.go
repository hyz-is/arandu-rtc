package unit_test

import (
	"os"
	"path/filepath"
	"testing"
)

// packageRoot is the directory holding the package's go.mod, found by walking
// up from the test's working directory.
func packageRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = parent
	}
}
