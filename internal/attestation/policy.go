package attestation

import "errors"

type Mode string

const (
	Advisory Mode = "advisory"
	Enforce  Mode = "enforce"
)

// Exemption admits one unscanned or unsupported artifact under one policy
// version. It never admits findings, scan errors or integrity failures.
type Exemption struct {
	Artifact      string `json:"artifact"`
	PolicyVersion int    `json:"policyVersion"`
}

// Policy comes from trusted configuration. The zero Mode is Advisory.
type Policy struct {
	Mode       Mode        `json:"mode"`
	Exemptions []Exemption `json:"exemptions"`
}

type Status string

const (
	Verified         Status = "verified"
	NotVerified      Status = "not-verified"
	FindingsDetected Status = "findings-detected"
	IntegrityFailure Status = "integrity-failure"
	Exempted         Status = "exempted"
)

// Decision reasons are stable codes for logs and UI.
const (
	ReasonMissing         = "attestation-missing"
	ReasonPassed          = "scan-passed"
	ReasonFindings        = "scan-findings"
	ReasonScanError       = "scan-error"
	ReasonNotScanned      = "scan-not-scanned"
	ReasonUnsupported     = "scan-unsupported"
	ReasonPartialCoverage = "scan-partial-coverage"
)

type Decision struct {
	Allowed bool
	Status  Status
	Reason  string
}

func (p Policy) Validate() error {
	if p.Mode != "" && p.Mode != Advisory && p.Mode != Enforce {
		return errors.New("attestation policy mode must be advisory or enforce")
	}
	for _, exemption := range p.Exemptions {
		if !digestPattern.MatchString(exemption.Artifact) || exemption.PolicyVersion < 1 {
			return errors.New("invalid attestation exemption")
		}
	}
	return nil
}

// Decide maps verification evidence to admission. Pass nil when no
// attestation exists. An unknown mode fails closed as Enforce.
func Decide(policy Policy, verification *Verification) Decision {
	permissive := policy.Mode == "" || policy.Mode == Advisory
	if verification == nil {
		return Decision{Allowed: permissive, Status: NotVerified, Reason: ReasonMissing}
	}
	if verification.Result != Valid {
		return Decision{Status: IntegrityFailure, Reason: string(verification.Result)}
	}
	scan := verification.Envelope.Scan
	var reason string
	switch {
	case scan.Status == ScanPassed && scan.Coverage == CoverageComplete:
		return Decision{Allowed: true, Status: Verified, Reason: ReasonPassed}
	case scan.Status == ScanFindings:
		return Decision{Status: FindingsDetected, Reason: ReasonFindings}
	case scan.Status == ScanError:
		return Decision{Allowed: permissive, Status: NotVerified, Reason: ReasonScanError}
	case scan.Status == ScanPassed:
		reason = ReasonPartialCoverage
	case scan.Status == ScanNotScanned:
		reason = ReasonNotScanned
	case scan.Status == ScanUnsupported:
		reason = ReasonUnsupported
	default:
		return Decision{Status: IntegrityFailure, Reason: string(Malformed)}
	}
	if policy.exempts(verification.Envelope) {
		return Decision{Allowed: true, Status: Exempted, Reason: reason}
	}
	return Decision{Allowed: permissive, Status: NotVerified, Reason: reason}
}

func (p Policy) exempts(envelope Envelope) bool {
	digest, err := envelope.Artifact.Digest()
	if err != nil {
		return false
	}
	for _, exemption := range p.Exemptions {
		if exemption.Artifact == digest && exemption.PolicyVersion == envelope.PolicyVersion {
			return true
		}
	}
	return false
}
