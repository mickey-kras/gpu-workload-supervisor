package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var workloadID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var unitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.@-]*\.service$`)

func ValidWorkloadID(id Workload) bool {
	return workloadID.MatchString(string(id)) && id != WorkloadIdle && id != WorkloadUnknown
}

type Profile struct {
	ID          Workload `json:"id"`
	Label       string   `json:"label"`
	Adapter     string   `json:"adapter"`
	Unit        string   `json:"unit"`
	Cgroup      string   `json:"cgroup"`
	HealthURL   string   `json:"healthURL"`
	ReleaseURL  string   `json:"releaseURL,omitempty"`
	RequiredMiB uint64   `json:"requiredMiB,omitempty"`
	BootPolicy  string   `json:"bootPolicy,omitempty"`
}
type Catalog struct {
	Version  int       `json:"version"`
	Profiles []Profile `json:"profiles"`
}
type CatalogSnapshot struct {
	Revision string  `json:"revision"`
	Catalog  Catalog `json:"catalog"`
}

func DecodeCatalog(r io.Reader) (Catalog, error) {
	var c Catalog
	raw, err := io.ReadAll(io.LimitReader(r, 65537))
	if err != nil {
		return c, err
	}
	if len(raw) > 65536 {
		return c, errors.New("catalog exceeds 64 KiB")
	}
	if err = uniqueKeys(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return c, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, errors.New("catalog must contain one JSON value")
	}
	return c, c.Validate()
}
func (c Catalog) Profile(id Workload) (Profile, bool) {
	for _, p := range c.Profiles {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}
func (c Catalog) Clone() Catalog { c.Profiles = append([]Profile(nil), c.Profiles...); return c }
func (c Catalog) Validate() error {
	if c.Version != 1 {
		return errors.New("unsupported catalog version")
	}
	if len(c.Profiles) < 1 || len(c.Profiles) > 32 {
		return errors.New("catalog requires 1 to 32 profiles")
	}
	for i, p := range c.Profiles {
		if !ValidWorkloadID(p.ID) {
			return fmt.Errorf("invalid workload ID %q", p.ID)
		}
		if !ValidWorkloadLabel(p.Label) {
			return errors.New("invalid workload label")
		}
		if p.Adapter != "systemd" && p.Adapter != "media-unload" {
			return errors.New("unsupported workload adapter")
		}
		if !unitName.MatchString(p.Unit) {
			return errors.New("invalid workload unit")
		}
		if p.Cgroup == "/" || !strings.HasPrefix(p.Cgroup, "/") || path.Clean(p.Cgroup) != p.Cgroup || strings.IndexFunc(p.Cgroup, unicode.IsControl) >= 0 {
			return errors.New("invalid workload cgroup")
		}
		if p.BootPolicy != "" && p.BootPolicy != "stop-to-idle" && p.BootPolicy != "retain" {
			return errors.New("invalid boot policy")
		}
		for _, e := range []string{p.HealthURL, p.ReleaseURL} {
			if e == "" && e == p.ReleaseURL {
				continue
			}
			u, err := url.Parse(e)
			if err != nil || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
				return errors.New("invalid endpoint")
			}
			ip := net.ParseIP(u.Hostname())
			if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
				return errors.New("endpoint must be loopback")
			}
		}
		if p.HealthURL == "" || p.Adapter == "media-unload" && p.ReleaseURL == "" {
			return errors.New("required endpoint missing")
		}
		for _, q := range c.Profiles[:i] {
			if p.ID == q.ID || p.Unit == q.Unit || p.Cgroup == q.Cgroup || strings.HasPrefix(p.Cgroup, q.Cgroup+"/") || strings.HasPrefix(q.Cgroup, p.Cgroup+"/") {
				return errors.New("duplicate or overlapping profiles")
			}
		}
	}
	return nil
}

func uniqueKeys(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim == '{' {
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate catalog key")
			}
			seen[name] = true
			if err = uniqueKeys(d); err != nil {
				return err
			}
		}
	} else if delim == '[' {
		for d.More() {
			if err = uniqueKeys(d); err != nil {
				return err
			}
		}
	} else {
		return errors.New("invalid catalog JSON")
	}
	_, err = d.Token()
	return err
}

// ValidWorkloadLabel is shared with local presentation protocols.
func ValidWorkloadLabel(label string) bool {
	return utf8.ValidString(label) && strings.TrimSpace(label) != "" && len(label) <= 128 && utf8.RuneCountInString(label) <= 80 && strings.IndexFunc(label, func(r rune) bool {
		return r == 0xfeff || unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
	}) < 0
}
