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
		return "", fmt.Errorf("%w: %s: path must be absolute and clean", ErrInvalid, path)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: %s does not exist; create it with mode 0600 and one line %s=<key>, or run setup again for a supervisor-managed model", ErrInvalid, path, Name)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := checkOwnership(info); err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrInvalid, path, err)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrInvalid, path, err)
	}
	if len(raw) > maxFileBytes {
		return "", fmt.Errorf("%w: %s: the file is larger than %d bytes", ErrInvalid, path, maxFileBytes)
	}
	key, err := parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrInvalid, path, err)
	}
	return key, nil
}

func checkOwnership(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return errors.New("the file mode must be 0600")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != effectiveUID() {
		return errors.New("the current user must own the file")
	}
	return nil
}

func parse(raw []byte) (string, error) {
	content := strings.TrimSuffix(string(raw), "\n")
	value, ok := strings.CutPrefix(content, Name+"=")
	if !ok || !token.MatchString(value) {
		return "", fmt.Errorf("the file must hold one line %s=<key> with 32 to 256 URL-safe characters", Name)
	}
	return value, nil
}

// Ensure keeps a usable key file and creates a new private key file when none
// exists. It never replaces a file that exists but is not usable.
func Ensure(path string) error {
	_, readErr := Read(path)
	if readErr == nil {
		return nil
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w; correct or remove the file, then run setup again", readErr)
	}
	dir := filepath.Dir(path)
	if err := makeDirectories(dir); err != nil {
		return fmt.Errorf("create the key directory for %s: %w", path, err)
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
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("publish key file: %w", err)
	}
	_, err = Read(path)
	return err
}

// makeDirectories creates the missing directories of dir with mode 0700 and
// makes each new entry durable in its parent.
func makeDirectories(dir string) error {
	var missing []string
	for current := dir; ; current = filepath.Dir(current) {
		if _, err := os.Lstat(current); err == nil || current == filepath.Dir(current) {
			break
		}
		missing = append(missing, current)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, created := range missing {
		if err := syncDirectory(filepath.Dir(created)); err != nil {
			return err
		}
	}
	return nil
}

// syncDirectory makes the entries of a directory durable. Test hook.
var syncDirectory = func(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
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
