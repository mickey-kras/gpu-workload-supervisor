package llmtraining

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestProcessorKeyScanIsLinear(t *testing.T) {
	// Deep nesting with a large leaf, close to the 1 MiB file limit. A scan
	// that parses the rest of the input again at each level takes minutes.
	const depth = 4000
	leaf := strings.Repeat("x", 512<<10)
	nested := strings.Repeat(`{"a":[`, depth) + `"` + leaf + `"` + strings.Repeat(`]}`, depth)
	content := strings.Replace(qwenImageProcessor, "{", `{"deep":`+nested+`,`, 1)
	start := time.Now()
	if err := compileWithFile(t, false, imageProcessorName, content); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("processor key scan took %v", elapsed)
	}
}

func TestDeepLoaderKeyFailsClosedOnUndecodedValues(t *testing.T) {
	category, key := deepLoaderKey(map[string]json.RawMessage{"broken": json.RawMessage(`{`)}, RemoteCodeKeys, nil)
	if category != "top level" || key != undecodedValue {
		t.Fatalf("undecoded value passed the scan: %q %q", category, key)
	}
	if _, key := deepLoaderKey(map[string]json.RawMessage{"big": json.RawMessage(`1e999`)}, RemoteCodeKeys, nil); key != "" {
		t.Fatalf("a large number is not a loader key: %q", key)
	}
}
