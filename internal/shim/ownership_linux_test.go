//go:build linux

package shim

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestRealFS_FsGroupAndChownAs(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root on Linux")
	}
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Mkdir(filepath.Join(root, "sub"), 0o755))
	must(os.WriteFile(filepath.Join(root, "sub", "f"), []byte("x"), 0o600))
	must(os.Symlink("sub/f", filepath.Join(root, "link")))
	outside := filepath.Join(t.TempDir(), "outside")
	must(os.WriteFile(outside, []byte("x"), 0o640))
	must(os.Symlink(outside, filepath.Join(root, "ext")))

	var out, warn bytes.Buffer
	must(FsGroup(PolicyAlways, 54321, []string{root}, &out, &warn))
	if warn.Len() != 0 {
		t.Fatalf("warnings: %s", warn.String())
	}
	stat := func(p string) (fs.FileInfo, *syscall.Stat_t) {
		fi, err := os.Lstat(p)
		must(err)
		return fi, fi.Sys().(*syscall.Stat_t)
	}
	fi, st := stat(filepath.Join(root, "sub"))
	if st.Gid != 54321 || fi.Mode()&fs.ModeSetgid == 0 || fi.Mode().Perm() != 0o775|0o770 {
		t.Errorf("sub: gid=%d mode=%v", st.Gid, fi.Mode())
	}
	fi, st = stat(filepath.Join(root, "sub", "f"))
	if st.Gid != 54321 || fi.Mode().Perm() != 0o660 {
		t.Errorf("f: gid=%d mode=%v", st.Gid, fi.Mode())
	}
	if _, st = stat(filepath.Join(root, "link")); st.Gid != 54321 {
		t.Errorf("link gid=%d, want lchowned", st.Gid)
	}
	ofi, ost := stat(outside)
	if ost.Gid == 54321 || ofi.Mode().Perm() != 0o640 {
		t.Errorf("symlink target was followed: gid=%d mode=%v", ost.Gid, ofi.Mode())
	}
	// Setgid inheritance by the kernel.
	must(os.WriteFile(filepath.Join(root, "sub", "new"), nil, 0o644))
	if _, st = stat(filepath.Join(root, "sub", "new")); st.Gid != 54321 {
		t.Errorf("new file gid=%d, want inherited 54321", st.Gid)
	}

	// chown-as on a fresh tree under the setgid root.
	must(os.MkdirAll(filepath.Join(root, "dl", "d"), 0o755))
	must(os.WriteFile(filepath.Join(root, "dl", "d", "g"), nil, 0o644))
	must(ChownAs(54322, 54323, []string{filepath.Join(root, "dl")}))
	for _, p := range []string{"dl", "dl/d", "dl/d/g"} {
		fi, st := stat(filepath.Join(root, p))
		if st.Uid != 54322 || st.Gid != 54321 {
			t.Errorf("%s owner = %d:%d, want 54322:54321", p, st.Uid, st.Gid)
		}
		if fi.IsDir() && fi.Mode()&fs.ModeSetgid == 0 {
			t.Errorf("%s lost setgid", p)
		}
	}

	var pout bytes.Buffer
	must(Probe(root, &pout))
	if !strings.Contains(pout.String(), "uid=0 ") || !strings.Contains(pout.String(), "file_gid=54321 dir_mode=27") {
		t.Errorf("probe output:\n%s", pout.String())
	}
}

func TestProbe_Format(t *testing.T) {
	var out bytes.Buffer
	if err := Probe(t.TempDir(), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "uid=") || !strings.Contains(lines[0], " gid=") || !strings.Contains(lines[0], " groups=") {
		t.Errorf("line 1: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "file_uid=") || !strings.Contains(lines[1], " file_gid=") || !strings.Contains(lines[1], " dir_mode=") {
		t.Errorf("line 2: %q", lines[1])
	}
}
