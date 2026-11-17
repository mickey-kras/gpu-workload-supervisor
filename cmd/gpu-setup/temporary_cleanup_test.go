package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/setup"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func TestTemporaryCleanupCommandBoundaries(t *testing.T) {
	a := systemActions()
	home := t.TempDir()
	a.home = func() (string, error) { return home, nil }
	a.euid = func() int { return 1000 }
	for _, input := range []string{`{`, `{"id":"one","id":"two"}`, `{"unknown":true}`, `{}`, `{"id":"one","token":"secret","externalControlPaused":false}`} {
		if err := a.run([]string{"temporary-cleanup"}, strings.NewReader(input), &bytes.Buffer{}); err == nil {
			t.Fatal("unsafe cleanup command accepted", input)
		}
	}
	if err := a.run([]string{"temporary-status"}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("missing profile accepted")
	}
	a.euid = func() int { return 0 }
	for _, action := range []string{"temporary-status", "temporary-cleanup"} {
		if err := a.run([]string{action}, strings.NewReader("{}"), &bytes.Buffer{}); err == nil {
			t.Fatal("root accepted", action)
		}
	}
}

func TestTemporaryStatusCommandReadsPendingSession(t *testing.T) {
	a := systemActions()
	home := t.TempDir()
	a.home = func() (string, error) { return home, nil }
	a.euid = func() int { return 1000 }
	var r setup.Request
	if err := json.Unmarshal([]byte(request), &r); err != nil {
		t.Fatal(err)
	}
	r.Profile.StatePath = filepath.Join(home, "state.db")
	r.Profile.ActivatedRelease = "v0.1.0"
	state, err := store.Open(t.Context(), r.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	state.Close()
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(r.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "operator.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := a.run([]string{"temporary-status"}, strings.NewReader(""), &out); err != nil || out.String() != "null\n" {
		t.Fatal(out.String(), err)
	}
	session := control.TemporaryDiscoverySession{ID: "old", Token: "secret", Status: "running", PriorStopped: true}
	raw, err = json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", r.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO temporary_discovery_sessions(id,payload,status) VALUES(?,?,?)`, session.ID, raw, session.Status); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := a.run([]string{"temporary-status"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	var actual control.TemporaryDiscoverySession
	if err := json.Unmarshal(out.Bytes(), &actual); err != nil || actual.ID != session.ID {
		t.Fatal(actual, err)
	}
	if err := a.run([]string{"temporary-status"}, strings.NewReader(""), brokenFingerprintIO{}); err == nil {
		t.Fatal("output failure hidden")
	}
}
