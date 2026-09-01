package project

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

var testTime = time.Unix(100, 0)

func writeFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func fakeGit(t *testing.T, listedPath, failingProbe string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "git")
	script := "#!/bin/sh\ncase \"$3\" in\nls-files) printf '" + listedPath + "\\000' ;;\nrev-parse) [ \"" + failingProbe + "\" = rev-parse ] && exit 1; printf 'head\\n' ;;\nstatus) [ \"" + failingProbe + "\" = status ] && exit 1 ;;\ndiff) [ \"" + failingProbe + "\" = diff ] && exit 1 ;;\n*) exit 1 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func filePaths(files []File) []string {
	paths := make([]string, len(files))
	for i, file := range files {
		paths[i] = file.Path
	}
	sort.Strings(paths)
	return paths
}
