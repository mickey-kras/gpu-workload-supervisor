package modelcheck

import (
	"errors"
	"strings"
	"testing"
)

func TestBranchesFollowJinja(t *testing.T) {
	for _, text := range []string{
		`{% if x %}{% else %}{% else %}{% endif %}`,
		`{% if x %}{% elif %}{% endif %}`,
		`{% if x %}{% else %}{% elif y %}{% endif %}`,
		`{% for x in y %}{% else %}{% else %}{% endfor %}`,
		`{% for x in y %}{% elif z %}{% endfor %}`,
	} {
		if got := scanReason(text); got != ReasonTemplateSyntax {
			t.Errorf("%s: got %q, want %q", text, got, ReasonTemplateSyntax)
		}
	}
	text := `{% if a %}{% elif b %}{% else %}{% if c %}{% else %}{% endif %}{% endif %}{% for x in messages %}{% else %}{% endfor %}`
	if got := scanReason(text); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestCheckerFailureIsAnError(t *testing.T) {
	if got := outcome(CheckTemplate, ReasonTemplateCheckFailed, nil); got.Outcome != OutcomeError {
		t.Fatalf("got %+v", got)
	}
}

// TestStoredReviewListIsBounded checks the bounds of a review list that a
// record holds, for a record that asks for review and has no approval.
func TestStoredReviewListIsBounded(t *testing.T) {
	model := templateModel(map[string]string{"chat_template.jinja": reviewTemplate})
	pending := run(t, newHub(t, upstreamOf(model)).checker(), writeModel(t, model))
	if err := pending.Validate(); err != nil {
		t.Fatal(err)
	}
	many := make([]ReviewItem, maxReviewItems+1)
	for i := range many {
		many[i] = ReviewItem{Line: 1, Text: "x"}
	}
	large := []ReviewItem{{Line: 1, Text: strings.Repeat("<", maxReviewBytes/6+1)}}
	for name, review := range map[string][]ReviewItem{
		"empty text": {{Line: 1, Text: ""}},
		"zero line":  {{Line: 0, Text: "x"}},
		"no items":   nil,
		"too many":   many,
		"too large":  large,
	} {
		r := pending
		r.TemplateReview = review
		if r.Validate() == nil {
			t.Errorf("%s: an invalid review list was accepted", name)
		}
	}
}

// TestPendingReviewNeedsAllChecks checks that PendingReview and Approve
// refuse a record without the checks, and do not panic.
func TestPendingReviewNeedsAllChecks(t *testing.T) {
	if _, ok := (Record{}).PendingReview(); ok {
		t.Fatal("a record without checks asked for review")
	}
	if _, err := Approve(Record{}, TemplateApproval{}); !errors.Is(err, ErrNoPendingReview) {
		t.Fatal("a record without checks got an approval", err)
	}
}
