package main

import (
	"encoding/json"
	"io"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// fingerprint qualifies an explicit launch file without executing or modifying it.
func (a setupActions) fingerprint(input io.Reader, output io.Writer) error {
	var request struct {
		Binding control.NativeModel `json:"binding"`
	}
	if err := strictjson.DecodeLimited(input, 16384, &request); err != nil {
		return err
	}
	hash, err := a.inspect(request.Binding.LaunchFile, request.Binding)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		SHA256 string `json:"sha256"`
	}{hash})
}
