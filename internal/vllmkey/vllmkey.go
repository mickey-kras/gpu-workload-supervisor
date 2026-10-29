// Package vllmkey reads and creates the private file that gives vLLM its API
// key. The key reaches vLLM only through VLLM_API_KEY in a systemd
// EnvironmentFile, never through the command line.
package vllmkey

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// Name is the only variable that the key file can set.
const Name = "VLLM_API_KEY"

const maxFileBytes = 512

var (
	// ErrInvalid tells that the key file is missing, unsafe or malformed.
	// Messages never contain the key.
	ErrInvalid = errors.New("vLLM API key file is not usable")
	token      = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)
)

// Test hooks.
var (
	effectiveUID = os.Geteuid
	randomRead   = rand.Read
)

// Read returns the key from a regular file that the current user owns, that
// only the owner can read or write, and that holds exactly one VLLM_API_KEY
// assignment.
func Read(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("%w: path must be absolute and clean", ErrInvalid)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("%w: open failed; run setup again to create it", ErrInvalid)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("%w: metadata could not be read", ErrInvalid)
	}
	if err := checkOwnership(info); err != nil {
		return "", err
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil || len(raw) > maxFileBytes {
		return "", fmt.Errorf("%w: content could not be read or is too large", ErrInvalid)
	}
	return parse(raw)
}

func checkOwnership(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: not a regular file", ErrInvalid)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%w: permissions must be 0600", ErrInvalid)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != effectiveUID() {
		return fmt.Errorf("%w: the current user must own the file", ErrInvalid)
	}
	return nil
}

func parse(raw []byte) (string, error) {
	content := strings.TrimSuffix(string(raw), "\n")
	value, ok := strings.CutPrefix(content, Name+"=")
	if !ok || !token.MatchString(value) {
		return "", fmt.Errorf("%w: the file must hold one line %s=<key> with 32 to 256 URL-safe characters", ErrInvalid, Name)
	}
	return value, nil
}

// Ensure keeps a usable key file and creates a new private key file when none
// exists. It never replaces a file that exists but is not usable.
func Ensure(path string) error {
	if _, err := Read(path); err == nil {
		return nil
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: an existing file is not usable; correct or remove it, then run setup again", ErrInvalid)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create key directory: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := randomRead(secret); err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	content := Name + "=" + base64.RawURLEncoding.EncodeToString(secret) + "\n"
	return publish(dir, path, []byte(content))
}

// publish writes a temporary file and links it into place, so the key file
// never exists in a partial state and an existing file is never replaced.
func publish(dir, path string, content []byte) error {
	temporary, err := os.CreateTemp(dir, ".vllm-key-*")
	if err != nil {
		return fmt.Errorf("create key file: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := writeSynced(temporary, content); err != nil {
		return err
	}
	if err := os.Link(name, path); err != nil {
		return fmt.Errorf("publish key file: %w", err)
	}
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	_, err = Read(path)
	return err
}

func writeSynced(file *os.File, content []byte) error {
	err := file.Chmod(0o600)
	if err == nil {
		_, err = file.Write(content)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write key file: %w", err)
	}
	return nil
}
