package setup

import (
	"encoding/json"
	"reflect"
	"testing"
)

func resourceDraft(t *testing.T, settings string) Draft {
	t.Helper()
	var draft Draft
	if err := json.Unmarshal([]byte(`{"id":"chosen","label":"Chosen","app":"ollama"`+settings+`}`), &draft); err != nil {
		t.Fatal(err)
	}
	return draft
}

func TestDraftResourceChecksPersistAndExplicitlyClear(t *testing.T) {
	home := t.TempDir()
	draft := resourceDraft(t, `,"requiredMiB":12345,"bootPolicy":"retain"`)
	saved, err := SaveDrafts(home, DraftRequest{Version: 1, Drafts: []Draft{draft}})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := ReadDrafts(home)
	if err != nil || !reflect.DeepEqual(reopened, saved) || *reopened.Drafts[0].RequiredMiB != 12345 || *reopened.Drafts[0].BootPolicy != "retain" {
		t.Fatalf("resource choices lost after reopen: %+v %v", reopened, err)
	}
	clear := resourceDraft(t, `,"requiredMiB":0,"bootPolicy":""`)
	cleared, err := SaveDrafts(home, DraftRequest{Version: 1, ExpectedRevision: reopened.Revision, Drafts: []Draft{clear}})
	if err != nil {
		t.Fatal(err)
	}
	got := cleared.Drafts[0]
	if got.RequiredMiB == nil || *got.RequiredMiB != 0 || got.BootPolicy == nil || *got.BootPolicy != "" {
		t.Fatalf("explicit clearing became an omitted choice: %+v", got)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || string(fields["requiredMiB"]) != "0" || string(fields["bootPolicy"]) != `""` {
		t.Fatalf("explicit clearing disappeared from response: %s %v", data, err)
	}
}

func TestDraftResourceChecksOmittedForOlderDrafts(t *testing.T) {
	draft := resourceDraft(t, "")
	saved, err := SaveDrafts(t.TempDir(), DraftRequest{Version: 1, Drafts: []Draft{draft}})
	if err != nil || saved.Drafts[0].RequiredMiB != nil || saved.Drafts[0].BootPolicy != nil {
		t.Fatalf("older draft acquired resource overrides: %+v %v", saved, err)
	}
}

func TestDraftResourceChecksRejectInvalidValuesWithoutReplacingSelections(t *testing.T) {
	for _, settings := range []string{
		`,"requiredMiB":-1`, `,"requiredMiB":1.5`, `,"requiredMiB":"12"`, `,"requiredMiB":true`,
		`,"requiredMiB":9007199254740992`, `,"requiredMiB":18446744073709551616`,
		`,"bootPolicy":"automatic"`, `,"bootPolicy":true`, `,"bootPolicy":1`,
	} {
		t.Run(settings, func(t *testing.T) {
			home := t.TempDir()
			original, err := SaveDrafts(home, DraftRequest{Version: 1, Drafts: []Draft{resourceDraft(t, `,"requiredMiB":8000,"bootPolicy":"retain"`)}})
			if err != nil {
				t.Fatal(err)
			}
			var invalid Draft
			err = json.Unmarshal([]byte(`{"id":"chosen","label":"Chosen","app":"ollama"`+settings+`}`), &invalid)
			if err == nil {
				_, err = SaveDrafts(home, DraftRequest{Version: 1, ExpectedRevision: original.Revision, Drafts: []Draft{invalid}})
			}
			if err == nil {
				t.Fatal("invalid resource checks saved")
			}
			reopened, err := ReadDrafts(home)
			if err != nil || !reflect.DeepEqual(reopened, original) {
				t.Fatalf("invalid save replaced existing selections: %+v %v", reopened, err)
			}
		})
	}
}
