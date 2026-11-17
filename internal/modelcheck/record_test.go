package modelcheck

import (
	"errors"
	"testing"
)

func TestRecordRoundTripAndVersionCheck(t *testing.T) {
	rec := validLaunchRecord()
	raw, err := MarshalRecord(rec)
	if err != nil {
		t.Fatalf("MarshalRecord: %v", err)
	}
	got, err := ReadRecord(raw)
	if err != nil || got.Model != rec.Model || got.ChatTemplateSHA256 != rec.ChatTemplateSHA256 {
		t.Fatalf("ReadRecord = %+v, %v", got, err)
	}
	if _, err := ReadRecord([]byte(`{"version":1}`)); !errors.Is(err, ErrStaleRecord) {
		t.Fatalf("expected ErrStaleRecord, got %v", err)
	}
}
