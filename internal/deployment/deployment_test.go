package deployment

import (
	"os"
	"path/filepath"
	"testing"
)

func TestActivationCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := Check(path, ""); err != nil {
		t.Fatal(err)
	}
	if err := Check(path, Release); err == nil {
		t.Fatal("managed profile accepted missing marker")
	}
	if err := os.WriteFile(path, []byte("existing state"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []Marker{{Version: 1, Release: Release}, {Version: 1, Release: "old"}, {Version: 1, Release: Release, Maintenance: true}} {
		if err := Write(path, marker); err != nil {
			t.Fatal(err)
		}
		err := Check(path, Release)
		if (err == nil) != (marker.Release == Release && !marker.Maintenance) {
			t.Fatalf("%+v: %v", marker, err)
		}
	}
	if err := os.WriteFile(path+Suffix, []byte(`{"version":1,"release":"dev","unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Check(path, ""); err == nil {
		t.Fatal("unknown fields accepted")
	}
}

func TestPrivateMarkerRejectsLinksPermissionsAndBounds(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state")
	for _, data := range [][]byte{make([]byte, 4097), []byte(`{"version":1,"release":"dev"} {}`), []byte(`{"version":2,"release":"dev"}`)} {
		if err := os.WriteFile(path+Suffix, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(path); err == nil {
			t.Fatal("invalid marker accepted")
		}
	}
	if err := os.Chmod(path+Suffix, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil {
		t.Fatal("public marker accepted")
	}
	os.Remove(path + Suffix)
	if err := os.Symlink(filepath.Join(root, "other"), path+Suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := OpenPrivate("relative"); err == nil {
		t.Fatal("relative accepted")
	}
	if err := Write(path, Marker{}); err == nil {
		t.Fatal("empty marker accepted")
	}
	if err := AtomicWrite(filepath.Join(root, "absent", "file"), nil); err == nil {
		t.Fatal("absent parent accepted")
	}
	if err := AtomicWrite(root, nil); err == nil {
		t.Fatal("directory replaced")
	}
}

func TestManagedActivationRequiresExistingPrivateState(t *testing.T) {
	for _, kind := range []string{"missing", "symlink", "public", "directory"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			if err := Write(path, Marker{Version: 1, Release: Release}); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				if err := os.Symlink(path+".target", path); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.WriteFile(path, []byte("untouched"), 0644); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for _, activated := range []string{"", Release} {
				if err := Check(path, activated); err == nil {
					t.Fatal("accepted untrusted or missing managed state")
				}
			}
			if kind == "missing" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("created missing state", err)
				}
			}
		})
	}
}

func TestManagedActivationAcceptsExistingRelativeState(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("state.db", []byte("existing state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Write("state.db", Marker{Version: 1, Release: Release}); err != nil {
		t.Fatal(err)
	}
	for _, activated := range []string{"", Release} {
		if err := Check("state.db", activated); err != nil {
			t.Fatal(err)
		}
	}
}
