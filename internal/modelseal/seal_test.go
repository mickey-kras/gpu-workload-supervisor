package modelseal

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/attestation"
)

func TestSealNotScannedPublishesCanonicalPrivateEvidence(t *testing.T) {
	w := newWorkspace(t)
	sealed, err := Seal(context.Background(), SealRequest{
		Target: Target{Root: w.model}, NotScanned: true, PolicyVersion: 3,
		Source: &attestation.Source{URI: "https://example.invalid/model", Revision: "abc123"},
		Output: w.output, Now: fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sealed.Scan != attestation.ScanNotScanned || len(sealed.Artifact) != 64 {
		t.Fatalf("unexpected seal result %+v", sealed)
	}
	envelopeBytes, err := os.ReadFile(filepath.Join(w.output, EnvelopeFile))
	if err != nil {
		t.Fatal(err)
	}
	signing, err := os.ReadFile(filepath.Join(w.output, SigningBytesFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(signing, append([]byte(attestation.SigningDomain), envelopeBytes...)) {
		t.Fatal("signing bytes are not the domain prefix and the canonical envelope")
	}
	envelope := readEnvelope(t, w.output)
	canonical, err := attestation.MarshalEnvelope(envelope)
	if err != nil || !bytes.Equal(canonical, envelopeBytes) {
		t.Fatal("published envelope is not canonical", err)
	}
	digest, err := envelope.Artifact.Digest()
	if err != nil || digest != sealed.Artifact {
		t.Fatal("printed artifact ID differs from the envelope artifact digest")
	}
	if envelope.Scan.Scanner != nil || envelope.Scan.ReportSHA256 != "" || envelope.Timestamp != "2026-10-01T12:00:00Z" || envelope.PolicyVersion != 3 {
		t.Fatalf("unexpected envelope %+v", envelope)
	}
	if envelope.Source == nil || envelope.Source.Revision != "abc123" || len(envelope.Artifact.Files) != 6 {
		t.Fatalf("envelope must bind the source claim and every file: %+v", envelope)
	}
	if _, err := os.Stat(filepath.Join(w.output, ReportFile)); !os.IsNotExist(err) {
		t.Fatal("a skipped scan must not publish a report")
	}
	assertPrivate(t, w.output)
}

func assertPrivate(t *testing.T, dir string) {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal("output directory must be private", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("output file %s must be private", entry.Name())
		}
	}
	siblings, err := os.ReadDir(filepath.Dir(dir))
	if err != nil || len(siblings) != 1 {
		t.Fatal("publication must leave no staging directory", err)
	}
}

func TestSealSingleArtifactForm(t *testing.T) {
	w := newWorkspace(t)
	file := filepath.Join(w.dir, "model.gguf")
	writeFile(t, file, "single weights", 0o600)
	sealed, err := Seal(context.Background(), SealRequest{Target: Target{File: file}, NotScanned: true, PolicyVersion: 1, Output: w.output, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	envelope := readEnvelope(t, w.output)
	if envelope.Artifact.SHA256 != digestOf("single weights") || envelope.Artifact.Files != nil {
		t.Fatalf("single form must bind only the file digest: %+v", envelope.Artifact)
	}
	digest, _ := envelope.Artifact.Digest()
	if digest != sealed.Artifact {
		t.Fatal("artifact ID differs")
	}
}

func TestSealRejectsUnsafeRequests(t *testing.T) {
	w := newWorkspace(t)
	base := SealRequest{Target: Target{Root: w.model}, NotScanned: true, PolicyVersion: 1, Output: w.output, Now: fixedNow}
	open := filepath.Join(w.dir, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*SealRequest){
		"no scan choice":      func(r *SealRequest) { r.NotScanned = false },
		"two scan choices":    func(r *SealRequest) { r.ScannerConfig = "/scanner.json" },
		"two targets":         func(r *SealRequest) { r.Target.File = filepath.Join(w.model, "config.json") },
		"no target":           func(r *SealRequest) { r.Target.Root = "" },
		"relative output":     func(r *SealRequest) { r.Output = "sealed" },
		"public parent":       func(r *SealRequest) { r.Output = filepath.Join(open, "sealed") },
		"missing parent":      func(r *SealRequest) { r.Output = filepath.Join(w.dir, "absent", "sealed") },
		"output in artifact":  func(r *SealRequest) { r.Output = filepath.Join(w.model, "sealed") },
		"zero policy version": func(r *SealRequest) { r.PolicyVersion = 0 },
		"empty source":        func(r *SealRequest) { r.Source = &attestation.Source{} },
		"relative root":       func(r *SealRequest) { r.Target.Root = "model" },
		"missing scanner":     func(r *SealRequest) { r.NotScanned, r.ScannerConfig = false, filepath.Join(w.dir, "absent.json") },
	} {
		t.Run(name, func(t *testing.T) {
			request := base
			change(&request)
			if _, err := Seal(context.Background(), request); err == nil {
				t.Fatal("unsafe request was sealed")
			}
		})
	}
	if _, err := os.Stat(w.output); !os.IsNotExist(err) {
		t.Fatal("a refused request must not publish output")
	}
}

func TestSealNeverReplacesOutput(t *testing.T) {
	w := newWorkspace(t)
	if err := os.Mkdir(w.output, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := sealWith(t, w, ""); err == nil || !strings.Contains(err.Error(), "must not exist") {
		t.Fatal("existing output must not be replaced", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(w.output))
	if len(entries) != 1 {
		t.Fatal("refused publication must remove its staging directory")
	}
}

func TestSealRefusesInvalidInventories(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, root string){
		"shard index names a missing shard": func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "extra.safetensors.index.json"), `{"weight_map":{"a":"absent.safetensors"}}`, 0o600)
		},
		"shard index names other weights": func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "pickle.bin"), "pickle", 0o600)
			writeFile(t, filepath.Join(root, "extra.safetensors.index.json"), `{"weight_map":{"a":"pickle.bin"}}`, 0o600)
		},
		"shard index has duplicate keys": func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "extra.safetensors.index.json"), `{"weight_map":{},"weight_map":{}}`, 0o600)
		},
		"path outside the attestation alphabet": func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, `quote".json`), "{}", 0o600)
		},
		"symbolic link": func(t *testing.T, root string) {
			if err := os.Symlink("config.json", filepath.Join(root, "linked.json")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorkspace(t)
			change(t, w.model)
			if _, err := sealWith(t, w, ""); err == nil {
				t.Fatal("invalid inventory was sealed")
			}
			if _, err := os.Stat(w.output); !os.IsNotExist(err) {
				t.Fatal("a refused seal must not publish output")
			}
		})
	}
}
