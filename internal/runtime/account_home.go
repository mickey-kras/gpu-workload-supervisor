package runtime

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// localAccountHome deliberately avoids os/user.Current and LookupId: static
// builds may synthesize the current account from USER and HOME. NSS-only
// accounts have no independently inspectable local record and are unsupported.
func localAccountHome() (string, error) {
	const path = "/etc/passwd"
	raw, err := readTrustedLaunchSource(path, true)
	if err != nil {
		return "", accountHomeError()
	}
	return accountHomeFromPasswd(raw, uint32(os.Geteuid()))
}

func accountHomeError() error {
	return fmt.Errorf("%w: %%h requires one valid effective-UID record in trusted root-owned local /etc/passwd; missing or NSS-only accounts are unsupported", ErrLaunchUnsupported)
}

// Scan the complete bounded source so duplicate effective-UID records cannot
// silently select an account identity.
func accountHomeFromPasswd(raw []byte, uid uint32) (string, error) {
	if len(raw) > 1<<20 {
		return "", accountHomeError()
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	home, found := "", false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if !validLocalAccountRecord(fields) {
			return "", accountHomeError()
		}
		id, _ := strconv.ParseUint(fields[2], 10, 32)
		if uint32(id) != uid {
			continue
		}
		if found {
			return "", accountHomeError()
		}
		home, found = fields[5], true
	}
	if scanner.Err() != nil || !found {
		return "", accountHomeError()
	}
	return home, nil
}

func validLocalAccountRecord(fields []string) bool {
	if len(fields) != 7 || fields[0] == "" {
		return false
	}
	_, uidErr := strconv.ParseUint(fields[2], 10, 32)
	_, gidErr := strconv.ParseUint(fields[3], 10, 32)
	return uidErr == nil && gidErr == nil
}

// A parse owns one lazy trusted snapshot, shared by all its contributing unit
// sources. Runtime requalification creates a fresh snapshot rather than caching
// account trust across inspections.
type accountHomeSnapshot struct {
	read   func() (string, error)
	loaded bool
	home   string
	err    error
}

func (s *accountHomeSnapshot) expand(value string) (string, error) {
	if !s.loaded {
		s.loaded = true
		s.home, s.err = s.read()
	}
	if s.err != nil {
		return "", s.err
	}
	return expandAccountHomePath(value, s.home)
}
