package modelcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// DefaultHub identifies the default model hub recorded in configuration metadata.
const DefaultHub = "https://huggingface.co"

// MaxTemplateBytes bounds a chat template file read when computing its SHA-256 pin.
const MaxTemplateBytes = 256 << 10

var (
	repoPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Source names the upstream repository and the pinned commit recorded for provenance.
type Source struct {
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
}

// Valid reports whether s names a hub repository as org/name and a 40-hex commit SHA.
func (s Source) Valid() bool {
	return repoPattern.MatchString(s.Repo) && !strings.Contains(s.Repo, "..") && revisionPattern.MatchString(s.Revision)
}

// PinTemplate returns the lowercase hex SHA-256 of template text.
func PinTemplate(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// templateFiles lists the top-level files from which a chat template pin may be derived.
var templateFiles = []string{"chat_template.jinja", "chat_template.json", "tokenizer_config.json"}

// JSON keys rejected by the safetensors model check allowlist.
var (
	RemoteCodeKeys    = []string{"auto_map", "auto_mapping", "custom_pipelines"}
	ModelRedirectKeys = []string{
		"speculators_config", "configuration_files", "attribute_map", "__dict__",
		"audio_model_id", "text_model_id",
		"audio_tokenizer", "audio_tokenizer_class", "audio_tokenizer_name_or_path",
	}
	FileRedirectKeys = []string{"fast_tokenizer_files", "transformers_weights"}
	FilePathKeys     = []string{
		"tokenizer_file", "vocab_file", "merges_file", "spm_file", "added_tokens_file",
		"source_spm", "target_spm", "vocab", "target_vocab_file", "tokenizer_config_file",
		"entity_vocab_file", "monolingual_vocab_file", "emoji_file", "word_shape_file",
		"word_pronunciation_file", "pretrained_vocab_file",
	}
)

var (
	errShardIndexTooLarge   = errors.New("shard index exceeds its size limit")
	errShardIndexChanged    = errors.New("shard index changed after the inventory")
	errShardIndexUnreadable = errors.New("shard index is unreadable")
)

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// ContextReader stops reading as soon as ctx is canceled or times out.
func ContextReader(ctx context.Context, r io.Reader) io.Reader {
	return contextReader{ctx: ctx, r: r}
}

// FileDigest computes the SHA-256 hex digest of a regular file without following symlinks.
func FileDigest(ctx context.Context, path string) (string, error) {
	if !cleanAbsolute(path) {
		return "", errors.New("file digest requires a clean absolute path")
	}
	f, err := openRegular(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, ContextReader(ctx, io.LimitReader(f, 1<<40))); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func verifyShardIndex(ctx context.Context, root string, files map[string]string, name string, limit int64) error {
	f, err := openRegular(filepath.Join(root, name))
	if err != nil {
		return errShardIndexUnreadable
	}
	defer f.Close()
	raw, err := io.ReadAll(ContextReader(ctx, io.LimitReader(f, limit+1)))
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return errShardIndexUnreadable
	}
	if int64(len(raw)) > limit {
		return errShardIndexTooLarge
	}
	if digest := sha256.Sum256(raw); hex.EncodeToString(digest[:]) != files[name] {
		return errShardIndexChanged
	}
	return validateShardWeightMap(raw, files, limit)
}

func validateShardWeightMap(raw []byte, files map[string]string, limit int64) error {
	var index map[string]json.RawMessage
	if !utf8.Valid(raw) || strictjson.DecodeLimited(bytes.NewReader(raw), limit, &index) != nil {
		return errors.New("shard index is invalid")
	}
	var weights map[string]string
	if json.Unmarshal(index["weight_map"], &weights) != nil || len(weights) == 0 {
		return errors.New("shard index requires a nonempty weight map")
	}
	for _, target := range weights {
		if !filepath.IsLocal(target) || strings.ContainsAny(target, `/\`) || filepath.Ext(target) != ".safetensors" || files[target] == "" {
			return errors.New("shard index must reference inventoried safetensors weights")
		}
	}
	return nil
}

// resolveTemplatePin hashes chat_template.jinja or an embedded string chat_template if present.
func resolveTemplatePin(ctx context.Context, root string, files map[string]string) (pin, file, reason string, err error) {
	if files["chat_template.jinja"] != "" {
		raw, r, readErr := readBound(ctx, filepath.Join(root, "chat_template.jinja"), files["chat_template.jinja"], MaxTemplateBytes, "chat-template-too-large")
		if readErr != nil || r != "" {
			return "", "", r, readErr
		}
		if !utf8.Valid(raw) {
			return "", "", "chat-template-invalid", nil
		}
		return PinTemplate(string(raw)), "chat_template.jinja", "", nil
	}
	for _, name := range []string{"chat_template.json", "tokenizer_config.json"} {
		if files[name] == "" {
			continue
		}
		p, r, readErr := extractJSONTemplatePin(ctx, root, name, files[name])
		if readErr != nil || r != "" || p != "" {
			return p, name, r, readErr
		}
	}
	return "", "", "", nil
}

func extractJSONTemplatePin(ctx context.Context, root, name, digest string) (string, string, error) {
	raw, reason, err := readBound(ctx, filepath.Join(root, name), digest, MaxJSONFileBytes, ReasonJSONTooLarge)
	if err != nil || reason != "" {
		return "", reason, err
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return "", ReasonInvalidJSON, nil
	}
	rawTpl, ok := doc["chat_template"]
	if !ok || string(rawTpl) == "null" {
		return "", "", nil
	}
	var text string
	if json.Unmarshal(rawTpl, &text) != nil {
		return "", "named-chat-templates", nil
	}
	if len(text) > MaxTemplateBytes {
		return "", "chat-template-too-large", nil
	}
	return PinTemplate(text), "", nil
}
