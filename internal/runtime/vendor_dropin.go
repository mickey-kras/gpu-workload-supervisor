package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// The results of the package manager check of a vendor drop-in.
const (
	// VendorDropInVerified: an installed package owns the file, records its
	// checksum, and the file matches it.
	VendorDropInVerified = "verified"
	// VendorDropInModified: the file differs from its package, or the
	// package manager reports it missing.
	VendorDropInModified = "modified"
	// VendorDropInUnowned: no installed package owns the file.
	VendorDropInUnowned = "unowned"
	// VendorDropInCannotCheck: no supported package manager is present, or
	// the package manager cannot give a result. The check does not pass.
	VendorDropInCannotCheck = "cannotCheck"
)

// VendorDropIn is the package manager check of one vendor drop-in of the
// slice or of the user manager. Path is the name that systemd lists.
// Manager is "dpkg" or "rpm" when a package manager gave the result.
type VendorDropIn struct {
	Unit     string   `json:"unit"`
	Path     string   `json:"path"`
	Manager  string   `json:"manager,omitempty"`
	Packages []string `json:"packages,omitempty"`
	Result   string   `json:"result"`
	Detail   string   `json:"detail,omitempty"`
}

// vendorCommandTimeout bounds each package manager command. dpkg-query -S
// reads the file lists of all packages, and a verify reads each file of a
// package, so the limit is generous. A caller deadline that comes first
// still applies. Tests shorten it.
var vendorCommandTimeout = 20 * time.Second

// packageTool is a supported package manager. query looks up the owner of a
// file and verify compares the installed files of a package with its
// database. For rpm both are the rpm executable.
type packageTool struct {
	name, query, verify string
}

// packageToolCandidates are the supported package managers in the order of
// the lookup. The first one that owns a file decides the result.
var packageToolCandidates = []packageTool{
	{name: "dpkg", query: "/usr/bin/dpkg-query", verify: "/usr/bin/dpkg"},
	{name: "rpm", query: "/usr/bin/rpm", verify: "/usr/bin/rpm"},
}

// findPackageTools returns the supported package managers of the host. A
// package manager without its executables is absent. An executable that is
// present but fails the executable trust check is an error: the check
// cannot use it, and it must not count as absent.
func findPackageTools() ([]packageTool, error) {
	var tools []packageTool
	for _, candidate := range packageToolCandidates {
		tool, present, err := resolvePackageTool(candidate)
		if err != nil {
			return nil, err
		}
		if present {
			tools = append(tools, tool)
		}
	}
	return tools, nil
}

// resolvePackageTool resolves the trusted executables of a package manager.
// present is false when one of them does not exist.
func resolvePackageTool(tool packageTool) (packageTool, bool, error) {
	resolved := tool
	for _, target := range []*string{&resolved.query, &resolved.verify} {
		if _, err := os.Lstat(*target); errors.Is(err, os.ErrNotExist) {
			return tool, false, nil
		}
		executable, err := validateExecutable(*target)
		if err != nil {
			return tool, false, fmt.Errorf("%s cannot be used: %v", *target, err)
		}
		*target = executable
	}
	return resolved, true, nil
}

// vendorCache keeps the verified vendor drop-ins. The health checks repeat
// the host cap checks, and a package manager lookup reads the database of
// all packages, so a verified result stays while the file keeps its device,
// inode, size, mtime and ctime. A package upgrade or a change to the file
// changes them. Only verified results are kept.
type vendorCache struct {
	mu       sync.Mutex
	verified map[string]vendorCacheEntry
}

type vendorCacheEntry struct {
	key  vendorFileKey
	drop VendorDropIn
}

// vendorFileKey identifies the content of an open file.
type vendorFileKey struct {
	dev, ino     uint64
	size         int64
	mtime, ctime unix.Timespec
}

func fileKey(stat unix.Stat_t) vendorFileKey {
	return vendorFileKey{dev: stat.Dev, ino: stat.Ino, size: stat.Size, mtime: stat.Mtim, ctime: stat.Ctim}
}

