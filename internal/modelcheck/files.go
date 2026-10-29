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
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"golang.org/x/sys/unix"
)

// MaxJSONFileBytes bounds each JSON file that the checks read, shard indexes
// included. Large tokenizer files and the indexes of large mixture-of-experts
// models are below this limit.
const MaxJSONFileBytes = 128 << 20

// allowedNames are the configuration, tokenizer and template files that a
// safetensors model can hold, and files that no loader reads. Weights and shard indexes match by suffix.
var allowedNames = map[string]bool{
	"config.json": true, "generation_config.json": true, "hf_quant_config.json": true,
	"preprocessor_config.json": true, "processor_config.json": true, "video_preprocessor_config.json": true,
	"tokenizer.json": true, "tokenizer_config.json": true, "tokenizer.model": true,
	"special_tokens_map.json": true, "added_tokens.json": true,
	"vocab.json": true, "vocab.txt": true, "merges.txt": true,
	"chat_template.jinja": true, "chat_template.json": true,
	// Documentation and git attributes that no loader reads.
	"README": true, "README.md": true, "LICENSE": true, "LICENSE.md": true, ".gitattributes": true,
}

// rejectedExtensions name code and pickle formats. The allowlist rejects
// them anyway; the reason makes the report clear.
var rejectedExtensions = map[string]bool{".py": true, ".bin": true, ".pt": true, ".pth": true, ".pkl": true, ".ckpt": true}

// remoteCodeKeys make a loader import code or load other weights.
var remoteCodeKeys = map[string]bool{"auto_map": true, "auto_mapping": true, "custom_pipelines": true}

// adapterConfig makes a loader load the base model that it names, which
// this check never saw.
const adapterConfig = "adapter_config.json"

// Reasons for a failed allowlist check. They name the kind of problem,
// never a path or file content.
const (
	ReasonCodeOrPickleFile = "code-or-pickle-file"
	ReasonUnlistedFile     = "unlisted-file"
	ReasonNestedFile       = "nested-file"
	ReasonAdapterRedirect  = "adapter-redirect"
	ReasonRemoteCode       = "remote-code-mapping"
	ReasonInvalidJSON      = "invalid-json"
	ReasonNoWeights        = "no-safetensors-weights"
	ReasonNoConfiguration  = "no-model-configuration"
)

var (
	errRemoteCode = errors.New("remote code key")
	// errChanged marks a file that changed after the inventory.
	errChanged = errors.New("model files changed during the check")
	// errUnreadable marks a file that the check could not read.
	errUnreadable = errors.New("model file is unreadable")
)

// allowed checks one top-level file name against the allowlist.
func allowed(name string) string {
	switch {
	case name == adapterConfig:
		return ReasonAdapterRedirect
	case rejectedExtensions[path.Ext(name)]:
		return ReasonCodeOrPickleFile
	case strings.HasSuffix(name, ".safetensors"), strings.HasSuffix(name, ".safetensors.index.json"), allowedNames[name]:
		return ""
	}
	return ReasonUnlistedFile
}

// checkAllowlist checks the inventoried files in name order. It reads each
// JSON file and rejects duplicate keys and remote code keys at any depth.
// digests are the inventory SHA-256 values, so the bytes that it reads must
// still match them.
func checkAllowlist(root string, digests map[string]string) (string, error) {
	names := sortedNames(digests)
	weights := false
	for _, name := range names {
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
	for _, name := range names {
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

func checkJSON(file, digest string) (string, error) {
	f, err := openRegular(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxJSONFileBytes+1))
	if err != nil {
		return "", errUnreadable
	}
	if len(raw) > MaxJSONFileBytes {
		return ReasonInvalidJSON, nil
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != digest {
		return "", errChanged
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	err = strictjson.Walk(d, func(key string) error {
		if remoteCodeKeys[key] {
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

func sortedNames(files map[string]string) []string {
	return slices.Sorted(maps.Keys(files))
}

// openRegular opens one regular file without following a final link and
// without blocking on a FIFO.
func openRegular(file string) (*os.File, error) {
	fd, err := unix.Open(file, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errUnreadable
	}
	f := os.NewFile(uintptr(fd), file)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errChanged
	}
	return f, nil
}
