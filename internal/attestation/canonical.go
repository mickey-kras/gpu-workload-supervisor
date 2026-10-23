package attestation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
)

// Domain prefixes keep envelope signatures and artifact digests from being
// valid for any other message format.
const (
	SigningDomain  = "gpu-model-attestation/v1\n"
	ArtifactDomain = "gpu-model-artifact/v1\n"
)

// SigningBytes returns the exact bytes an offline signer signs: SigningDomain
// followed by the canonical JSON of the validated envelope.
func SigningBytes(e Envelope) ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	canonical, err := canonicalJSON(e.object())
	if err != nil {
		return nil, err
	}
	return append([]byte(SigningDomain), canonical...), nil
}

// Digest identifies the complete artifact for exemptions: lowercase hex
// SHA-256 of ArtifactDomain followed by the artifact's canonical JSON.
func (a Artifact) Digest() (string, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}
	canonical, err := canonicalJSON(a.object())
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(ArtifactDomain), canonical...))
	return hex.EncodeToString(sum[:]), nil
}

func (e Envelope) object() map[string]any {
	object := map[string]any{
		"version":       e.Version,
		"artifact":      e.Artifact.object(),
		"scan":          e.Scan.object(),
		"policyVersion": e.PolicyVersion,
		"timestamp":     e.Timestamp,
	}
	if e.Source != nil {
		source := map[string]any{}
		putText(source, "uri", e.Source.URI)
		putText(source, "revision", e.Source.Revision)
		object["source"] = source
	}
	return object
}

func (a Artifact) object() map[string]any {
	if a.SHA256 != "" {
		return map[string]any{"sha256": a.SHA256}
	}
	files := make(map[string]any, len(a.Files))
	for path, digest := range a.Files {
		files[path] = digest
	}
	return map[string]any{"files": files}
}

func (s Scan) object() map[string]any {
	object := map[string]any{"status": string(s.Status)}
	if s.Scanner != nil {
		object["scanner"] = map[string]any{"tool": s.Scanner.Tool, "version": s.Scanner.Version, "configSHA256": s.Scanner.ConfigSHA256}
	}
	putText(object, "coverage", string(s.Coverage))
	putText(object, "verdict", s.Verdict)
	putText(object, "reportSHA256", s.ReportSHA256)
	return object
}

func putText(object map[string]any, key, value string) {
	if value != "" {
		object[key] = value
	}
}

// canonicalJSON writes objects with members sorted by key bytes, no
// insignificant whitespace, unescaped strings and decimal integers. For the
// restricted envelope alphabet this is the RFC 8785 serialization.
func canonicalJSON(value any) ([]byte, error) {
	var out bytes.Buffer
	if err := writeCanonical(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func writeCanonical(out *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case map[string]any:
		return writeObject(out, v)
	case string:
		if !text(v, MaxEnvelopeBytes) {
			return errors.New("attestation string needs JSON escapes")
		}
		out.WriteByte('"')
		out.WriteString(v)
		out.WriteByte('"')
	case int:
		out.WriteString(strconv.Itoa(v))
	default:
		return errors.New("unsupported canonical attestation value")
	}
	return nil
}

func writeObject(out *bytes.Buffer, object map[string]any) error {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		if err := writeCanonical(out, key); err != nil {
			return err
		}
		out.WriteByte(':')
		if err := writeCanonical(out, object[key]); err != nil {
			return err
		}
	}
	out.WriteByte('}')
	return nil
}
