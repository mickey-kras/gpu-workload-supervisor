package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// ErrArtifactEscape rejects a model artifact link that is unresolved or
// resolves outside the approved artifact root. A bind over such a link
// would pin a path the runtime cannot be proven unable to rewrite.
var ErrArtifactEscape = errors.New("model artifact link is unresolved or escapes the approved artifact root")

// ErrArtifactAlias rejects a model artifact file that is reachable through
// another path: a hard link shares the inode, so a read-only bind of one
// path leaves the other path writable.
var ErrArtifactAlias = errors.New("model artifact file is reachable through another writable path")

// ErrArtifactRoot rejects an artifact root that cannot be pinned safely:
// the filesystem root, or a tree inside the world-writable temporary
// directory where a local rename can race the bind setup.
var ErrArtifactRoot = errors.New("model artifact root must not be the filesystem root or a temporary-directory tree")

// ErrArtifactBounds rejects a model artifact tree that exceeds the
// inspection bounds. A tree that cannot be fully inspected cannot be
// claimed protected, so the check fails closed.
var ErrArtifactBounds = errors.New("model artifact tree exceeds the supported inspection bounds")

// ErrArtifactUnchecked marks an artifact check that proves nothing because
// its subject is unknown to the runtime.
var ErrArtifactUnchecked = errors.New("artifact checks are unchecked")

// Inspection bounds of the model artifact tree. They bound the work of one
// admission check; a tree past them is refused, not partially inspected.
const (
	artifactWalkMaxDepth   = 8
	artifactWalkMaxEntries = 4096
)

// artifactBindValue renders the deterministic BindReadOnlyPaths value of a
// protected launch: the model directory, the pinned chat template and the
// private environment file, exact duplicates removed. The model operand is
// a parameter because adopted launches take it from the command line.
func artifactBindValue(modelPath string, n control.NativeModel) string {
	paths := make([]string, 0, 3)
	seen := map[string]bool{}
	for _, path := range []string{modelPath, n.ChatTemplate, n.EnvironmentFile} {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return strings.Join(paths, " ")
}

// samePathSet compares two BindReadOnlyPaths values as sets of
// whitespace-separated paths, independent of joining or order.
func samePathSet(a, b string) bool {
	af, bf := strings.Fields(a), strings.Fields(b)
	if len(af) != len(bf) {
		return false
	}
	seen := make(map[string]bool, len(af))
	for _, path := range af {
		seen[path] = true
	}
	for _, path := range bf {
		if !seen[path] {
			return false
		}
	}
	return true
}

// validateArtifactBindValue checks the source syntax of an adopted
// BindReadOnlyPaths line. Remapping forms ("from:to"), relative paths and
// expansions are unsupported: the binds must name the approved artifacts
// exactly.
func validateArtifactBindValue(value string) error {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return fmt.Errorf("%w: BindReadOnlyPaths requires at least one path", ErrLaunchUnsupported)
	}
	for _, path := range fields {
		if strings.Contains(path, ":") {
			return fmt.Errorf("%w: BindReadOnlyPaths remapping with : is unsupported", ErrLaunchUnsupported)
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, launchExpansionCharacters) {
			return fmt.Errorf("%w: BindReadOnlyPaths paths must be absolute, clean and literal", ErrLaunchUnsupported)
		}
	}
	return nil
}

// qualifyParsedArtifacts verifies the read-only artifact binds of a parsed
// launch. An owned spec with artifact protection is verified against its
// rendered line and its artifact paths are mapped on this host. An adopted
// unit that configures BindReadOnlyPaths must cover exactly the recorded
// model artifacts; an adopted unit without binds is admitted without a
// protection claim. ComfyUI units never reach this check: their
// qualification rejects the directive earlier.
func qualifyParsedArtifacts(unit parsedLaunchUnit, n control.NativeModel) error {
	if n.Owned != nil {
		if n.Owned.Artifacts.IsZero() {
			return nil
		}
		if unit.artifactBinds != artifactBindValue(n.Owned.ModelPath, n) {
			// parseOwnedLaunchUnit accepted only the exact rendered line.
			return fmt.Errorf("%w: artifact binds differ from the recorded owned launch", ErrLaunchUnsupported)
		}
		return verifyArtifactPaths(n.Owned.ModelPath, n.ChatTemplate, n.EnvironmentFile)
	}
	if unit.artifactBinds == "" {
		return nil
	}
	args := strings.Fields(unit.execStart)
	flags, err := parseServerCommand(args, n.Runtime)
	if err != nil {
		return err
	}
	want := artifactBindValue(flags.model, n)
	if !samePathSet(unit.artifactBinds, want) {
		return fmt.Errorf("%w: BindReadOnlyPaths must cover exactly the recorded model artifacts; run setup again for this model", ErrLaunchUnsupported)
	}
	return verifyArtifactPaths(flags.model, n.ChatTemplate, n.EnvironmentFile)
}

// verifyArtifactPaths maps the approved artifact paths before launch. The
// model tree must resolve to a directory tree whose links stay inside the
// resolved root and whose files have no hard-linked aliases; the chat
// template and environment file must resolve to regular files without
// hard-linked aliases. It changes nothing.
func verifyArtifactPaths(modelPath, chatTemplate, environmentFile string) error {
	if _, err := resolveArtifactRoot(modelPath); err != nil {
		return err
	}
	for _, file := range []string{chatTemplate, environmentFile} {
		if file == "" {
			continue
		}
		if err := verifyArtifactFile(file); err != nil {
			return err
		}
	}
	return nil
}

