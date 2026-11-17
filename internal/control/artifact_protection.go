package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
)

// ArtifactProtectionVersion is the only accepted artifact protection
// contract.
const ArtifactProtectionVersion = 1

// ErrArtifactsRequireV7 rejects artifact protection in a catalog version
// that predates it.
var ErrArtifactsRequireV7 = errors.New("model artifact protection requires catalog version 7")

// ErrEmptyArtifactProtection rejects a present but null or empty artifact
// protection object. omitzero would otherwise decode it to the absent form
// and drop the key, so the accepted bytes could not round-trip.
var ErrEmptyArtifactProtection = errors.New("artifact protection object must not be empty")

// ArtifactProtection requests read-only pinning of the launch's model
// artifacts: the model directory, the pinned chat template and the private
// environment file. The zero value keeps the exact legacy unit bytes and
// fingerprints; a present value is a protection claim the runtime must
// verify before every admit, start and retain. The type is a comparable
// value struct, like ServingOptions.
type ArtifactProtection struct {
	Version int `json:"version"`
}

// UnmarshalJSON decodes the artifact protection settings strictly in every
// decoder and rejects the empty forms with ErrEmptyArtifactProtection.
func (a *ArtifactProtection) UnmarshalJSON(raw []byte) error {
	type plain ArtifactProtection
	var decoded plain
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&decoded); err != nil {
		return err
	}
	if ArtifactProtection(decoded).IsZero() {
		return ErrEmptyArtifactProtection
	}
	*a = ArtifactProtection(decoded)
	return nil
}

// IsZero reports absent artifact protection, keeping legacy owned unit
// bytes and fingerprints byte-exact.
func (a ArtifactProtection) IsZero() bool {
	return a == ArtifactProtection{}
}

// Validate checks the artifact protection settings of one owned launch.
func (a ArtifactProtection) Validate() error {
	if a.IsZero() {
		return nil
	}
	if a.Version != ArtifactProtectionVersion {
		return errors.New("unsupported artifact protection version")
	}
	return nil
}

// HasArtifactProtection reports whether the profile's owned launch carries
// artifact protection. It is the one test for the version rule. The setup
// client mirrors it in clients/setup/review.mjs.
func (p WorkloadProfile) HasArtifactProtection() bool {
	return p.NativeModel != nil && p.NativeModel.Owned != nil && !p.NativeModel.Owned.Artifacts.IsZero()
}

// HasArtifactProtection reports whether any owned launch carries artifact
// protection.
func (c Catalog) HasArtifactProtection() bool {
	return slices.ContainsFunc(c.Profiles, WorkloadProfile.HasArtifactProtection)
}
