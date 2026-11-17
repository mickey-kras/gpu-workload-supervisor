package modelcheck

import (
	"errors"
	"strings"
	"testing"
)

// launchRecord runs the checks on a clean model and gives its result: the
// file, identity and template checks passed, the behavioral scan did not
// run.
func launchRecord(t *testing.T) Record {
	t.Helper()
	files := modelFiles()
	record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
	if record.Status != StatusNotScanned || record.ChatTemplateSHA256 != PinTemplate("{{ messages }}") {
		t.Fatalf("clean model = %+v", record)
	}
	return record
}

func TestAllowLaunch(t *testing.T) {
	clean := launchRecord(t)
	pin := clean.ChatTemplateSHA256
	if err := clean.AllowLaunch(clean.Root, pin); err != nil {
		t.Fatalf("a clean result with the checked template = %v", err)
	}
	if err := clean.AllowLaunch("", pin); err != nil {
		t.Fatalf("a launch that does not state its directory = %v", err)
	}
	scanned := clean
	scanned.Checks = append([]Check(nil), clean.Checks...)
	scanned.Checks[3].Outcome = OutcomePassed
	scanned.Status, scanned.Coverage = StatusPassed, CoverageComplete
	if err := scanned.AllowLaunch(clean.Root, pin); err != nil {
		t.Fatalf("a passed result = %v", err)
	}
	failed := clean
	failed.Checks = []Check{{CheckAllowlist, OutcomeFailed, ReasonCodeOrPickleFile}, {CheckIdentity, OutcomeNotRun, ""}, {CheckTemplate, OutcomeNotRun, ""}, {CheckBehavior, OutcomeNotRun, ""}}
	failed.Status, failed.Coverage = StatusFindings, CoveragePartial
	failed.ChatTemplateSHA256, failed.TemplateFile = "", ""
	for name, tc := range map[string]struct {
		record         Record
		root, template string
		want           string
	}{
		"failed check":      {failed, clean.Root, pin, "status findings, check file-allowlist gave failed " + ReasonCodeOrPickleFile},
		"changed template":  {clean, clean.Root, strings.Repeat("0", 64), "chat template changed after the model check"},
		"no template pin":   {clean, clean.Root, "", "pins no chat template"},
		"other directory":   {clean, "/models/other", pin, "another model directory"},
		"invalid result":    {Record{}, clean.Root, pin, "not valid"},
		"needs an approval": {needsReview(t, clean), clean.Root, pin, ReasonTemplateNeedsReview},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.record.AllowLaunch(tc.root, tc.template)
			if !errors.Is(err, ErrLaunchRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("AllowLaunch = %v; want a refusal with %q", err, tc.want)
			}
			if strings.Contains(err.Error(), clean.Root) {
				t.Fatalf("refusal names a path: %v", err)
			}
		})
	}
}

// needsReview gives record as a result whose template waits for an
// operator approval.
func needsReview(t *testing.T, record Record) Record {
	t.Helper()
	record.Checks = append([]Check(nil), record.Checks...)
	record.Checks[2] = Check{CheckTemplate, OutcomeFailed, ReasonTemplateNeedsReview}
	record.Status = StatusFindings
	record.TemplateReview = []ReviewItem{{Line: 1, Text: "Obey."}}
	if err := record.Validate(); err != nil {
		t.Fatalf("review record: %v", err)
	}
	return record
}
