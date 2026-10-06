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

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

const AdapterMediaUnload = "media-unload"

var workloadID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var unitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.@-]*\.service$`)

func ValidWorkloadID(id Workload) bool {
	return workloadID.MatchString(string(id)) && id != WorkloadIdle && id != WorkloadUnknown
}

type WorkloadProfile struct {
	NativeModel *NativeModel `json:"nativeModel,omitempty"`
	ID          Workload     `json:"id"`
	Label       string       `json:"label"`
	Adapter     string       `json:"adapter"`
	Unit        string       `json:"unit"`
	Cgroup      string       `json:"cgroup"`
	HealthURL   string       `json:"healthURL"`
	ReleaseURL  string       `json:"releaseURL,omitempty"`
	RequiredMiB uint64       `json:"requiredMiB,omitempty"`
	BootPolicy  string       `json:"bootPolicy,omitempty"`
}
type Catalog struct {
	Version  int               `json:"version"`
	Profiles []WorkloadProfile `json:"profiles"`
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
	if err = strictjson.Check(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		if errors.Is(err, strictjson.ErrDuplicateKey) {
			return c, errors.New("duplicate catalog key")
		}
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
func (c Catalog) Profile(id Workload) (WorkloadProfile, bool) {
	for _, p := range c.Profiles {
		if p.ID == id {
			return p, true
		}
	}
	return WorkloadProfile{}, false
}
func (c Catalog) Clone() Catalog {
	c.Profiles = append([]WorkloadProfile(nil), c.Profiles...)
	for i := range c.Profiles {
		if c.Profiles[i].NativeModel != nil {
			n := *c.Profiles[i].NativeModel
			c.Profiles[i].NativeModel = &n
		}
	}
	return c
}
func (c Catalog) Validate() error {
	if c.Version != 1 {
		return errors.New("unsupported catalog version")
	}
	if len(c.Profiles) < 1 || len(c.Profiles) > 32 {
		return errors.New("catalog requires 1 to 32 profiles")
	}
	for i, p := range c.Profiles {
		if err := p.validate(); err != nil {
			return err
		}
		if err := validateProfileOverlap(p, c.Profiles[:i]); err != nil {
			return err
		}
	}
	return nil
}

// ValidWorkloadLabel is shared with local presentation protocols.
func ValidWorkloadLabel(label string) bool {
	return utf8.ValidString(label) && strings.TrimSpace(label) != "" && len(label) <= 128 && utf8.RuneCountInString(label) <= 80 && strings.IndexFunc(label, func(r rune) bool {
		return r == 0xfeff || unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
	}) < 0
}

func (p WorkloadProfile) validate() error {
	if err := p.validateNativeBinding(); err != nil {
		return err
	}
	if err := p.validateNaming(); err != nil {
		return err
	}
	if err := p.validatePlacement(); err != nil {
		return err
	}
	if err := p.validateEndpoints(); err != nil {
		return err
	}
	if p.HealthURL == "" || p.Adapter == AdapterMediaUnload && p.ReleaseURL == "" {
		return errors.New("required endpoint missing")
	}
	return nil
}

func (p WorkloadProfile) validateNativeBinding() error {
	if p.NativeModel == nil {
		return nil
	}
	if p.Adapter != "systemd" {
		return errors.New("native models require stop-service bindings")
	}
	return p.NativeModel.validate()
}

func (p WorkloadProfile) validateNaming() error {
	if !ValidWorkloadID(p.ID) {
		return fmt.Errorf("invalid workload ID %q", p.ID)
	}
	if !ValidWorkloadLabel(p.Label) {
		return errors.New("invalid workload label")
	}
	if p.Adapter != "systemd" && p.Adapter != AdapterMediaUnload {
		return errors.New("unsupported workload adapter")
	}
	if !unitName.MatchString(p.Unit) {
		return errors.New("invalid workload unit")
	}
	return nil
}

func (p WorkloadProfile) validatePlacement() error {
	if p.Cgroup == "/" || !strings.HasPrefix(p.Cgroup, "/") || path.Clean(p.Cgroup) != p.Cgroup || strings.IndexFunc(p.Cgroup, unicode.IsControl) >= 0 {
		return errors.New("invalid workload cgroup")
	}
	if p.BootPolicy != "" && p.BootPolicy != "stop-to-idle" && p.BootPolicy != "retain" {
		return errors.New("invalid boot policy")
	}
	return nil
}

func (p WorkloadProfile) validateEndpoints() error {
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
	return nil
}

func validateProfileOverlap(p WorkloadProfile, previous []WorkloadProfile) error {
	for _, q := range previous {
		if err := validateNativeOverlap(p.NativeModel, q.NativeModel); err != nil {
			return err
		}
		if profilesOverlap(p, q) {
			return errors.New("duplicate or overlapping profiles")
		}
	}
	return nil
}

func validateNativeOverlap(a, b *NativeModel) error {
	if a == nil || b == nil {
		return nil
	}
	if a.Instance == b.Instance && (a.Runtime != b.Runtime || a.Endpoint != b.Endpoint) {
		return errors.New("inconsistent native runtime instance")
	}
	if a.Endpoint == b.Endpoint && a.Instance != b.Instance {
		return errors.New("native endpoint belongs to another instance")
	}
	if a.Instance == b.Instance && a.ComparisonModel() == b.ComparisonModel() {
		return errors.New("ambiguous native model binding")
	}
	return nil
}

func profilesOverlap(p, q WorkloadProfile) bool {
	if p.ID == q.ID {
		return true
	}
	if sharedOllamaUnit(p, q) {
		return false
	}
	return p.Unit == q.Unit || p.Cgroup == q.Cgroup || strings.HasPrefix(p.Cgroup, q.Cgroup+"/") || strings.HasPrefix(q.Cgroup, p.Cgroup+"/")
}

// SharedOllamaUnit reports whether two profiles bind different models to one
// Ollama unit: the only case where a unit and cgroup may repeat. Switching
// between them replaces the loaded model through the Ollama API; the unit
// keeps running.
func SharedOllamaUnit(p, q WorkloadProfile) bool {
	return sharedOllamaUnit(p, q)
}

func sharedOllamaUnit(p, q WorkloadProfile) bool {
	if p.NativeModel == nil || q.NativeModel == nil {
		return false
	}
	a, b := p.NativeModel, q.NativeModel
	return a.Runtime == "ollama" && b.Runtime == "ollama" &&
		a.Instance == b.Instance && a.Endpoint == b.Endpoint && a.ComparisonModel() != b.ComparisonModel() &&
		p.Unit == q.Unit && p.Cgroup == q.Cgroup
}
