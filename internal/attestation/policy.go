package attestation

import (
	"errors"
	"fmt"
)

type Mode string

const (
	Advisory Mode = "advisory"
	Enforce  Mode = "enforce"
)

// ExemptionKind names the one unverified outcome an exemption admits.
type ExemptionKind string

const (
	ExemptNotScanned      ExemptionKind = "not-scanned"
	ExemptUnsupported     ExemptionKind = "unsupported"
	ExemptPartialCoverage ExemptionKind = "partial-coverage"
	ExemptFindings        ExemptionKind = "findings"
)

// Exemption admits one outcome for one artifact digest under one policy
// version. It never admits scan errors, integrity failures, unreadable or
// unsupported evidence, or a missing attestation.
type Exemption struct {
	Artifact      string        `json:"artifact"`
	PolicyVersion int           `json:"policyVersion"`
	Kind          ExemptionKind `json:"kind"`
	Reason        string        `json:"reason"`
}

// Policy comes from trusted configuration. Mode has no implicit default:
// Decide treats every value other than Advisory as Enforce.
type Policy struct {
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
	ReasonUnknownResult      Reason = "unknown-result"
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

type Decision struct {
	Allowed bool   `json:"allowed"`
	Status  Status `json:"status"`
	Reason  Reason `json:"reason"`
}

// Validate needs an explicit mode. The configuration loader applies and
// shows the advisory default; the zero Policy fails closed in Decide.
func (p Policy) Validate() error {
	if p.Mode != Advisory && p.Mode != Enforce {
		return errors.New("attestation policy mode must be advisory or enforce")
	}
	seen := map[Exemption]bool{}
	for index, exemption := range p.Exemptions {
		if !exemption.valid() {
			return fmt.Errorf("attestation exemption %d needs a digest, a policy version of 1 to %d, a known kind and a reason of at most %d bytes", index, MaxPolicyVersion, MaxTextBytes)
		}
		scope := Exemption{Artifact: exemption.Artifact, PolicyVersion: exemption.PolicyVersion, Kind: exemption.Kind}
		if seen[scope] {
			return fmt.Errorf("attestation exemption %d duplicates an earlier scope", index)
		}
		seen[scope] = true
	}
	denied := map[string]bool{}
	for index, digest := range p.Deny {
		if !digestPattern.MatchString(digest) || denied[digest] {
			return fmt.Errorf("attestation denial %d must be a unique artifact digest", index)
		}
		denied[digest] = true
	}
	return nil
}

func (e Exemption) valid() bool {
	switch e.Kind {
	case ExemptNotScanned, ExemptUnsupported, ExemptPartialCoverage, ExemptFindings:
	default:
		return false
	}
	return digestPattern.MatchString(e.Artifact) && validPolicyVersion(e.PolicyVersion) && safeText(e.Reason, MaxTextBytes)
}

// Decide maps one verification to admission. Pass nil when no attestation
// exists. Only a sealed Valid verification can reach the scan outcome.
func Decide(policy Policy, verification *Verification) Decision {
	permissive := policy.Mode == Advisory
	if verification == nil {
		return Decision{Allowed: permissive, Status: NotVerified, Reason: ReasonMissing}
	}
	if !verification.sealed {
		return Decision{Status: IntegrityFailure, Reason: ReasonUnsealed}
	}
	switch verification.result {
	case Valid:
	case Unreadable, UnsupportedVersion:
		return Decision{Allowed: permissive, Status: NotVerified, Reason: Reason(verification.result)}
	case Malformed, InvalidInventory, InvalidSignature, UnknownKey, ArtifactMismatch, IncompatiblePolicy:
		return Decision{Status: IntegrityFailure, Reason: Reason(verification.result)}
	default:
		return Decision{Status: IntegrityFailure, Reason: ReasonUnknownResult}
	}
	envelope := verification.envelope
	digest, err := envelope.Artifact.Digest()
	if err != nil {
		return Decision{Status: IntegrityFailure, Reason: ReasonMalformed}
	}
	if policy.denies(digest) {
		return Decision{Status: IntegrityFailure, Reason: ReasonArtifactDenied}
	}
	return decideScan(policy, envelope, digest)
}

func decideScan(policy Policy, envelope Envelope, digest string) Decision {
	permissive := policy.Mode == Advisory
	scan := envelope.Scan
	var kind ExemptionKind
	var reason Reason
	unexempted := Decision{Allowed: permissive, Status: NotVerified}
	switch {
	case scan.Status == ScanPassed && scan.Coverage == CoverageComplete:
		return Decision{Allowed: true, Status: Verified, Reason: ReasonPassed}
	case scan.Status == ScanError:
		return Decision{Allowed: permissive, Status: NotVerified, Reason: ReasonScanError}
	case scan.Status == ScanFindings:
		kind, reason = ExemptFindings, ReasonFindings
		unexempted = Decision{Status: FindingsDetected}
	case scan.Status == ScanPassed:
		kind, reason = ExemptPartialCoverage, ReasonPartialCoverage
	case scan.Status == ScanNotScanned:
		kind, reason = ExemptNotScanned, ReasonNotScanned
	case scan.Status == ScanUnsupported:
		kind, reason = ExemptUnsupported, ReasonUnsupported
	default:
		return Decision{Status: IntegrityFailure, Reason: ReasonMalformed}
	}
	if policy.exempts(digest, envelope.PolicyVersion, kind) {
		return Decision{Allowed: true, Status: Exempted, Reason: reason}
	}
	unexempted.Reason = reason
	return unexempted
}

func (p Policy) denies(digest string) bool {
	for _, denied := range p.Deny {
		if denied == digest {
			return true
		}
	}
	return false
}

func (p Policy) exempts(digest string, policyVersion int, kind ExemptionKind) bool {
	for _, exemption := range p.Exemptions {
		if exemption.Artifact == digest && exemption.PolicyVersion == policyVersion && exemption.Kind == kind {
			return true
		}
	}
	return false
}

// statusRank orders combined decisions from most to least severe.
var statusRank = map[Status]int{IntegrityFailure: 4, FindingsDetected: 3, NotVerified: 2, Exempted: 1, Verified: 0}

// DecideAll combines one verification per inventory, such as separate model
// and tokenizer roots. The most severe decision wins and any rejection
// rejects the whole set. An empty set is a missing attestation.
func DecideAll(policy Policy, verifications []*Verification) Decision {
	if len(verifications) == 0 {
		return Decide(policy, nil)
	}
	combined := Decide(policy, verifications[0])
	allowed := combined.Allowed
	for _, verification := range verifications[1:] {
		decision := Decide(policy, verification)
		allowed = allowed && decision.Allowed
		if statusRank[decision.Status] > statusRank[combined.Status] {
			combined = decision
		}
	}
	combined.Allowed = allowed
	return combined
}
