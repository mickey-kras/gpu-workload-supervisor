package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestTrustRejectsHiddenSymlinkAncestors(t *testing.T) {
	for _, target := range []string{"absolute", "relative", "dotdot"} {
		t.Run(target, func(t *testing.T) {
			dir, file, _ := trustFixture(t)
			unsafe := filepath.Join(dir, "unsafe")
			if err := os.Mkdir(unsafe, 0777); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(unsafe, 0777); err != nil {
				t.Fatal(err)
			}
			hidden := filepath.Join(unsafe, "hop")
			if err := os.Symlink(file, hidden); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "entry")
			destination := hidden
			if target == "relative" {
				destination = "unsafe/hop"
			}
			if target == "dotdot" {
				destination = "unsafe/../conf.yaml"
			}
			if err := os.Symlink(destination, link); err != nil {
				t.Fatal(err)
			}
			if _, err := trustPath(link, kindFile); err == nil || !strings.Contains(err.Error(), unsafe) {
				t.Fatalf("hidden unsafe ancestor accepted or not named: %v", err)
			}
		})
	}
}

func TestTrustRejectsStickyWorldWritableLeaves(t *testing.T) {
	dir, file, exec := trustFixture(t)
	for _, tc := range []struct {
		path string
		kind pathKind
	}{{dir, kindDir}, {file, kindFile}, {exec, kindExec}} {
		t.Run(filepath.Base(tc.path), func(t *testing.T) {
			info, err := os.Stat(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(tc.path, info.Mode()|os.ModeSticky|0002); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(tc.path, info.Mode())
			if _, err := trustPath(tc.path, tc.kind); err == nil {
				t.Fatal("sticky world-writable leaf accepted")
			}
		})
	}
}

func TestTrustSymlinkResolutionBoundsAndRelativeTargets(t *testing.T) {
	dir, file, _ := trustFixture(t)
	previous := filepath.Base(file)
	for i := 0; i < 41; i++ {
		link := filepath.Join(dir, fmt.Sprintf("link%d", i))
		if err := os.Symlink(previous, link); err != nil {
			t.Fatal(err)
		}
		previous = filepath.Base(link)
		resolved, err := trustPath(link, kindFile)
		if i < 40 && (err != nil || resolved != file) {
			t.Fatalf("trusted chain %d: %s %v", i+1, resolved, err)
		}
		if i == 40 && err == nil {
			t.Fatal("overlong symlink chain accepted")
		}
	}
	loop := filepath.Join(dir, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	if _, err := trustPath(loop, kindFile); err == nil {
		t.Fatal("symlink loop accepted")
	}
}

func TestRootOnlyLaunchSourceChecksEveryComponent(t *testing.T) {
	_, file, _ := trustFixture(t)
	// Ordinary launch sources can use temporary ancestors, but account sources
	// must reject /tmp even when the process and fixture are root-owned.
	if _, err := readTrustedLaunchSource(file, false); err != nil {
		t.Fatal(err)
	}
	if _, err := readTrustedLaunchSource(file, true); err == nil {
		t.Fatal("root-only source accepted temporary ancestry")
	}
	if _, err := readTrustedLaunchSource("/etc/passwd", true); err != nil {
		t.Fatal("root-owned account database rejected", err)
	}
}

// Synthetic ownership avoids requiring privileged chown or a particular UID
// mapping in containers; the traversal behavior above uses actual files.
type pathOwnershipInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (info pathOwnershipInfo) Sys() any { return &info.stat }

func TestRootOnlyComponentRejectsForeignOwnerAndGroupWrites(t *testing.T) {
	_, file, _ := trustFixture(t)
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	foreign := pathOwnershipInfo{FileInfo: info, stat: syscall.Stat_t{Uid: 12345}}
	if err := checkPathComponent(file, foreign, false, true); err == nil {
		t.Fatal("non-root account source accepted")
	}
	if err := os.Chmod(file, 0664); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	root := pathOwnershipInfo{FileInfo: info, stat: syscall.Stat_t{Uid: 0}}
	if err := checkPathComponent(file, root, false, true); err == nil {
		t.Fatal("group-writable account source accepted")
	}
}

func TestTrustRequiresReadableRegularFileTargets(t *testing.T) {
	dir, file, _ := trustFixture(t)
	link := filepath.Join(dir, "input-link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode     os.FileMode
		readable bool
	}{{0000, false}, {0111, false}, {0400, true}, {0040, true}, {0004, true}} {
		if err := os.Chmod(file, tc.mode); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{file, link} {
			_, err := trustPath(path, kindFile)
			if !tc.readable {
				if err == nil || !strings.Contains(err.Error(), file) {
					t.Fatalf("unreadable target %s with mode %04o accepted or not named: %v", path, tc.mode, err)
				}
			} else if err != nil {
				t.Fatalf("readable target %s with mode %04o rejected: %v", path, tc.mode, err)
			}
		}
	}
}
