// Package strictjson rejects duplicate object keys that encoding/json silently
// resolves by keeping the last value.
package strictjson

import (
	"encoding/json"
	"errors"
)

var ErrDuplicateKey = errors.New("duplicate JSON key")

// Check consumes one JSON value from d and rejects duplicate object keys at
// any depth, including inside arrays.
func Check(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
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
			if err := Check(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := Check(d); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON value")
	}
	_, err = d.Token()
	return err
}
