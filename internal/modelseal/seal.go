package modelseal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/attestation"
)

// SealRequest names one target, the scan choice and a new output directory.
// Exactly one of ScannerConfig and NotScanned is set: a skipped scan is an
// explicit choice, never a default pass.
type SealRequest struct {
	Target        Target
	ScannerConfig string
	NotScanned    bool
	PolicyVersion int
	// Source is optional. Leave it nil when the source is unknown.
	Source *attestation.Source
	Output string
	Now    func() time.Time
}

// Sealed identifies the published evidence without its content.
type Sealed struct {
	Artifact string                 `json:"artifact"`
	Scan     attestation.ScanStatus `json:"scan"`
}

// Seal inventories the target, runs the configured scanner if any, and
// publishes the canonical envelope, its exact signing bytes and the report as
// one new private directory. An offline signer signs the signing bytes. Seal
// never receives a key.
func Seal(ctx context.Context, r SealRequest) (Sealed, error) {
	if r.NotScanned == (r.ScannerConfig != "") {
		return Sealed{}, errors.New("select exactly one scanner configuration or not-scanned")
	}
	if !cleanAbsolute(r.Output) || privateDirectory(filepath.Dir(r.Output)) != nil {
		return Sealed{}, errors.New("output must be a new path in a private directory")
	}
	var config *ScannerConfig
	if r.ScannerConfig != "" {
		read, err := ReadScannerConfig(r.ScannerConfig)
		if err != nil {
			return Sealed{}, err
		}
		config = &read
	}
	if overlaps(r, config) {
		return Sealed{}, errors.New("output must be outside the artifact and the scanner runtime")
	}
	taken, err := r.Target.take()
	if err != nil {
		return Sealed{}, err
	}
	evidence := notScanned()
	if config != nil {
		if evidence, err = assess(ctx, r.Target, taken, *config, filepath.Dir(r.Output)); err != nil {
			return Sealed{}, err
		}
		// An interrupted scan is not evidence; publish nothing.
		if ctx.Err() != nil {
			return Sealed{}, errors.New("seal was interrupted before publication")
		}
	}
	files, err := sealedFiles(r, taken, evidence)
	if err != nil {
		return Sealed{}, err
	}
	if err := publishDirectory(r.Output, files); err != nil {
		return Sealed{}, err
	}
	return Sealed{Artifact: taken.digest, Scan: evidence.scan.Status}, nil
}

func overlaps(r SealRequest, config *ScannerConfig) bool {
	parent := filepath.Dir(r.Output)
	roots := []string{r.Target.path()}
	if config != nil {
		roots = append(roots, config.Runtime.Inventory.Root)
	}
	for _, root := range roots {
		if root == "" || within(parent, root) || within(root, r.Output) {
			return true
		}
	}
	return config != nil && (within(r.Target.path(), config.Runtime.Inventory.Root) || within(config.Runtime.Inventory.Root, r.Target.path()))
}

func sealedFiles(r SealRequest, taken inventory, evidence assessment) ([]output, error) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	envelope := attestation.Envelope{
		Version:       attestation.FormatVersion,
		Artifact:      taken.artifact,
		Source:        r.Source,
		Scan:          evidence.scan,
		PolicyVersion: r.PolicyVersion,
		Timestamp:     now().UTC().Format(attestation.TimestampLayout),
	}
	canonical, err := attestation.MarshalEnvelope(envelope)
	if err != nil {
		return nil, errors.New("envelope is invalid: check the policy version and the source fields")
	}
	signing, err := attestation.SigningBytes(envelope)
	if err != nil {
		return nil, errors.New("signing bytes cannot be made")
	}
	files := []output{{name: EnvelopeFile, data: canonical}, {name: SigningBytesFile, data: signing}}
	if evidence.report != nil {
		files = append(files, output{name: ReportFile, data: evidence.report})
	}
	return files, nil
}

// VerifyRequest names the target, the evidence files, the pinned trust and
// an optional policy file.
type VerifyRequest struct {
	Target    Target
	Envelope  string
	Signature string
	Trust     string
	Policy    string
}

// Outcome is the verification result. It holds identifiers only.
type Outcome struct {
	Result   attestation.Result     `json:"result"`
	Artifact string                 `json:"artifact,omitempty"`
	KeyID    string                 `json:"keyId,omitempty"`
	Scan     attestation.ScanStatus `json:"scan,omitempty"`
	Decision *attestation.Decision  `json:"decision,omitempty"`
}

// Accepted is true for valid evidence that the policy, if any, allows.
func (o Outcome) Accepted() bool {
	return o.Result == attestation.Valid && (o.Decision == nil || o.Decision.Allowed)
}

// Verify checks the detached signature against the pinned keys, the policy
// version against the trust configuration and the bound digests against a
// fresh walk of the target. It uses attestation.Verify and attestation.Decide,
// the same code as the supervisor gate.
func Verify(r VerifyRequest) (Outcome, error) {
	raw, err := readTrusted(r.Trust, attestation.MaxTrustConfigBytes)
	if err != nil {
		return Outcome{}, err
	}
	trust, err := attestation.ReadTrustConfig(bytes.NewReader(raw))
	if err != nil {
		return Outcome{}, errors.New("trust configuration is invalid")
	}
	verifier, err := attestation.NewVerifier(trust)
	if err != nil {
		return Outcome{}, errors.New("trust configuration is invalid")
	}
	taken, err := r.Target.take()
	if err != nil {
		return Outcome{}, err
	}
	envelope, signature := evidence(r.Envelope), evidence(r.Signature)
	defer envelope.Close()
	defer signature.Close()
	verification := verifier.Verify(envelope, signature, taken.artifact)
	outcome := Outcome{Result: verification.Result(), Artifact: taken.digest, KeyID: verification.KeyID()}
	if verification.Result() == attestation.Valid {
		outcome.Scan = verification.Envelope().Scan.Status
	}
	if r.Policy != "" {
		decision := attestation.Decide(readPolicy(r.Policy), verifier, taken.artifact, &verification)
		outcome.Decision = &decision
	}
	return outcome, nil
}

// readPolicy gives the zero policy for any failure, which Decide rejects as
// invalid-policy. It never falls back to advisory mode.
func readPolicy(path string) attestation.Policy {
	raw, err := readTrusted(path, attestation.MaxPolicyBytes)
	if err != nil {
		return attestation.Policy{}
	}
	policy, err := attestation.ReadPolicy(bytes.NewReader(raw))
	if err != nil {
		return attestation.Policy{}
	}
	return policy
}

// evidence opens one evidence file. A file that cannot be opened reads as a
// failing reader, so Verify reports it as unreadable.
func evidence(path string) io.ReadCloser {
	f, err := openRegular(path)
	if err != nil {
		return failingReader{}
	}
	return f
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, os.ErrNotExist }
func (failingReader) Close() error             { return nil }
