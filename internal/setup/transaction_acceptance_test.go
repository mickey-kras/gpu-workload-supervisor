package setup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAcceptanceAmbiguousCommitPreservesJournalUntilDurableDecision(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "precommit", true: "postcommit"}[committed], func(t *testing.T) {
			root := t.TempDir()
			tx := Transaction{Root: root, Changes: map[string][]byte{"unit": []byte("old")}}
			good := Hooks{Quiescent: func() error { return nil }, Commit: func() error { return nil }, Committed: func() (bool, error) { return false, nil }}
			if err := tx.Apply(good); err != nil {
				t.Fatal(err)
			}
			tx.Changes["unit"] = []byte("new")
			unavailable := errors.New("catalog unavailable")
			h := good
			h.Commit = func() error { return errors.New("connection lost after commit request") }
			h.Committed = func() (bool, error) { return false, unavailable }
			if err := tx.Apply(h); !errors.Is(err, unavailable) {
				t.Fatalf("lost inspection failure: %v", err)
			}
			if data, err := os.ReadFile(filepath.Join(root, "unit")); err != nil || string(data) != "new" {
				t.Fatalf("ambiguous outcome must retain prerequisites: %q %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(root, journalName)); err != nil {
				t.Fatal("lost resumable journal", err)
			}
			calls := 0
			h.Committed = func() (bool, error) { return committed, nil }
			h.Commit = func() error { calls++; return errors.New("definite precommit rejection") }
			err := tx.Apply(h)
			want := "old"
			if committed {
				want = "new"
				if err != nil || calls != 0 {
					t.Fatalf("postcommit retried mutation: calls=%d err=%v", calls, err)
				}
			} else if err == nil || calls != 1 {
				t.Fatalf("precommit outcome calls=%d err=%v", calls, err)
			}
			if data, err := os.ReadFile(filepath.Join(root, "unit")); err != nil || string(data) != want {
				t.Fatalf("got %q %v want %q", data, err, want)
			}
			if _, err := os.Stat(filepath.Join(root, journalName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("resolved journal remains", err)
			}
		})
	}
}

func TestAcceptanceQuiescenceFailureHasNoIntegrationEffects(t *testing.T) {
	root := t.TempDir()
	tx := Transaction{Root: root, Changes: map[string][]byte{"unit": []byte("new")}}
	busy := errors.New("runtime became busy")
	h := Hooks{Quiescent: func() error { return busy }, Commit: func() error { t.Fatal("commit despite busy runtime"); return nil }, Committed: func() (bool, error) { return false, nil }}
	if err := tx.Apply(h); !errors.Is(err, busy) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "unit")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("prerequisite installed", err)
	}
	if _, err := os.Stat(filepath.Join(root, journalName)); err != nil {
		t.Fatal("missing recovery journal", err)
	}
}

func TestAcceptancePrivateTransactionMetadata(t *testing.T) {
	for _, name := range []string{manifestName, journalName} {
		for _, kind := range []string{"public", "symlink", "wrong-owner"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, name)
				data := []byte(`{"version":1,"files":{}}`)
				if kind == "symlink" {
					target := filepath.Join(t.TempDir(), "metadata")
					if err := os.WriteFile(target, data, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
					if kind == "public" {
						if err := os.Chmod(path, 0644); err != nil {
							t.Fatal(err)
						}
					} else {
						if os.Geteuid() != 0 {
							t.Skip("foreign ownership requires root")
						}
						if err := os.Chown(path, 65534, 65534); err != nil {
							t.Skipf("foreign uid unavailable in this environment: %v", err)
						}
					}
				}
				h := Hooks{Quiescent: func() error { t.Fatal("untrusted metadata reached effects"); return nil }, Commit: func() error { return nil }, Committed: func() (bool, error) { return false, nil }}
				if err := (Transaction{Root: root, Changes: map[string][]byte{"unit": []byte("new")}}).Apply(h); err == nil {
					t.Fatal("untrusted metadata accepted")
				}
			})
		}
	}
}

func TestAcceptanceMalformedOwnershipCannotPanic(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, manifestName), []byte(`{"version":1,"files":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	h := Hooks{Quiescent: func() error { return nil }, Commit: func() error { return nil }, Committed: func() (bool, error) { return false, nil }}
	if err := (Transaction{Root: root, Changes: map[string][]byte{"unit": []byte("new")}}).Apply(h); err == nil {
		t.Fatal("malformed ownership accepted")
	}
}
