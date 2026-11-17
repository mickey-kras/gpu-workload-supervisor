package modelcheck

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxTokenBytes bounds the token file of a hub login.
const maxTokenBytes = 4096

// errTokenUnusable marks a configured token that cannot be used. It never
// holds the token or its path.
var errTokenUnusable = errors.New("hub login token is configured but unusable")

// hubToken gives the token of an existing Hugging Face login. The first
// location that is configured decides; later locations are not tried:
//
//  1. HF_TOKEN, when it is set and not empty.
//  2. The file at HF_TOKEN_PATH, when it is set. It must be absolute.
//  3. The file token in HF_HOME, when HF_HOME is set. It must be absolute.
//  4. huggingface/token in XDG_CACHE_HOME when it is set and absolute, else
//     in ~/.cache.
//
// A missing file in step 3 or 4 means no login: the request has no token. A
// token that is set but cannot be used gives errTokenUnusable, so the check
// does not fall back to an anonymous request. The token file must be a
// regular file, not a link, of at most 4096 bytes.
func hubToken() (string, error) {
	if value := os.Getenv("HF_TOKEN"); value != "" {
		return usable(cleanToken(value))
	}
	if path, set := os.LookupEnv("HF_TOKEN_PATH"); set && path != "" {
		return readToken(path, true)
	}
	if home, set := os.LookupEnv("HF_HOME"); set && home != "" {
		return readToken(filepath.Join(home, "token"), false)
	}
	cache := os.Getenv("XDG_CACHE_HOME")
	if !filepath.IsAbs(cache) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil
		}
		cache = filepath.Join(home, ".cache")
	}
	return readToken(filepath.Join(cache, "huggingface", "token"), false)
}

func usable(token string) (string, error) {
	if token == "" {
		return "", errTokenUnusable
	}
	return token, nil
}

// readToken reads one token file. required makes a missing file unusable.
func readToken(path string, required bool) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errTokenUnusable
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) && !required {
		return "", nil
	}
	f, err := openRegular(filepath.Clean(path))
	if err != nil {
		return "", errTokenUnusable
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxTokenBytes+1))
	if err != nil || len(raw) > maxTokenBytes {
		return "", errTokenUnusable
	}
	return usable(cleanToken(string(raw)))
}

// cleanToken accepts one printable token without spaces, so it cannot
// change the request header. Anything else gives an empty string.
func cleanToken(value string) string {
	value = strings.TrimSpace(value)
	for i := 0; i < len(value); i++ {
		if value[i] <= ' ' || value[i] > '~' {
			return ""
		}
	}
	return value
}
