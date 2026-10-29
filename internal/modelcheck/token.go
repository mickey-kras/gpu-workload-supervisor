package modelcheck

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxTokenBytes bounds the token file of a hub login.
const maxTokenBytes = 4096

// hubToken gives the token of an existing Hugging Face login, or nothing.
// The order follows the hub tool: HF_TOKEN, HF_TOKEN_PATH, HF_HOME/token,
// then the default cache. The checks send it only to the hub. They never
// store, log or show it.
func hubToken() string {
	if token := cleanToken(os.Getenv("HF_TOKEN")); token != "" {
		return token
	}
	for _, path := range tokenPaths() {
		if token := readToken(path); token != "" {
			return token
		}
	}
	return ""
}

func tokenPaths() []string {
	if path := os.Getenv("HF_TOKEN_PATH"); path != "" {
		return []string{path}
	}
	if home := os.Getenv("HF_HOME"); home != "" {
		return []string{filepath.Join(home, "token")}
	}
	cache := os.Getenv("XDG_CACHE_HOME")
	if cache == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		cache = filepath.Join(home, ".cache")
	}
	return []string{filepath.Join(cache, "huggingface", "token")}
}

func readToken(path string) string {
	if !filepath.IsAbs(path) {
		return ""
	}
	f, err := openRegular(filepath.Clean(path))
	if err != nil {
		return ""
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxTokenBytes+1))
	if err != nil || len(raw) > maxTokenBytes {
		return ""
	}
	return cleanToken(string(raw))
}

// cleanToken accepts one printable token without spaces, so it cannot
// change the request header.
func cleanToken(value string) string {
	value = strings.TrimSpace(value)
	for i := 0; i < len(value); i++ {
		if value[i] <= ' ' || value[i] > '~' {
			return ""
		}
	}
	return value
}
