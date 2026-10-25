// Package strictjson rejects duplicate object keys that encoding/json silently
// resolves by keeping the last value.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var ErrDuplicateKey = errors.New("duplicate JSON key")

// Match encoding/json's scanner limit. Decoder.Token does not enforce it
// for delimiters, so the duplicate-key walk must bound its own recursion.
const maxNestingDepth = 10000

// DecodeLimited reads at most max bytes from r and decodes one JSON value
// into v, rejecting duplicate keys, unknown fields, and trailing values.
func DecodeLimited(r io.Reader, max int64, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > max {
		return fmt.Errorf("json exceeds %d bytes", max)
	}
	if err := Check(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("json must contain one JSON value")
	}
	return nil
}

// Check consumes one JSON value from d and rejects duplicate object keys at
// any depth, including inside arrays. Nesting is limited as in encoding/json.
func Check(d *json.Decoder) error {
	return check(d, 0)
}

func check(d *json.Decoder, depth int) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if depth >= maxNestingDepth {
		return errors.New("json exceeds maximum nesting depth")
	}
	switch delim {
	case '{':
		return checkObject(d, depth+1)
	case '[':
		return checkArray(d, depth+1)
	default:
		return errors.New("invalid JSON value")
	}
}

func checkObject(d *json.Decoder, depth int) error {
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return ErrDuplicateKey
		}
		seen[name] = true
		if err := check(d, depth); err != nil {
			return err
		}
	}
	_, err := d.Token()
	return err
}

func checkArray(d *json.Decoder, depth int) error {
	for d.More() {
		if err := check(d, depth); err != nil {
			return err
		}
	}
	_, err := d.Token()
	return err
}
