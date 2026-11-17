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
	record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
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
		"other version":        strings.Replace(text, `"version":2`, `"version":0`, 1),
		"status not derived":   strings.Replace(text, `"status":"not_scanned"`, `"status":"passed"`, 1),
		"unknown outcome":      strings.Replace(text, `"outcome":"not_run"`, `"outcome":"skipped"`, 1),
		"checks out of order":  strings.Replace(text, `"name":"file-allowlist"`, `"name":"upstream-identity"`, 1),
		"local time":           strings.Replace(text, `09:00:00Z`, `09:00:00+01:00`, 1),
		"invalid digest":       strings.Replace(text, record.Files["config.json"], "zz", 1),
		"floating revision":    strings.Replace(text, record.Source.Revision, "main", 1),
		"relative root":        strings.Replace(text, `"root":"/`, `"root":"`, 1),
		"differently spelled":  strings.Replace(text, `"checker":`, `"Checker":`, 1),
		"coverage not derived": strings.Replace(text, `"coverage":"partial"`, `"coverage":"complete"`, 1),
		"other configuration":  strings.Replace(text, `"maxModelFiles":4096`, `"maxModelFiles":0`, 1),
		"plain HTTP hub":       strings.Replace(text, `"hub":"https://`, `"hub":"http://`, 1),
		"reason with content":  strings.Replace(text, `"outcome":"not_run"`, `"outcome":"not_run","reason":"/private/path"`, 1),
		"no template limits":   regexp.MustCompile(`"template":\{[^}]*\}`).ReplaceAllString(text, `"template":{}`),
		"zero maxBytes":        zeroLimit(text, "maxBytes"),
		"zero maxDepth":        zeroLimit(text, "maxDepth"),
		"zero maxTagTokens":    zeroLimit(text, "maxTagTokens"),
		"zero maxReviewItems":  zeroLimit(text, "maxReviewItems"),
		"zero maxReviewBytes":  zeroLimit(text, "maxReviewBytes"),
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
		{[]Outcome{OutcomePassed, OutcomePassed, OutcomePassed, OutcomePassed}, StatusPassed},
		{[]Outcome{OutcomePassed, OutcomePassed, OutcomePassed, OutcomeFailed}, StatusFindings},
		{[]Outcome{OutcomePassed, OutcomeError, OutcomeNotRun, OutcomeNotRun}, StatusError},
		{[]Outcome{OutcomePassed, OutcomePassed, OutcomePassed, OutcomeUnsupported}, StatusUnsupported},
		{[]Outcome{OutcomePassed, OutcomePassed, OutcomeNotRun, OutcomeNotRun}, StatusNotScanned},
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
		"missing check": func(r *Record) { r.Checks = r.Checks[:3] },
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
	for _, version := range []string{`"version":1`, `"version":3`} {
		other := strings.Replace(string(raw), `"version":2`, version, 1)
		if _, err := ReadRecord([]byte(other)); !errors.Is(err, ErrStaleRecord) {
			t.Fatal("another record version must read as stale", version, err)
		}
	}
	// A record of version 1 has no template limits. It reads as stale, not
	// as invalid.
	old := `{"version":1,"configuration":{"hub":"https://huggingface.co","maxJSONFileBytes":1,"maxGitBlobBytes":1,"maxModelFiles":1}}`
	if _, err := ReadRecord([]byte(old)); !errors.Is(err, ErrStaleRecord) {
		t.Fatal("a version 1 record must read as stale", err)
	}
}
