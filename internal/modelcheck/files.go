// Package modelcheck runs the model checks that come before a model can
// serve: a file allowlist, the upstream identity of each file and, later, a
// chat template check and a behavioral scan. It binds each result to the
// exact SHA-256 digests of the model files. A result is not a proof of
// model safety.
package modelcheck

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"golang.org/x/sys/unix"
)

// MaxJSONFileBytes bounds each JSON file that the allowlist reads. Large
// tokenizer files are below this limit.
const MaxJSONFileBytes = 128 << 20

// allowedNames are the configuration, tokenizer, template and adapter files
// that a safetensors model can hold. Weights and shard indexes match by
// suffix.
var allowedNames = map[string]bool{
	"config.json": true, "generation_config.json": true, "hf_quant_config.json": true,
	"preprocessor_config.json": true, "processor_config.json": true, "video_preprocessor_config.json": true,
	"tokenizer.json": true, "tokenizer_config.json": true, "tokenizer.model": true,
	"special_tokens_map.json": true, "added_tokens.json": true,
	"vocab.json": true, "vocab.txt": true, "merges.txt": true,
	"chat_template.jinja": true, "chat_template.json": true,
	"adapter_config.json": true,
}

// rejectedExtensions name code and pickle formats. The allowlist rejects
// them anyway; the reason makes the report clear.
var rejectedExtensions = map[string]bool{".py": true, ".bin": true, ".pt": true, ".pth": true, ".pkl": true, ".ckpt": true}

// Reasons for a failed allowlist check. They name the kind of problem,
// never a path or file content.
const (
	ReasonCodeOrPickleFile = "code-or-pickle-file"
	ReasonUnlistedFile     = "unlisted-file"
	ReasonNestedFile       = "nested-file"
	ReasonRemoteCode       = "remote-code-mapping"
	ReasonInvalidJSON      = "invalid-json"
	ReasonNoWeights        = "no-safetensors-weights"
	ReasonNoConfiguration  = "no-model-configuration"
)

// allowed checks one inventory path against the allowlist.
func allowed(name string) string {
	switch {
	case strings.Contains(name, "/"):
		return ReasonNestedFile
	case rejectedExtensions[path.Ext(name)]:
		return ReasonCodeOrPickleFile
	case strings.HasSuffix(name, ".safetensors"), strings.HasSuffix(name, ".safetensors.index.json"), allowedNames[name]:
		return ""
	}
	return ReasonUnlistedFile
}

// checkAllowlist checks every inventoried file. It reads each JSON file and
// rejects duplicate keys and an auto_map key at any depth, because auto_map
// makes a loader import code. digests are the inventory SHA-256 values, so
// the bytes that it reads must still match them.
func checkAllowlist(root string, digests map[string]string) (string, error) {
	weights := false
	for name := range digests {
		if reason := allowed(name); reason != "" {
			return reason, nil
		}
		weights = weights || strings.HasSuffix(name, ".safetensors")
	}
	if !weights {
		return ReasonNoWeights, nil
	}
	if digests["config.json"] == "" {
		return ReasonNoConfiguration, nil
	}
	for name := range digests {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		reason, err := checkJSON(filepath.Join(root, name), digests[name])
		if err != nil || reason != "" {
			return reason, err
		}
	}
	return "", nil
}

var (
	errRemoteCode = errors.New("auto_map")
	// errChanged marks a file that changed after the inventory.
	errChanged = errors.New("model files changed during the check")
)

func checkJSON(file, digest string) (string, error) {
	f, err := openRegular(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxJSONFileBytes+1))
	if err != nil {
		return "", errors.New("model file is unreadable")
	}
	if len(raw) > MaxJSONFileBytes {
		return ReasonInvalidJSON, nil
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != digest {
		return "", errChanged
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	err = strictjson.Walk(d, func(key string) error {
		if key == "auto_map" {
			return errRemoteCode
		}
		return nil
	})
	if errors.Is(err, errRemoteCode) {
		return ReasonRemoteCode, nil
	}
	if err != nil {
		return ReasonInvalidJSON, nil
	}
	if _, err := d.Token(); err != io.EOF {
		return ReasonInvalidJSON, nil
	}
	return "", nil
}

// openRegular opens one regular file without following a final link and
// without blocking on a FIFO.
func openRegular(file string) (*os.File, error) {
	fd, err := unix.Open(file, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("model file is unreadable")
	}
	f := os.NewFile(uintptr(fd), file)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("model file is not a regular file")
	}
	return f, nil
}
