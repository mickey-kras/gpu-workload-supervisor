package setup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnedTransaction(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "integration")
	committed := false
	tx := Transaction{Root: root, Changes: map[string][]byte{"integration": []byte("new")}}
	hooks := Hooks{Quiescent: func() error { return nil }, Commit: func() error { committed = true; return nil }, Committed: func() (bool, error) { return committed, nil }}
	if err := tx.Apply(hooks); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Fatal(string(got))
	}
	if err := tx.Apply(hooks); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("user edit"), 0600); err != nil {
		t.Fatal(err)
	}
	tx.Changes["integration"] = []byte("next")
	if err := tx.Apply(hooks); err == nil {
		t.Fatal("overwrote edited file")
	}
}
func TestPrecommitRollback(t *testing.T) {
	root := t.TempDir()
	tx := Transaction{Root: root, Changes: map[string][]byte{"integration": []byte("new")}}
	err := tx.Apply(Hooks{Quiescent: func() error { return nil }, Commit: func() error { return errors.New("failed") }, Committed: func() (bool, error) { return false, nil }})
	if err == nil {
		t.Fatal("wanted failure")
	}
	if _, err := os.Stat(filepath.Join(root, "integration")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestTransactionInterruptedAndAmbiguousCommit(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "after"}[committed], func(t *testing.T) {
			root := t.TempDir()
			tx := Transaction{Root: root, Changes: map[string][]byte{"integration": []byte("next")}}
			pending := journal{Version: 1, Files: map[string]entry{"integration": {After: []byte("next")}}}
			if err := writeJSON(filepath.Join(root, journalName), pending); err != nil {
				t.Fatal(err)
			}
			if committed {
				if err := os.WriteFile(filepath.Join(root, "integration"), []byte("next"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			h := Hooks{Quiescent: func() error { return nil }, Commit: func() error { return errors.New("ambiguous response") }, Committed: func() (bool, error) { return committed, nil }}
			err := tx.Apply(h)
			if committed && err != nil {
				t.Fatal(err)
			}
			if !committed && err == nil {
				t.Fatal("missing error")
			}
		})
	}
}
func TestTransactionRejectsUnsafeAndChangedJournal(t *testing.T) {
	h := Hooks{Quiescent: func() error { return nil }, Commit: func() error { return nil }, Committed: func() (bool, error) { return false, nil }}
	for _, name := range []string{"../file", "/file", ".", "..", manifestName, journalName, "a/b", ""} {
		if err := (Transaction{Root: t.TempDir(), Changes: map[string][]byte{name: nil}}).Apply(h); err == nil {
			t.Fatal(name)
		}
	}
	for _, data := range []string{"{", `{"version":2}`, `{"version":1,"files":{"other":{"after":""}}}`, `{"version":1,"files":{"integration":{"after":"b2xk"}}}`} {
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, journalName), []byte(data), 0600)
		if err := (Transaction{Root: root, Changes: map[string][]byte{"integration": []byte("new")}}).Apply(h); err == nil {
			t.Fatal(data)
		}
	}
	for _, data := range []string{"{", `{"version":2}`} {
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, manifestName), []byte(data), 0600)
		if err := (Transaction{Root: root}).Apply(h); err == nil {
			t.Fatal(data)
		}
	}
	root := t.TempDir()
	tx := Transaction{Root: root, Changes: map[string][]byte{"integration": []byte("new")}}
	os.WriteFile(filepath.Join(root, "integration"), []byte("user unit"), 0600)
	if err := tx.Apply(h); err == nil {
		t.Fatal("unowned overwrite")
	}
	if err := TrustedDirectory("relative"); err == nil {
		t.Fatal("relative directory")
	}
	if err := TrustedDirectory(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing directory")
	}
	os.Mkdir(filepath.Join(root, "unsafe"), 0777)
	os.Chmod(filepath.Join(root, "unsafe"), 0777)
	if err := TrustedDirectory(filepath.Join(root, "unsafe")); err == nil {
		t.Fatal("unsafe directory")
	}
	if _, err := privateRead(root); err == nil {
		t.Fatal("directory read")
	}
	if err := writeJSON(filepath.Join(root, "json"), make(chan int)); err == nil {
		t.Fatal("marshal channel")
	}
}
func TestRollbackPreservesBeforeAndRefusesUserEdits(t *testing.T) {
	root := t.TempDir()
	tx := Transaction{Root: root, Changes: map[string][]byte{"integration": []byte("old")}}
	h := Hooks{Quiescent: func() error { return nil }, Commit: func() error { return nil }, Committed: func() (bool, error) { return false, nil }}
	if err := tx.Apply(h); err != nil {
		t.Fatal(err)
	}
	tx.Changes["integration"] = []byte("new")
	h.Commit = func() error { return errors.New("commit failed") }
	if err := tx.Apply(h); err == nil {
		t.Fatal("commit error lost")
	}
	data, _ := os.ReadFile(filepath.Join(root, "integration"))
	if string(data) != "old" {
		t.Fatal(string(data))
	}
	pending := journal{Version: 1, Files: map[string]entry{"integration": {Before: []byte("old"), Existed: true, After: []byte("new")}}}
	os.WriteFile(filepath.Join(root, "integration"), []byte("user edit"), 0600)
	if err := tx.rollback(pending); err == nil {
		t.Fatal("rolled back user edit")
	}
	if err := tx.finalize(filepath.Join(root, manifestName), filepath.Join(root, journalName), ownership{Version: 1, Files: map[string]string{}}, pending); err == nil {
		t.Fatal("finalized user edit")
	}
}
