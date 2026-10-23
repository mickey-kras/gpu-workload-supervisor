package attestation

import (
	"encoding/json"
	"fmt"
	"io"
)

// PolicyFileVersion is the only policy format this package reads.
const PolicyFileVersion = 1

// MaxPolicyBytes bounds the policy file, including its exemptions.
const MaxPolicyBytes = 1 << 20

type Mode string

const (
	Advisory Mode = "advisory"
	Enforce  Mode = "enforce"
)

// ExemptionKind names the one unverified outcome an exemption admits. The
// values follow the scan status spelling.
type ExemptionKind string

const (
	ExemptNotScanned      ExemptionKind = "not_scanned"
	ExemptUnsupported     ExemptionKind = "unsupported"
	ExemptFindings        ExemptionKind = "findings"
	ExemptPartialCoverage ExemptionKind = "partial_coverage"
)

// Exemption admits one outcome for one artifact digest under one policy
// version. Findings and partial coverage also bind the report digest, so a
// new report is never covered silently. It never admits scan errors,
// integrity failures, unreadable or unsupported evidence, or a missing
// attestation. The setup journal records who added it and when.
type Exemption struct {
	Artifact      string        `json:"artifact"`
	PolicyVersion int           `json:"policyVersion"`
	Kind          ExemptionKind `json:"kind"`
	ReportSHA256  string        `json:"reportSHA256,omitempty"`
	Reason        string        `json:"reason"`
}

// Policy is admission policy, separate from signer trust. Setup and the
// rollback journal persist it; consumers embed *Policy with omitempty. Mode
// has no implicit default: Decide treats every value other than Advisory as
// Enforce.
type Policy struct {
	Version    int         `json:"version"`
	Mode       Mode        `json:"mode"`
	Exemptions []Exemption `json:"exemptions,omitempty"`
	Deny       []string    `json:"deny,omitempty"`
}

type Status string

const (
	Verified         Status = "verified"
	NotVerified      Status = "not-verified"
	FindingsDetected Status = "findings-detected"
	IntegrityFailure Status = "integrity-failure"
	Exempted         Status = "exempted"
)

// Reason is a stable code for logs and UI.
type Reason string

const (
	ReasonMissing            Reason = "attestation-missing"
	ReasonUnsealed           Reason = "unsealed-verification"
	ReasonArtifactDenied     Reason = "artifact-denied"
	ReasonMalformed          Reason = Reason(Malformed)
	ReasonUnreadable         Reason = Reason(Unreadable)
	ReasonUnsupportedVersion Reason = Reason(UnsupportedVersion)
	ReasonInvalidInventory   Reason = Reason(InvalidInventory)
	ReasonInvalidSignature   Reason = Reason(InvalidSignature)
	ReasonUnknownKey         Reason = Reason(UnknownKey)
	ReasonArtifactMismatch   Reason = Reason(ArtifactMismatch)
	ReasonIncompatiblePolicy Reason = Reason(IncompatiblePolicy)
	ReasonPassed             Reason = "scan-passed"
	ReasonFindings           Reason = "scan-findings"
	ReasonScanError          Reason = "scan-error"
	ReasonNotScanned         Reason = "scan-not-scanned"
	ReasonUnsupported        Reason = "scan-unsupported"
	ReasonPartialCoverage    Reason = "scan-partial-coverage"
)

// Decision is recomputed for each admission and never persisted.
type Decision struct {
	Allowed bool   `json:"allowed"`
	Status  Status `json:"status"`
	Reason  Reason `json:"reason"`
}

