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
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

const AdapterMediaUnload = "media-unload"

var ErrOwnedRequiresV2 = errors.New("owned launch profiles require catalog version 2")
var ErrOwnedCatalogManagedBySetup = errors.New("owned launch profiles are created and removed by gpu-setup apply")

// OwnedUnitName derives the supervisor-owned unit name; it is never user
// supplied. Ollama owned units are per instance so owned models may share
// them; other runtimes render one unit per workload.
func OwnedUnitName(runtimeName, instance string, id Workload) string {
	if runtimeName == "ollama" {
		return "gws-owned-ollama-" + instance + ".service"
	}
	return "gws-owned-" + string(id) + ".service"
}

var workloadID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var unitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.@-]*\.service$`)

func ValidWorkloadID(id Workload) bool {
	return workloadID.MatchString(string(id)) && id != WorkloadIdle && id != WorkloadUnknown
}

type LaunchBinding struct {
	Runtime      string `json:"runtime"`
	Endpoint     string `json:"endpoint"`
	LaunchFile   string `json:"launchFile"`
	LaunchSHA256 string `json:"launchSHA256"`
}

type WorkloadProfile struct {
	LaunchBinding  *LaunchBinding `json:"launchBinding,omitempty"`
	SystemdSlice   string         `json:"systemdSlice,omitempty"`
	SystemdVersion uint16         `json:"systemdVersion,omitempty"`
	NativeModel    *NativeModel   `json:"nativeModel,omitempty"`
	ID             Workload       `json:"id"`
	Label          string         `json:"label"`
	Adapter        string         `json:"adapter"`
	Unit           string         `json:"unit"`
	Cgroup         string         `json:"cgroup"`
	HealthURL      string         `json:"healthURL"`
	ReleaseURL     string         `json:"releaseURL,omitempty"`
	RequiredMiB    uint64         `json:"requiredMiB,omitempty"`
	BootPolicy     string         `json:"bootPolicy,omitempty"`
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
	return DecodeCatalogBytes(raw)
}

// DecodeCatalogBytes is the single strict decode path for catalog bytes from
// any source, including the database blob: duplicate-key check, unknown-field
// rejection, a single JSON value, and full validation.
func DecodeCatalogBytes(raw []byte) (Catalog, error) {
	var c Catalog
	if err := strictjson.Check(json.NewDecoder(bytes.NewReader(raw))); err != nil {
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
		if c.Profiles[i].LaunchBinding != nil {
			b := *c.Profiles[i].LaunchBinding
			c.Profiles[i].LaunchBinding = &b
		}
		if c.Profiles[i].NativeModel != nil {
			n := *c.Profiles[i].NativeModel
			if n.Owned != nil {
				o := *n.Owned
				n.Owned = &o
			}
			c.Profiles[i].NativeModel = &n
		}
	}
	return c
}
func (c Catalog) Validate() error {
	if c.Version != 1 && c.Version != 2 {
		return errors.New("unsupported catalog version")
	}
	if len(c.Profiles) < 1 || len(c.Profiles) > 32 {
		return errors.New("catalog requires 1 to 32 profiles")
	}
	for i, p := range c.Profiles {
		if c.Version == 1 && p.NativeModel != nil && p.NativeModel.Owned != nil {
			return ErrOwnedRequiresV2
		}
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
	if err := p.validateOwnedPlacement(); err != nil {
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
	if p.SystemdSlice != "" && (p.SystemdSlice != "app.slice" || (p.SystemdVersion != 252 && p.SystemdVersion != 255 && p.SystemdVersion != 259)) {
		return errors.New("unsupported automatic systemd slice")
	}
	if p.LaunchBinding != nil {
		b := p.LaunchBinding
		if p.NativeModel != nil || b.Runtime != "comfyui" || p.Adapter != "systemd" {
			return errors.New("unsupported application launch binding")
		}
		if err := validateLaunchEvidence(b.Endpoint, b.LaunchFile, b.LaunchSHA256); err != nil {
			return err
		}
		if p.HealthURL != b.Endpoint+"/system_stats" {
			return errors.New("ComfyUI health route must match launch endpoint")
		}
	}
	if p.NativeModel == nil {
		return nil
	}
	if p.Adapter != "systemd" {
		return errors.New("native models require stop-service bindings")
	}
	return p.NativeModel.validate()
}

// validateOwnedPlacement pins an owned profile to its derived unit name, the
// cgroup derived under the manager's app.slice, and a launch file inside the
// systemd user directory. Setup enforces the exact home/manager-root prefixes
// because only it knows the host values.
func (p WorkloadProfile) validateOwnedPlacement() error {
	if p.NativeModel == nil || p.NativeModel.Owned == nil {
		return nil
	}
	unit := OwnedUnitName(p.NativeModel.Runtime, p.NativeModel.Instance, p.ID)
	if p.Unit != unit {
		return errors.New("owned launch unit must be the derived supervisor-owned name")
	}
	if !strings.HasSuffix(p.Cgroup, "/app.slice/"+unit) {
		return errors.New("owned launch cgroup must derive from the manager app.slice")
	}
	if !strings.HasSuffix(p.NativeModel.LaunchFile, "/.config/systemd/user/"+unit) {
		return errors.New("owned launch file must live in the systemd user directory")
	}
	return nil
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
		if ownedEndpointCollision(p, q) {
			return errors.New("native endpoint belongs to another instance")
		}
		if sharedOllamaUnit(p, q) && (p.NativeModel.LaunchFile != q.NativeModel.LaunchFile || p.NativeModel.LaunchSHA256 != q.NativeModel.LaunchSHA256) {
			return errors.New("shared Ollama unit requires identical launch bindings")
		}
		if profilesOverlap(p, q) {
			return errors.New("duplicate or overlapping profiles")
		}
	}
	return nil
}

// ownedEndpointCollision extends the endpoint rule to owned profiles: two
// profiles may share an endpoint only when they share one Ollama instance per
// the shared-unit rule.
func ownedEndpointCollision(p, q WorkloadProfile) bool {
	if p.NativeModel == nil || q.NativeModel == nil || nativeEndpointKey(p.NativeModel.Endpoint) != nativeEndpointKey(q.NativeModel.Endpoint) {
		return false
	}
	if p.NativeModel.Owned == nil && q.NativeModel.Owned == nil {
		return false
	}
	return !sharedOllamaUnit(p, q)
}

// nativeEndpointKey normalizes an endpoint to its socket address for collision
// detection only; the stored endpoint string is never rewritten. Textually
// different URLs that name the same TCP socket (zero-padded ports, hostname
// casing, expanded IPv6 loopback forms) must collide.
func nativeEndpointKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	port := u.Port()
	if n, err := strconv.Atoi(port); err == nil {
		port = strconv.Itoa(n)
	}
	return host + ":" + port
}

func validateNativeOverlap(a, b *NativeModel) error {
	if a == nil || b == nil {
		return nil
	}
	if a.Instance == b.Instance && (a.Runtime != b.Runtime || nativeEndpointKey(a.Endpoint) != nativeEndpointKey(b.Endpoint)) {
		return errors.New("inconsistent native runtime instance")
	}
	if nativeEndpointKey(a.Endpoint) == nativeEndpointKey(b.Endpoint) && a.Instance != b.Instance {
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
