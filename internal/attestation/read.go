package attestation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"strconv"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// document names one strict JSON input so errors locate it without content.
type document struct {
	name    string
	limit   int64
	version int
	invalid error
}

var (
	envelopeDocument  = document{name: "envelope", limit: MaxEnvelopeBytes, version: FormatVersion, invalid: ErrMalformedEnvelope}
	signatureDocument = document{name: "signature file", limit: MaxSignatureBytes, version: SignatureFormatVersion, invalid: ErrMalformedSignature}
	trustDocument     = document{name: "trust config", limit: MaxTrustConfigBytes, version: TrustConfigVersion, invalid: ErrInvalidTrust}
	policyDocument    = document{name: "policy", limit: MaxPolicyBytes, version: PolicyFileVersion, invalid: ErrInvalidPolicy}
)

// read decodes one bounded document into v. A well-formed document with
// another version reports ErrUnsupportedVersion before any field checks.
func (d document) read(r io.Reader, v any, encode func() ([]byte, error)) error {
	raw, err := d.readLimited(r)
	if err != nil {
		return err
	}
	if otherVersion(raw, d.version) {
		return fmt.Errorf("%w: %s", ErrUnsupportedVersion, d.name)
	}
	return d.decodeExact(raw, v, encode)
}

// readLimited separates reader failures from oversized or non-UTF-8 input.
// Reader errors keep only their class, never their text.
func (d document) readLimited(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: %s", ErrMissingInput, d.name)
	}
	raw, err := io.ReadAll(io.LimitReader(r, d.limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %s", ErrUnreadable, d.name, readErrorClass(err))
	}
	if int64(len(raw)) > d.limit {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", d.invalid, d.name, d.limit)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: %s is not UTF-8", d.invalid, d.name)
	}
	return raw, nil
}

func readErrorClass(err error) string {
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "permission"
	case errors.As(err, &timeout) && timeout.Timeout():
		return "timeout"
	}
	return "other"
}

// decodeExact strictly decodes raw into v, then needs raw to decode to the
// same JSON value as encode's output. Errors from encode return unchanged.
func (d document) decodeExact(raw []byte, v any, encode func() ([]byte, error)) error {
	if strictjson.DecodeLimited(bytes.NewReader(raw), d.limit, v) != nil {
		return fmt.Errorf("%w: %s is not strict JSON", d.invalid, d.name)
	}
	canonical, err := encode()
	if err != nil {
		return err
	}
	if !sameJSON(raw, canonical) {
		return fmt.Errorf("%w: %s fields differ from the canonical form", d.invalid, d.name)
	}
	return nil
}

// otherVersion reports one well-formed JSON object whose exact "version"
// member is a positive int64 other than current.
func otherVersion(raw []byte, current int) bool {
	if strictjson.Check(json.NewDecoder(bytes.NewReader(raw))) != nil {
		return false
	}
	var object map[string]json.RawMessage
	d := json.NewDecoder(bytes.NewReader(raw))
	if d.Decode(&object) != nil || d.Decode(&struct{}{}) != io.EOF {
		return false
	}
	version, err := strconv.ParseInt(string(object["version"]), 10, 64)
	return err == nil && version >= 1 && version != int64(current)
}

// sameJSON compares decoded values, so key order, whitespace and string
// escapes can differ while names, nulls and omitted fields cannot.
func sameJSON(supplied, expected []byte) bool {
	decode := func(raw []byte) (any, error) {
		var value any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		err := d.Decode(&value)
		return value, err
	}
	got, err := decode(supplied)
	if err != nil {
		return false
	}
	want, err := decode(expected)
	return err == nil && reflect.DeepEqual(got, want)
}
