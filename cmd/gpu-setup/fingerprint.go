package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

var inspectLaunch = runtime.InspectQualifiedNativeLaunch

// fingerprint qualifies an explicit launch file without executing or modifying it.
func fingerprint(input io.Reader, output io.Writer) error {
	data, err := io.ReadAll(io.LimitReader(input, 16385))
	if err != nil {
		return err
	}
	if len(data) > 16384 {
		return errors.New("fingerprint request exceeds 16 KiB")
	}
	var request struct {
		Binding control.NativeModel `json:"binding"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return err
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return errors.New("trailing fingerprint request")
	}
	hash, err := inspectLaunch(request.Binding.LaunchFile, request.Binding)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		SHA256 string `json:"sha256"`
	}{hash})
}
