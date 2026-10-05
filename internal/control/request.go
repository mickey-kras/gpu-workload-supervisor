package control

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxRequestIDBytes leaves room in the 64 KiB completion callback for the fence,
// registration token and outcome even when every ID byte uses a six-byte JSON
// escape (8192 * 6 = 49152). The limit applies after proxy whitespace trimming.
const MaxRequestIDBytes = 8192

var ErrRequestIDTooLong = errors.New("request id exceeds 8192 bytes")

// ValidateRequestID validates new registrations without changing their identity.
// UTF-8 is required because completion JSON cannot round-trip invalid UTF-8, and
// control characters are rejected because the ID is forwarded as an HTTP header.
func ValidateRequestID(requestID string) error {
	if requestID == "" {
		return errors.New("request id is empty")
	}
	if len(requestID) > MaxRequestIDBytes {
		return ErrRequestIDTooLong
	}
	if !utf8.ValidString(requestID) {
		return errors.New("request id must be valid UTF-8")
	}
	if strings.IndexFunc(requestID, unicode.IsControl) >= 0 {
		return errors.New("request id contains control characters")
	}
	return nil
}
