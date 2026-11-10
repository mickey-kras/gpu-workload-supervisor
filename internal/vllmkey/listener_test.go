package vllmkey

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const tcpHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

// procFixture gives a system that reads the tables tcp and tcp6; "-" leaves
// a table out.
func procFixture(t *testing.T, tcp, tcp6 string) system {
	t.Helper()
	dir := t.TempDir()
	if tcp != "-" {
		if err := os.WriteFile(filepath.Join(dir, "tcp"), []byte(tcpHeader+tcp), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if tcp6 != "-" {
		if err := os.WriteFile(filepath.Join(dir, "tcp6"), []byte(tcpHeader+tcp6), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := host()
	s.procNet = dir
	return s
}

func line(address, state string, uid int) string {
	return "   0: " + address + " 00000000:0000 " + state + " 00000000:00000000 00:00000000 00000000 " + strconv.Itoa(uid) + " 0 12345 1\n"
}

func TestCheckListenerRequiresOnlyTheDesktopUser(t *testing.T) {
	t.Parallel()
	me := os.Geteuid()
	other := me + 1
	const endpoint = "http://127.0.0.1:8000" // port 8000 is 1F40
	cases := []struct {
		name, tcp, tcp6 string
		ok              bool
	}{
		{"own loopback listener", line("0100007F:1F40", "0A", me), "", true},
		{"own listener without tcp6 table", line("0100007F:1F40", "0A", me), "-", true},
		{"other user on loopback", line("0100007F:1F40", "0A", other), "", false},
		{"other user on wildcard", line("0100007F:1F40", "0A", me) + line("00000000:1F40", "0A", other), "", false},
		{"other user on IPv6 wildcard beside own IPv4", line("0100007F:1F40", "0A", me), line(strings.Repeat("0", 32)+":1F40", "0A", other), true},
		{"other user on IPv6 wildcard alone", "", line(strings.Repeat("0", 32)+":1F40", "0A", other), false},
		{"other user on mapped loopback alone", "", line("0000000000000000FFFF00000100007F:1F40", "0A", other), false},
		{"own IPv6 wildcard alone", "", line(strings.Repeat("0", 32)+":1F40", "0A", me), true},
		{"nothing listens", line("0100007F:1F41", "0A", me) + line("0100007F:1F40", "01", other), "", false},
		{"other user on another address", line("0100007F:1F40", "0A", me) + line("0101A8C0:1F40", "0A", other), "", true},
		{"short line", "garbage\n" + line("0100007F:1F40", "0A", me), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := procFixture(t, tc.tcp, tc.tcp6).checkListener(endpoint)
			if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrForeignListener)) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCheckListenerRejectsBadInput(t *testing.T) {
	t.Parallel()
	empty := procFixture(t, "-", "-")
	if err := empty.checkListener("http://127.0.0.1:8000"); !errors.Is(err, ErrForeignListener) {
		t.Fatalf("missing tcp table accepted: %v", err)
	}
	for _, endpoint := range []string{"http://localhost:8000", "http://127.0.0.1", "http://127.0.0.1:0", "://bad"} {
		if err := empty.checkListener(endpoint); !errors.Is(err, ErrForeignListener) {
			t.Fatalf("%s accepted: %v", endpoint, err)
		}
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "tcp"), 0o700); err != nil {
		t.Fatal(err)
	}
	unreadable := host()
	unreadable.procNet = dir
	if err := unreadable.checkListener("http://127.0.0.1:8000"); !errors.Is(err, ErrForeignListener) {
		t.Fatalf("unreadable table accepted: %v", err)
	}
}

func TestCheckListenerReadsTheRealTable(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/proc/net/tcp"); err != nil {
		t.Skip("no /proc/net/tcp")
	}
	if err := CheckListener("http://127.0.0.1:1"); !errors.Is(err, ErrForeignListener) {
		t.Fatalf("closed port accepted: %v", err)
	}
}
