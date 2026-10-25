package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/attestation"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelseal"
)

func TestCLIRejectsBadArgumentsWithoutEchoingThem(t *testing.T) {
	for _, args := range [][]string{
		nil, {"SECRET"}, {"-version", "SECRET"}, {"seal", "-SECRET"}, {"seal", "-root", "/SECRET", "extra"},
		{"verify", "-root", "/SECRET"}, {"seal", "-root", "/SECRET", "-not-scanned", "-policy-version", "1", "-output", "/SECRET/out"},
		{"verify", "-root", "/SECRET", "-envelope", "/SECRET", "-signature", "/SECRET", "-trust", "/SECRET"},
	} {
		var stderr bytes.Buffer
		if exitCode(context.Background(), args, io.Discard, &stderr) != 1 || strings.Contains(stderr.String(), "SECRET") {
			t.Fatalf("unexpected diagnostic for %q: %s", args, stderr.String())
		}
	}
}

func TestCLIPrintsRelease(t *testing.T) {
	var stdout bytes.Buffer
	if exitCode(context.Background(), []string{"-version"}, &stdout, io.Discard) != 0 || stdout.String() != deployment.Release+"\n" {
		t.Fatalf("unexpected version output %q", stdout.String())
	}
}

func TestCLISealsAndVerifies(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "model")
	for name, content := range map[string]string{"config.json": "{}", "model.safetensors": "weights"} {
		write(t, filepath.Join(model, name), content)
	}
	if err := os.Mkdir(filepath.Join(dir, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(dir, "out", "sealed")
	var stdout bytes.Buffer
	args := []string{"seal", "-root", model, "-not-scanned", "-policy-version", "1", "-source-revision", "main", "-output", sealed}
	if code := exitCode(context.Background(), args, &stdout, io.Discard); code != 0 {
		t.Fatal("seal failed")
	}
	var printed modelseal.Sealed
	if err := json.Unmarshal(stdout.Bytes(), &printed); err != nil || printed.Scan != attestation.ScanNotScanned || len(printed.Artifact) != 64 {
		t.Fatalf("seal must print only the artifact ID and scan status: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), "weights") || strings.Contains(stdout.String(), model) {
		t.Fatal("seal output must not hold model content or paths")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	message, err := os.ReadFile(filepath.Join(sealed, modelseal.SigningBytesFile))
	if err != nil {
		t.Fatal(err)
	}
	signature, trust, policy := filepath.Join(dir, "sig.json"), filepath.Join(dir, "trust.json"), filepath.Join(dir, "policy.json")
	writeJSON(t, signature, attestation.Signature{Version: 1, Algorithm: "ed25519", KeyID: attestation.KeyID(public), Value: base64.StdEncoding.EncodeToString(ed25519.Sign(private, message))})
	writeJSON(t, trust, attestation.TrustConfig{Version: 1, Keys: []attestation.TrustedKey{{KeyID: attestation.KeyID(public), PublicKey: base64.StdEncoding.EncodeToString(public)}}, PolicyVersions: []int{1}})
	verify := []string{"verify", "-root", model, "-envelope", filepath.Join(sealed, modelseal.EnvelopeFile), "-signature", signature, "-trust", trust}
	stdout.Reset()
	if code := exitCode(context.Background(), verify, &stdout, io.Discard); code != 0 || !strings.Contains(stdout.String(), `"result":"valid"`) {
		t.Fatalf("valid evidence was rejected: %s", stdout.String())
	}
	write(t, policy, `{"version":1,"mode":"enforce"}`)
	stdout.Reset()
	var stderr bytes.Buffer
	if code := exitCode(context.Background(), append(verify, "-policy", policy), &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), `"reason":"scan-not-scanned"`) {
		t.Fatalf("enforce mode must reject a skipped scan: %s %s", stdout.String(), stderr.String())
	}
	if err := os.WriteFile(filepath.Join(model, "model.safetensors"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := exitCode(context.Background(), verify, &stdout, io.Discard); code != 1 || !strings.Contains(stdout.String(), `"result":"artifact-mismatch"`) {
		t.Fatalf("tampered weights were accepted: %s", stdout.String())
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(raw))
}