func (c *vendorCache) get(file string, stat unix.Stat_t) (VendorDropIn, bool) {
	if c == nil {
		return VendorDropIn{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.verified[file]
	if !ok || entry.key != fileKey(stat) {
		return VendorDropIn{}, false
	}
	return entry.drop, true
}

// put keeps a verified result and removes any other result of file.
func (c *vendorCache) put(file string, stat unix.Stat_t, drop VendorDropIn) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if drop.Result != VendorDropInVerified {
		delete(c.verified, file)
		return
	}
	if c.verified == nil {
		c.verified = map[string]vendorCacheEntry{}
	}
	c.verified[file] = vendorCacheEntry{key: fileKey(stat), drop: drop}
}

// vendorCheck returns the package manager check for the vendor drop-ins of
// unit. own is the host cap drop-in that the fix of an unowned file names.
func (m *SystemdManager) vendorCheck(ctx context.Context, unit, own string) vendorCheck {
	return func(listed, file string, stat unix.Stat_t) error {
		return vendorError(m.checkVendorDropIn(ctx, unit, listed, file, stat), own)
	}
}

// vendorError classifies a vendor drop-in result. A modified or unowned file
// is ErrHostCap. A result that the package manager cannot give proves no
// wrong cap, so it is ErrLimitReadback. It never passes.
func vendorError(drop VendorDropIn, own string) error {
	switch drop.Result {
	case VendorDropInVerified:
		return nil
	case VendorDropInModified:
		return fmt.Errorf("%w: vendor drop-in %s differs from package %s: %s; reinstall that package, then run `sudo systemctl daemon-reload`",
			ErrHostCap, drop.Path, strings.Join(drop.Packages, ", "), drop.Detail)
	case VendorDropInUnowned:
		return fmt.Errorf("%w: no installed package owns vendor drop-in %s; remove it, or move its settings into %s, then run `sudo systemctl daemon-reload`",
			ErrHostCap, drop.Path, own)
	default:
		return fmt.Errorf("%w: the package manager cannot check vendor drop-in %s: %s", ErrLimitReadback, drop.Path, drop.Detail)
	}
}

// checkVendorDropIn checks a vendor drop-in with the package manager. file
// is its name under /usr/lib, and stat is the status of the open file.
func (m *SystemdManager) checkVendorDropIn(ctx context.Context, unit, listed, file string, stat unix.Stat_t) VendorDropIn {
	if cached, ok := m.vendorCache.get(file, stat); ok {
		cached.Unit, cached.Path = unit, listed
		return cached
	}
	verdict := m.packageVerdict(ctx, file)
	verdict.Unit, verdict.Path = unit, listed
	m.vendorCache.put(file, stat, verdict)
	return verdict
}

// packageVerdict asks each supported package manager in turn. The first one
// that owns the file decides the result, and a package manager that cannot
// answer ends the lookup.
func (m *SystemdManager) packageVerdict(ctx context.Context, file string) VendorDropIn {
	if m.host.packageTools == nil {
		return cannotCheck("", "the host has no package manager lookup")
	}
	tools, err := m.host.packageTools()
	if err != nil {
		return cannotCheck("", err.Error())
	}
	if len(tools) == 0 {
		return cannotCheck("", "no supported package manager (dpkg or rpm) is installed")
	}
	if strings.ContainsAny(file, "*?[]\\") {
		// dpkg-query -S takes a pattern, not a file name.
		return cannotCheck("", "the name holds a pattern character")
	}
	names := m.host.packageNames(file)
	verdict := VendorDropIn{Result: VendorDropInUnowned}
	for _, tool := range tools {
		verdict = m.toolVerdict(ctx, tool, names)
		if verdict.Result != VendorDropInUnowned {
			return verdict
		}
	}
	return verdict
}

// packageNames returns the names to look up for a vendor file under
// /usr/lib: that name, and the /lib name when /lib is a trusted link to
// usr/lib. A Debian package from before the merged /usr move records its
// files under /lib.
func (h limitHost) packageNames(file string) []string {
	names := []string{file}
	if rest, ok := strings.CutPrefix(file, "/usr/lib/"); ok && h.checkLibLink() == nil {
		names = append(names, "/lib/"+rest)
	}
	return names
}

func cannotCheck(manager, detail string) VendorDropIn {
	return VendorDropIn{Manager: manager, Result: VendorDropInCannotCheck, Detail: detail}
}

// toolVerdict checks a file with one package manager. It looks up each
// name, and the first owned name determines the packages to verify.
func (m *SystemdManager) toolVerdict(ctx context.Context, tool packageTool, names []string) VendorDropIn {
	for _, name := range names {
		packages, recorded, err := m.packageOwners(ctx, tool, name)
		if err != nil {
			return cannotCheck(tool.name, err.Error())
		}
		if len(packages) != 0 {
			verdict := m.verifyPackages(ctx, tool, packages, recorded)
			verdict.Manager, verdict.Packages = tool.name, packages
			return verdict
		}
	}
	return VendorDropIn{Manager: tool.name, Result: VendorDropInUnowned, Detail: "no installed package owns it"}
}

// packageOwners returns the packages that own name and the name the package
// database records. No packages and no error mean no package owns it.
func (m *SystemdManager) packageOwners(ctx context.Context, tool packageTool, name string) ([]string, string, error) {
	if tool.name == "dpkg" {
		return m.dpkgOwners(ctx, tool, name)
	}
	return m.rpmOwners(ctx, tool, name)
}

// verifyPackages verifies the file in each package that owns it. Each one
// must pass.
func (m *SystemdManager) verifyPackages(ctx context.Context, tool packageTool, packages []string, name string) VendorDropIn {
	for _, pkg := range packages {
		var verdict VendorDropIn
		if tool.name == "dpkg" {
			verdict = m.dpkgVerify(ctx, tool, pkg, name)
		} else {
			verdict = m.rpmVerify(ctx, tool, pkg, name)
		}
		if verdict.Result != VendorDropInVerified {
			return verdict
		}
	}
	return VendorDropIn{Result: VendorDropInVerified}
}

// dpkgPackage matches a dpkg package name with an optional architecture
// qualifier, as dpkg-query -S prints it for a Multi-Arch package.
var dpkgPackage = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+(:[a-z0-9-]+)?$`)

// rpmPackage matches the name-version-release.arch that rpm -qf prints.
var rpmPackage = regexp.MustCompile(`^[A-Za-z0-9_+][A-Za-z0-9._+~^:-]*$`)

// dpkgOwners runs dpkg-query -S for one exact name. A found name prints
// "pkg[, pkg...]: name" and exits 0. No owner exits 1. A diversion of the
// name returns an error, because the file can then come from another package.
func (m *SystemdManager) dpkgOwners(ctx context.Context, tool packageTool, name string) ([]string, string, error) {
	output, code, err := m.runPackageCommand(ctx, tool.query, "-S", name)
	if err != nil {
		return nil, "", err
	}
	lines := outputLines(output)
	if code == 1 && !slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, ": /") }) {
		return nil, "", nil
	}
	if code != 0 {
		return nil, "", commandStatus("dpkg-query -S", code, lines)
	}
	var packages []string
	for _, line := range lines {
		owners, err := dpkgOwnerLine(line, name)
		if err != nil {
			return nil, "", err
		}
		packages = append(packages, owners...)
	}
	if len(packages) == 0 {
		return nil, "", errors.New("dpkg-query -S printed no owner")
	}
	return packages, name, nil
}

// dpkgOwnerLine reads one dpkg-query -S line of name.
func dpkgOwnerLine(line, name string) ([]string, error) {
	if strings.HasPrefix(line, "diversion by ") {
		return nil, fmt.Errorf("dpkg diverts it: %q", firstLine(line))
	}
	list, rest, ok := strings.Cut(line, ": /")
	if !ok || "/"+rest != name {
		return nil, fmt.Errorf("unexpected dpkg-query -S output %q", firstLine(line))
	}
	packages := strings.Split(list, ", ")
	for _, pkg := range packages {
		if !dpkgPackage.MatchString(pkg) {
			return nil, fmt.Errorf("unexpected dpkg package name %q", pkg)
		}
	}
	return packages, nil
}

// dpkgVerify checks name in one package. dpkg --verify compares only the
// MD5 checksums from the md5sums control file, and it prints nothing for a
// file without a recorded checksum. So the md5sums file must list name
// first. Then a passing file has no line in the verify output.
func (m *SystemdManager) dpkgVerify(ctx context.Context, tool packageTool, pkg, name string) VendorDropIn {
	output, code, err := m.runPackageCommand(ctx, tool.query, "--control-show", pkg, "md5sums")
	if err == nil && code != 0 {
		err = commandStatus("dpkg-query --control-show", code, outputLines(output))
	}
	if err != nil {
		return cannotCheck(tool.name, fmt.Sprintf("the checksums of package %s cannot be read: %v", pkg, err))
	}
	if !md5sumsLists(output, name) {
		return cannotCheck(tool.name, "package "+pkg+" records no checksum of "+name)
	}
	output, code, err = m.runPackageCommand(ctx, tool.verify, "--verify-format=rpm", "--verify", pkg)
	return verifyVerdict(tool.name, "dpkg --verify", packageRun{output, code, err}, name, false)
}

// md5sumsLists reports whether an md5sums control file has a line for
// name. Its lines are "<md5>  <name without the leading slash>".
func md5sumsLists(output []byte, name string) bool {
	for _, line := range outputLines(output) {
		sum, file, ok := strings.Cut(line, "  ")
		if ok && len(sum) == 32 && "/"+file == name {
			return true
		}
	}
	return false
}

// rpmOwners runs rpm -qf for one name. It prints one package per line and
// exits 0. A file that no package owns prints "file ... is not owned by any
// package" and exits 1.
func (m *SystemdManager) rpmOwners(ctx context.Context, tool packageTool, name string) ([]string, string, error) {
	output, code, err := m.runPackageCommand(ctx, tool.query, "-qf", name)
	if err != nil {
		return nil, "", err
	}
	lines := outputLines(output)
	if code == 1 && len(lines) == 1 && strings.HasSuffix(lines[0], " is not owned by any package") {
		return nil, "", nil
	}
	if code != 0 {
		return nil, "", commandStatus("rpm -qf", code, lines)
	}
	for _, line := range lines {
		if !rpmPackage.MatchString(line) {
			return nil, "", fmt.Errorf("unexpected rpm -qf output %q", firstLine(line))
		}
	}
	if len(lines) == 0 {
		return nil, "", errors.New("rpm -qf printed no owner")
	}
	return lines, name, nil
}

// rpmVerify checks name in one package. --verbose lists each file, so a
// passing file has a line too. --nodeps and --noscripts leave out the
// dependency check and the %verifyscript of the package.
func (m *SystemdManager) rpmVerify(ctx context.Context, tool packageTool, pkg, name string) VendorDropIn {
	output, code, err := m.runPackageCommand(ctx, tool.verify, "-V", "--verbose", "--nodeps", "--noscripts", pkg)
	return verifyVerdict(tool.name, "rpm -V", packageRun{output, code, err}, name, true)
}

// packageRun is the result of one package manager command.
type packageRun struct {
	output []byte
	code   int
	err    error
}

// verifyVerdict reads the verify output of a package for name. Both
// package managers print the rpm format and exit 1 when a file of the
// package fails. Each line must be a file line. listed means a passing
// file has a line too.
func verifyVerdict(manager, command string, run packageRun, name string, listed bool) VendorDropIn {
	err := run.err
	if err == nil && run.code > 1 {
		err = commandStatus(command, run.code, outputLines(run.output))
	}
	if err != nil {
		return cannotCheck(manager, err.Error())
	}
	verdict := VendorDropIn{Result: VendorDropInVerified}
	found := false
	for _, line := range outputLines(run.output) {
		status, attributes, file, ok := parseVerifyLine(line)
		if !ok {
			return cannotCheck(manager, fmt.Sprintf("unexpected %s output %q", command, firstLine(line)))
		}
		if file != name {
			continue
		}
		found = true
		verdict = worseVerdict(verdict, lineVerdict(status, attributes))
	}
	if listed && !found {
		return cannotCheck(manager, command+" does not list "+name)
	}
	return verdict
}

// parseVerifyLine splits a verify line of the rpm format: a status of 9
// result characters or "missing", optional attribute letters, then the
// file name.
func parseVerifyLine(line string) (status, attributes, file string, ok bool) {
	index := strings.Index(line, " /")
	if index < 0 {
		return "", "", "", false
	}
	fields := strings.Fields(line[:index])
	file = line[index+1:]
	switch {
	case len(fields) == 0 || len(fields) > 2:
		return "", "", "", false
	case len(fields) == 2:
		attributes = fields[1]
		if strings.Trim(attributes, "abcdghlmnrs") != "" {
			return "", "", "", false
		}
	}
	status = fields[0]
	if status != "missing" && (len(status) != 9 || strings.Trim(status, ".?SM5DLUGTP") != "") {
		return "", "", "", false
	}
	return status, attributes, file, true
}

// lineVerdict classifies one verify line of the file. The size, digest and
// link target results show the content. The owner and mode results do not:
// trustedFile already checks them.
func lineVerdict(status, attributes string) VendorDropIn {
	switch {
	case status == "missing":
		return VendorDropIn{Result: VendorDropInModified, Detail: "the package manager reports it missing"}
	case strings.Contains(attributes, "g"):
		return VendorDropIn{Result: VendorDropInCannotCheck, Detail: "the package marks it as a ghost file without a checksum"}
	}
	for _, index := range []int{0, 2, 4} {
		if status[index] != '.' && status[index] != '?' {
			return VendorDropIn{Result: VendorDropInModified, Detail: "verify result " + status}
		}
	}
	if status[2] == '?' {
		return VendorDropIn{Result: VendorDropInCannotCheck, Detail: "the checksum cannot be compared: verify result " + status}
	}
	return VendorDropIn{Result: VendorDropInVerified}
}

// worseVerdict keeps the worse of two results: modified, then cannot check,
// then verified.
func worseVerdict(a, b VendorDropIn) VendorDropIn {
	rank := map[string]int{VendorDropInVerified: 0, VendorDropInCannotCheck: 1, VendorDropInModified: 2}
	if rank[b.Result] > rank[a.Result] {
		return b
	}
	return a
}

// exitCoder is an error with the exit status of a command, such as
// *exec.ExitError.
type exitCoder interface {
	error
	ExitCode() int
}

// runPackageCommand runs one package manager command with
// vendorCommandTimeout. It returns the output and the exit status. An error
// means that the command did not end with an exit status, such as a timeout
// or an executable that cannot start. The error keeps the cause as text
// only, so a missing executable does not look like a missing drop-in.
func (m *SystemdManager) runPackageCommand(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, vendorCommandTimeout)
	defer cancel()
	output, err := m.runner.Run(ctx, name, args...)
	if err == nil {
		return output, 0, nil
	}
	if ctx.Err() != nil {
		return nil, 0, fmt.Errorf("%s did not finish: %v", path.Base(name), ctx.Err())
	}
	var exit exitCoder
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		return output, exit.ExitCode(), nil
	}
	return nil, 0, fmt.Errorf("%s failed: %v", path.Base(name), err)
}

// commandStatus describes an unexpected exit status with the first output
// line.
func commandStatus(command string, code int, lines []string) error {
	if len(lines) == 0 {
		return fmt.Errorf("%s exited with status %d", command, code)
	}
	return fmt.Errorf("%s exited with status %d: %q", command, code, firstLine(lines[0]))
}

// outputLines returns the non-empty lines of command output.
func outputLines(output []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(output), "\n") {
		if line = strings.TrimRight(line, "\r"); strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
