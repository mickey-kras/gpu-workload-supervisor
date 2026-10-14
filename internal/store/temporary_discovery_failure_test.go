package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestTemporaryDiscoveryRejectsInvalidEntryWithoutStartingTransition(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*control.TemporaryDiscoverySession)
	}{
		{"mismatched id", func(v *control.TemporaryDiscoverySession) { v.ID = "other" }},
		{"missing token", func(v *control.TemporaryDiscoverySession) { v.Token = "" }},
		{"already running", func(v *control.TemporaryDiscoverySession) { v.Status = "running" }},
		{"prior invocation running", func(v *control.TemporaryDiscoverySession) { v.PriorStopped = false }},
		{"preexisting invocation", func(v *control.TemporaryDiscoverySession) { v.InvocationID = "unexpected" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, e := activationFixture(t)
			ctx := context.Background()
			before, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			v := control.TemporaryDiscoverySession{ID: "temporary", Token: "token", Status: "starting", PriorStopped: true}
			test.change(&v)
			tr := Transition{ID: "temporary", Source: before, Target: before, Previous: before, ConfigurationRevision: e.ConfigurationRevision, Deadline: time.Now().Add(time.Minute)}
			if _, err := s.StartTemporaryDiscovery(ctx, e, tr, v); err == nil {
				t.Fatal("invalid session started")
			}
			after, err := s.State(ctx)
			if err != nil || after != before {
				t.Fatalf("rejected entry changed state: %+v, %v", after, err)
			}
			if id, err := s.InProgressTransition(ctx); err != nil || id != "" {
				t.Fatalf("rejected entry left transition %q: %v", id, err)
			}
			if session, err := s.TemporaryDiscoveryStatus(ctx); err != nil || session != nil {
				t.Fatalf("rejected entry persisted session: %+v, %v", session, err)
			}
		})
	}
}

func TestTemporaryDiscoveryChecksTokenAndCompletedSession(t *testing.T) {
	s, _, v := startTemporaryFixture(t)
	ctx := context.Background()
	if err := s.CheckTemporaryDiscovery(ctx, v.ID, v.Token); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "wrong"} {
		if err := s.CheckTemporaryDiscovery(ctx, v.ID, token); !errors.Is(err, ErrStaleFence) {
			t.Fatalf("token accepted: %v", err)
		}
	}
	if _, err := s.FinishTemporaryDiscovery(ctx, v.ID, v.Token); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckTemporaryDiscovery(ctx, v.ID, v.Token); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("completed session accepted: %v", err)
	}
}

func TestTemporaryDiscoveryCannotEraseInvocationOrInventLaunchEvidence(t *testing.T) {
	s, _, v := startTemporaryFixture(t)
	ctx := context.Background()
	if err := s.UpdateTemporaryDiscovery(ctx, v.ID, v.Token, "invocation", "running", ""); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("unrecorded invocation accepted: %v", err)
	}
	if err := s.UpdateTemporaryDiscovery(ctx, v.ID, v.Token, "", "completed", ""); err == nil {
		t.Fatal("update bypassed completion transaction")
	}
	launch := control.TemporaryDiscoveryLaunchEvidence{InvocationID: "invocation", JobID: "42", ActivationTimestamp: "123"}
	if err := s.RecordTemporaryLaunch(ctx, v.ID, v.Token, launch); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTemporaryDiscovery(ctx, v.ID, v.Token, launch.InvocationID, "running", ""); err != nil {
		t.Fatal(err)
	}
	before, err := s.TemporaryDiscoveryStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTemporaryDiscovery(ctx, v.ID, v.Token, "", "cleanup_required", "failure"); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("invocation erased: %v", err)
	}
	after, err := s.TemporaryDiscoveryStatus(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected update changed evidence: %+v, %v", after, err)
	}
}

func TestTemporaryDiscoveryCompletionRollsBackWhenSessionWriteFails(t *testing.T) {
	s, state, v := startTemporaryFixture(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_session_update BEFORE UPDATE ON temporary_discovery_sessions
		BEGIN SELECT RAISE(ABORT, 'session write unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishTemporaryDiscovery(ctx, v.ID, v.Token); err == nil || !strings.Contains(err.Error(), "session write unavailable") {
		t.Fatalf("completion failure = %v", err)
	}
	after, err := s.State(ctx)
	if err != nil || after != state {
		t.Fatalf("failed completion changed state: %+v, %v", after, err)
	}
	if id, err := s.InProgressTransition(ctx); err != nil || id != v.ID {
		t.Fatalf("failed completion lost transition: %q, %v", id, err)
	}
	record, err := s.TemporaryDiscoveryStatus(ctx)
	if err != nil || !reflect.DeepEqual(record, &v) {
		t.Fatalf("failed completion changed session: %+v, %v", record, err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER reject_session_update"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishTemporaryDiscovery(ctx, v.ID, v.Token); err != nil {
		t.Fatalf("retry could not complete: %v", err)
	}
}
