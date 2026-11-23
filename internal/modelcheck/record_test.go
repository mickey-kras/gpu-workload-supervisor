package modelcheck

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

func validRecord(t *testing.T) (Record, []byte) {
	t.Helper()
	files := modelFiles()
	record := run(t, testChecker(), writeModel(t, files))
	raw, err := MarshalRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	return record, raw
}

func TestRecordRoundTripsStrictly(t *testing.T) {
	record, raw := validRecord(t)
	read, err := ReadRecord(raw)
	if err != nil || read.Status != record.Status || read.Files["config.json"] != record.Files["config.json"] || !read.CheckedAt.Equal(record.CheckedAt) {
		t.Fatal("a valid record did not round trip", err)
	}
	text := string(raw)
	for name, content := range map[string]string{
		"malformed":            text[:len(text)-1],
		"oversized":            text + strings.Repeat(" ", MaxRecordBytes),
		"duplicate key":        strings.Replace(text, "{", `{"version":1,`, 1),
		"unknown field":        strings.Replace(text, "{", `{"extra":1,`, 1),
		"null field":           strings.Replace(text, `"checker":"test-1"`, `"checker":null`, 1),
		"trailing value":       text + "{}",
		"not UTF-8":            strings.Replace(text, `"test-1"`, "\"test-\xff\"", 1),
		"other version":        strings.Replace(text, `"version":3`, `"version":0`, 1),
		"status not derived":   strings.Replace(text, `"status":"not_scanned"`, `"status":"passed"`, 1),
		"unknown outcome":      strings.Replace(text, `"outcome":"not_run"`, `"outcome":"skipped"`, 1),
		"checks out of order":  strings.Replace(text, `"name":"file-allowlist"`, `"name":"chat-template"`, 1),
		"local time":           strings.Replace(text, `09:00:00Z`, `09:00:00+01:00`, 1),
		"invalid digest":       strings.Replace(text, record.Files["config.json"], "zz", 1),
		"relative root":        strings.Replace(text, `"root":"/`, `"root":"`, 1),
		"differently spelled":  strings.Replace(text, `"checker":`, `"Checker":`, 1),
		"coverage not derived": strings.Replace(text, `"coverage":"partial"`, `"coverage":"complete"`, 1),
		"other configuration":  strings.Replace(text, `"maxModelFiles":4096`, `"maxModelFiles":0`, 1),
		"template review":      strings.Replace(text, "{", `{"templateReview":[],`, 1),
		"source":               strings.Replace(text, "{", `{"source":{"repo":"a/b","revision":"`+strings.Repeat("a", 40)+`"},`, 1),
		"reason with content":  strings.Replace(text, `"outcome":"not_run"`, `"outcome":"not_run","reason":"/private/path"`, 1),
		"no template limits":   regexp.MustCompile(`"template":\{[^}]*\}`).ReplaceAllString(text, `"template":{}`),
		"zero maxBytes":        zeroLimit(text, "maxBytes"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadRecord([]byte(content)); err == nil {
				t.Fatal("an invalid record was read")
			}
		})
	}
}

// zeroLimit sets one template limit of a record to 0.
func zeroLimit(text, key string) string {
	return regexp.MustCompile(`"`+key+`":[0-9]+`).ReplaceAllString(text, `"`+key+`":0`)
}

func TestStatusFollowsTheFirstCheckThatDidNotPass(t *testing.T) {
	for _, tc := range []struct {
		outcomes []Outcome
		want     Status
	}{
		{[]Outcome{OutcomePassed, OutcomePassed, OutcomePassed}, StatusPassed},
		{[]Outcome{OutcomePassed, OutcomePassed, OutcomeFailed}, StatusFindings},
		{[]Outcome{OutcomePassed, OutcomeError, OutcomeNotRun}, StatusError},
		{[]Outcome{OutcomePassed, OutcomePassed, OutcomeUnsupported}, StatusUnsupported},
		{[]Outcome{OutcomePassed, OutcomeNotRun, OutcomeNotRun}, StatusNotScanned},
	} {
		checks := make([]Check, len(tc.outcomes))
		for i, outcome := range tc.outcomes {
			checks[i] = Check{Name: checkOrder[i], Outcome: outcome}
		}
		if got := statusOf(checks); got != tc.want {
			t.Fatalf("status of %v = %s, want %s", tc.outcomes, got, tc.want)
		}
	}
}

func TestMarshalRecordRefusesInvalidRecords(t *testing.T) {
	record, _ := validRecord(t)
	for name, change := range map[string]func(*Record){
		"no files":      func(r *Record) { r.Files = nil },
		"nested escape": func(r *Record) { r.Files["../x"] = r.Files["config.json"] },
		"bad model":     func(r *Record) { r.Model = "-bad" },
		"bad checker":   func(r *Record) { r.Checker = "" },
		"missing check": func(r *Record) { r.Checks = r.Checks[:2] },
		"zero time":     func(r *Record) { r.CheckedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			copied := record
			copied.Files = map[string]string{}
			for k, v := range record.Files {
				copied.Files[k] = v
			}
			copied.Checks = append([]Check(nil), record.Checks...)
			change(&copied)
			if _, err := MarshalRecord(copied); err == nil {
				t.Fatal("an invalid record was encoded")
			}
		})
	}
}

func TestOtherRecordVersionsAreStale(t *testing.T) {
	_, raw := validRecord(t)
	for _, version := range []string{`"version":2`, `"version":4`} {
		other := strings.Replace(string(raw), `"version":3`, version, 1)
		if _, err := ReadRecord([]byte(other)); !errors.Is(err, ErrStaleRecord) {
			t.Fatal("another record version must read as stale", version, err)
		}
	}
	// A record of version 2 has a source, a hub and a template review. It
	// reads as stale, not as invalid.
	old := `{"version":2,"source":{"repo":"a/b","revision":"` + strings.Repeat("a", 40) + `"},"templateReview":[{"line":1,"text":"x"}],"configuration":{"hub":"https://huggingface.co","maxJSONFileBytes":1,"maxGitBlobBytes":1,"maxModelFiles":1}}`
	if _, err := ReadRecord([]byte(old)); !errors.Is(err, ErrStaleRecord) {
		t.Fatal("a version 2 record must read as stale", err)
	}
}
