package cork

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// BuildKit gives a COPY'd file the owner its context entry carries, so every
// entry must say root: anything else is whoever owns the challenge tree on the
// build host, in every image. Only meaningful when the test does not run as
// root, which is the case on CI and on a developer's machine.
func TestBuildContextEntriesAreOwnedByRoot(t *testing.T) {
	chalDir := t.TempDir()
	for rel, body := range map[string]string{
		"Dockerfile":       "FROM busybox:1.36 AS base\nCOPY secret /secret\n",
		"problem.md":       "# chal\n",
		"secret":           "s3cr3t\n",
		"app/sudoers.conf": "nobody ALL=(ALL) NOPASSWD: ALL\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(chalDir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(chalDir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	m := &Manager{log: newLogger(DISABLED)}
	ctxFile, err := m.createBuildContext(&ChallengeMetadata{Path: filepath.Join(chalDir, "problem.md")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(ctxFile)

	f, err := os.Open(ctxFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	seen := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen++
		if hdr.Uid != 0 || hdr.Gid != 0 || hdr.Uname != "" || hdr.Gname != "" {
			t.Errorf("%s enters the context as %d:%d (%q:%q), not root", hdr.Name, hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname)
		}
	}
	if seen == 0 {
		t.Fatal("the context is empty")
	}
}