// resolveArtifactRoot resolves the model directory and inspects its tree.
// The root must not be the filesystem root or a temporary-directory tree.
func resolveArtifactRoot(modelPath string) (string, error) {
	resolved, err := filepath.EvalSymlinks(modelPath)
	if err != nil {
		return "", markBindingRead(fmt.Errorf("%w: model directory could not be resolved", ErrLaunchUnsupported), err)
	}
	resolved = filepath.Clean(resolved)
	if resolved == string(filepath.Separator) {
		return "", fmt.Errorf("%w: %s", ErrArtifactRoot, modelPath)
	}
	if tmp := filepath.Clean(os.TempDir()); resolved == tmp || strings.HasPrefix(resolved, tmp+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrArtifactRoot, modelPath)
	}
	if err := walkArtifactTree(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// verifyArtifactFile resolves one pinned artifact file: it must exist as a
// regular file without a hard-linked alias. The path may symlink anywhere
// the operator trusts; the read-only bind pins the resolved inode, and the
// alias check closes the write-through-second-name hole.
func verifyArtifactFile(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return markBindingRead(fmt.Errorf("%w: artifact file could not be resolved", ErrLaunchUnsupported), err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return markBindingRead(fmt.Errorf("%w: artifact file could not be inspected", ErrLaunchUnsupported), err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s has the wrong file type", ErrLaunchUnsupported, path)
	}
	if artifactNlink(info) > 1 {
		return fmt.Errorf("%w: %s", ErrArtifactAlias, path)
	}
	return nil
}

// artifactNlink reads the hard-link count of a regular file. Linux is the
// supported host, where FileInfo always carries a Stat_t.
func artifactNlink(info fs.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Nlink)
	}
	return 1
}

// walkArtifactTree inspects every entry of the resolved model tree. Links
// must resolve inside the root; files must have no hard-linked aliases.
// WalkDir does not descend into symlinked directories, so each link is
// checked once as a link, and its target inside the root is covered by its
// real path.
func walkArtifactTree(root string) error {
	entries := 0
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return markBindingRead(fmt.Errorf("%w: model artifact tree could not be inspected", ErrLaunchUnsupported), err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel != "." {
			if strings.Count(rel, string(filepath.Separator)) > artifactWalkMaxDepth {
				return fmt.Errorf("%w: %s is deeper than %d levels", ErrArtifactBounds, path, artifactWalkMaxDepth)
			}
			entries++
			if entries > artifactWalkMaxEntries {
				return fmt.Errorf("%w: more than %d entries", ErrArtifactBounds, artifactWalkMaxEntries)
			}
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("%w: %s is dangling", ErrArtifactEscape, path)
			}
			if err := pathWithinRoot(filepath.Clean(target), root); err != nil {
				return fmt.Errorf("%w: %s -> %s", ErrArtifactEscape, path, target)
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return markBindingRead(fmt.Errorf("%w: model artifact could not be inspected", ErrLaunchUnsupported), err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s has an unsupported non-regular file type", ErrLaunchUnsupported, path)
		}
		if artifactNlink(info) > 1 {
			return fmt.Errorf("%w: %s", ErrArtifactAlias, path)
		}
		return nil
	})
}

// pathWithinRoot requires target to stay at or below root after both are
// cleaned, so a ".." link target cannot escape the approved tree.
func pathWithinRoot(target, root string) error {
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s is outside %s", ErrArtifactEscape, target, root)
	}
	return nil
}

// VerifyArtifactProtection runs the artifact checks the supervisor needs
// before it retains a running protected unit or admits a protected switch:
// the binding check with its artifact mapping and loaded-bind comparison.
// A runtime that cannot run the checks fails them. It changes nothing.
func (m *SystemdManager) VerifyArtifactProtection(ctx context.Context, workload control.Workload) error {
	p, ok := m.config.Catalog.Profile(workload)
	if !ok {
		return fmt.Errorf("%w: %s is not in the runtime catalog", ErrArtifactUnchecked, workload)
	}
	if !p.HasArtifactProtection() {
		return nil
	}
	return m.verifyNativeBinding(ctx, p)
}

// verifyStartedArtifacts re-checks the loaded BindReadOnlyPaths property of
// a just-started protected unit. Owned launches derive the expectation from
// the recorded spec; adopted launches keep their protection evidence in the
// recurring binding check, which reads the unit sources.
func (m *SystemdManager) verifyStartedArtifacts(ctx context.Context, p control.WorkloadProfile) error {
	n := p.NativeModel
	if n == nil || n.Owned == nil || n.Owned.Artifacts.IsZero() {
		return nil
	}
	want := artifactBindValue(n.Owned.ModelPath, *n)
	out, err := m.runner.Run(ctx, m.config.SystemctlPath, "--user", "show", "--property=BindReadOnlyPaths", "--no-pager", "--", p.Unit)
	if err != nil {
		return SafeError("cannot inspect the artifact binds: command failed", ErrLaunchChanged, errBindingCommand, err)
	}
	values, err := ParseUnitProperties(out)
	if err != nil {
		return err
	}
	if !samePathSet(values["BindReadOnlyPaths"], want) {
		return fmt.Errorf("%w: BindReadOnlyPaths changed after start", ErrLaunchChanged)
	}
	return nil
}
