//go:build darwin

package repair

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlatformRepairPathCaseInsensitiveMatchesDirectoryLookup(t *testing.T) {
	root := t.TempDir()
	exact := filepath.Join(root, "CaseProbe")
	if err := os.WriteFile(exact, []byte("probe"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, lookupErr := os.Stat(filepath.Join(root, "caseprobe"))
	want := lookupErr == nil
	if got := platformRepairPathCaseInsensitive(exact); got != want {
		t.Fatalf("case-insensitive detection = %v, directory lookup = %v", got, want)
	}
}
