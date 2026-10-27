package control

import "strconv"

// CanonicalUint parses a decimal uint64 with no sign, spaces or leading zeros.
// Kernel and systemd counters use this form.
func CanonicalUint(text string) (uint64, bool) {
	value, err := strconv.ParseUint(text, 10, 64)
	return value, err == nil && text == strconv.FormatUint(value, 10)
}