// ReadPolicy reads one bounded strict policy file and validates it.
func ReadPolicy(r io.Reader) (Policy, error) {
	var policy Policy
	if err := policyDocument.read(r, &policy, func() ([]byte, error) { return json.Marshal(policy) }); err != nil {
		return Policy{}, err
	}
	if err := policy.Validate(); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

// Validate needs an explicit mode. Setup writes "advisory" when the operator
// chooses nothing and shows it; the zero Policy fails closed in Decide.
func (p Policy) Validate() error {
	if p.Version != PolicyFileVersion {
		return fmt.Errorf("%w: policy", ErrUnsupportedVersion)
	}
	if p.Mode != Advisory && p.Mode != Enforce {
		return fmt.Errorf("%w: mode must be advisory or enforce", ErrInvalidPolicy)
	}
	seen := map[Exemption]bool{}
	for index, exemption := range p.Exemptions {
		if !exemption.valid() {
			return fmt.Errorf("%w: exemption %d needs a digest, a policy version of 1 to %d, a known kind, a report digest for findings and partial_coverage only, and a reason of at most %d bytes", ErrInvalidPolicy, index, MaxPolicyVersion, MaxTextBytes)
		}
		scope := exemption
		scope.Reason = ""
		if seen[scope] {
			return fmt.Errorf("%w: exemption %d duplicates an earlier scope", ErrInvalidPolicy, index)
		}
		seen[scope] = true
	}
	denied := map[string]bool{}
	for index, digest := range p.Deny {
		if !digestPattern.MatchString(digest) || denied[digest] {
			return fmt.Errorf("%w: denial %d must be a unique lowercase hex artifact digest", ErrInvalidPolicy, index)
		}
		denied[digest] = true
	}
	return nil
}

func (e Exemption) valid() bool {
	var needsReport bool
	switch e.Kind {
	case ExemptFindings, ExemptPartialCoverage:
		needsReport = true
	case ExemptNotScanned, ExemptUnsupported:
	default:
		return false
	}
	if needsReport != digestPattern.MatchString(e.ReportSHA256) || (!needsReport && e.ReportSHA256 != "") {
		return false
	}
	return digestPattern.MatchString(e.Artifact) && validPolicyVersion(e.PolicyVersion) && safeText(e.Reason, MaxTextBytes)
}

// Decide maps one root to admission. onDisk is the inventory the gate built;
// its digest decides denial first, whatever the evidence state. Pass a nil
// verification when no attestation exists.
func Decide(policy Policy, onDisk Artifact, verification *Verification) Decision {
	permissive := policy.Mode == Advisory
	missing := Decision{Allowed: permissive, Status: NotVerified, Reason: ReasonMissing}
	digest, err := onDisk.Digest()
	if err != nil {
		// No denial can name an inventory without a digest.
		if verification == nil {
			return missing
		}
		return Decision{Status: IntegrityFailure, Reason: ReasonInvalidInventory}
	}
	if policy.denies(digest) {
		return Decision{Status: IntegrityFailure, Reason: ReasonArtifactDenied}
	}
	if verification == nil {
		return missing
	}
	if decision, final := decideEvidence(permissive, verification); final {
		return decision
	}
	if verification.digest != digest {
		return Decision{Status: IntegrityFailure, Reason: ReasonArtifactMismatch}
	}
	return decideScan(policy, verification.envelope, digest)
}

// decideEvidence settles every result other than a sealed Valid.
func decideEvidence(permissive bool, verification *Verification) (Decision, bool) {
	unsealed := Decision{Status: IntegrityFailure, Reason: ReasonUnsealed}
	if !verification.sealed {
		return unsealed, true
	}
	switch verification.result {
	case Valid:
		if verification.digest == "" {
			return unsealed, true
		}
		return Decision{}, false
	case Unreadable, UnsupportedVersion:
		// Neither proves tampering, and denial already applied.
		return Decision{Allowed: permissive, Status: NotVerified, Reason: Reason(verification.result)}, true
	case Malformed, InvalidInventory, InvalidSignature, UnknownKey, ArtifactMismatch, IncompatiblePolicy:
		return Decision{Status: IntegrityFailure, Reason: Reason(verification.result)}, true
	}
	return unsealed, true
}

func decideScan(policy Policy, envelope Envelope, digest string) Decision {
	permissive := policy.Mode == Advisory
	scan := envelope.Scan
	var kind ExemptionKind
	var reason Reason
	status := NotVerified
	switch {
	case scan.Status == ScanPassed && scan.Coverage == CoverageComplete:
		return Decision{Allowed: true, Status: Verified, Reason: ReasonPassed}
	case scan.Status == ScanError:
		return Decision{Allowed: permissive, Status: NotVerified, Reason: ReasonScanError}
	case scan.Status == ScanFindings:
		kind, reason, status = ExemptFindings, ReasonFindings, FindingsDetected
	case scan.Status == ScanPassed:
		kind, reason = ExemptPartialCoverage, ReasonPartialCoverage
	case scan.Status == ScanNotScanned:
		kind, reason = ExemptNotScanned, ReasonNotScanned
	case scan.Status == ScanUnsupported:
		kind, reason = ExemptUnsupported, ReasonUnsupported
	default:
		return Decision{Status: IntegrityFailure, Reason: ReasonUnsealed}
	}
	if policy.exempts(digest, envelope.PolicyVersion, kind, scan.ReportSHA256) {
		return Decision{Allowed: true, Status: Exempted, Reason: reason}
	}
	return Decision{Allowed: permissive && status == NotVerified, Status: status, Reason: reason}
}

func (p Policy) denies(digest string) bool {
	for _, denied := range p.Deny {
		if denied == digest {
			return true
		}
	}
	return false
}

func (p Policy) exempts(digest string, policyVersion int, kind ExemptionKind, report string) bool {
	for _, exemption := range p.Exemptions {
		if exemption.Artifact != digest || exemption.PolicyVersion != policyVersion || exemption.Kind != kind {
			continue
		}
		// Report-bound kinds fail closed even in an unvalidated policy.
		if kind != ExemptFindings && kind != ExemptPartialCoverage {
			return true
		}
		if report != "" && exemption.ReportSHA256 == report {
			return true
		}
	}
	return false
}

// Root is one required inventory root and its attestation. Use a nil
// Verification for an absent attestation.
type Root struct {
	Artifact     Artifact
	Verification *Verification
}

// statusRank orders combined decisions from most to least severe.
var statusRank = map[Status]int{IntegrityFailure: 4, FindingsDetected: 3, NotVerified: 2, Exempted: 1, Verified: 0}

// DecideAll combines one entry for each required root, such as separate
// model and tokenizer roots. The first most severe decision wins and any
// rejection rejects the whole set. An empty set is a missing attestation.
func DecideAll(policy Policy, roots []Root) Decision {
	if len(roots) == 0 {
		return Decision{Allowed: policy.Mode == Advisory, Status: NotVerified, Reason: ReasonMissing}
	}
	combined := Decide(policy, roots[0].Artifact, roots[0].Verification)
	allowed := combined.Allowed
	for _, root := range roots[1:] {
		decision := Decide(policy, root.Artifact, root.Verification)
		allowed = allowed && decision.Allowed
		if statusRank[decision.Status] > statusRank[combined.Status] {
			combined = decision
		}
	}
	combined.Allowed = allowed
	return combined
}
