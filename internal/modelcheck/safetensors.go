package modelcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

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
	digest, _, err := digestFile(ctx, path)
	return digest, err
}

// digestFile also gives the file identity. The identity must be the same
// before and after the read, so the digest belongs to that identity.
func digestFile(ctx context.Context, path string) (string, FileStat, error) {
	if !cleanAbsolute(path) {
		return "", FileStat{}, errors.New("file digest requires a clean absolute path")
	}
	if err := noParentLinks(path); err != nil {
		return "", FileStat{}, err
	}
	f, err := openRegular(path)
	if err != nil {
		return "", FileStat{}, err
	}
	defer f.Close()
	const maxFileBytes int64 = 1 << 40
	before, err := statOf(f)
	if err != nil {
		return "", FileStat{}, err
	}
	if before.Size > maxFileBytes {
		return "", FileStat{}, errors.New("model file exceeds its size limit")
	}
	h := sha256.New()
	n, err := io.Copy(h, ContextReader(ctx, io.LimitReader(f, maxFileBytes+1)))
	if err != nil {
		return "", FileStat{}, err
	}
	after, err := statOf(f)
	if err != nil || n != before.Size || after != before {
		return "", FileStat{}, errors.New("model file changed or exceeds its size limit")
	}
	return hex.EncodeToString(h.Sum(nil)), before, nil
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
