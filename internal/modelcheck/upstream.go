package modelcheck

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultHub is the model hub that publishes upstream git object IDs.
const DefaultHub = "https://huggingface.co"

// Upstream listing bounds.
const (
	maxListingPages     = 64
	maxListingPageBytes = 16 << 20
	maxUpstreamFiles    = 32768
)

// MaxGitBlobBytes bounds files that the identity check hashes as git blobs.
// The hub stores larger files in LFS, which publishes SHA-256.
const MaxGitBlobBytes = 64 << 20

// maxGitBlobBytes is the effective limit. Tests lower it.
var maxGitBlobBytes int64 = MaxGitBlobBytes

var (
	repoPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha1Pattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	nextLinkPattern = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)
)

// Source names the upstream repository and the pinned commit. A model from
// another source or revision is another model.
type Source struct {
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
}

func (s Source) valid() bool {
	return repoPattern.MatchString(s.Repo) && !strings.Contains(s.Repo, "..") && revisionPattern.MatchString(s.Revision)
}

// UpstreamFile is one file of the upstream tree at the pinned revision. OID
// is the git blob ID. LFS files also publish the SHA-256 of their content.
type UpstreamFile struct {
	Type string    `json:"type"`
	Path string    `json:"path"`
	OID  string    `json:"oid"`
	Size int64     `json:"size"`
	LFS  *LFSEntry `json:"lfs,omitempty"`
}

// LFSEntry is the LFS pointer of one upstream file.
type LFSEntry struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// Listing gives the upstream files of a source, by path.
type Listing func(ctx context.Context, source Source) (map[string]UpstreamFile, error)

// Reasons for a failed upstream listing.
const (
	ReasonUpstreamNotFound    = "upstream-not-found"
	ReasonUpstreamDenied      = "upstream-denied"
	ReasonUpstreamInvalid     = "upstream-invalid"
	ReasonUpstreamUnreachable = "upstream-unreachable"
	ReasonUpstreamMoved       = "upstream-moved"
	ReasonHistoryTooLong      = "upstream-history-too-long"
	// ReasonNotOnBranch is a finding: no branch or tag reaches the revision.
	ReasonNotOnBranch = "revision-not-on-branch"
)

// upstreamError keeps the reason of a failed listing.
type upstreamError struct{ reason string }

func (e upstreamError) Error() string { return "upstream listing failed: " + e.reason }

func listingError(reason string) error { return upstreamError{reason: reason} }

// listingTimeout bounds one listing when ctx has no deadline. Tests shorten it.
var listingTimeout = 10 * time.Minute

// maxHistoryPages bounds the commit history pages that the branch check reads.
const maxHistoryPages = 64

// errNotOnBranch marks a revision that no branch or tag of the repository
// reaches, for example a commit of a community pull request.
var errNotOnBranch = errors.New("revision is not on a branch or tag")

// HubListing reads the recursive tree of a source from a hub API over
// HTTPS. The revision must be on a branch or a tag of the repository. It
// refuses redirects, follows only next-page links to the same URL path and
// bounds every page. A redirected repository needs its current name in the
// source. It sends the token of an existing hub login only to the hub.
func HubListing(client *http.Client, hub string) Listing {
	strict := *client
	strict.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return func(ctx context.Context, source Source) (map[string]UpstreamFile, error) {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, listingTimeout)
			defer cancel()
		}
		base, err := url.Parse(hub + "/api/models/" + source.Repo)
		if !source.valid() || err != nil || base.Scheme != "https" {
			return nil, listingError(ReasonUpstreamInvalid)
		}
		h := hubClient{client: &strict, base: base, token: hubToken()}
		if err := h.onBranch(ctx, source.Revision); err != nil {
			return nil, err
		}
		return h.tree(ctx, source.Revision)
	}
}

// tree reads the recursive file tree at the revision.
func (h hubClient) tree(ctx context.Context, revision string) (map[string]UpstreamFile, error) {
	files := map[string]UpstreamFile{}
	_, _, err := h.pages(ctx, "/tree/"+revision+"?recursive=true", maxListingPages, func(raw []byte) (bool, error) {
		var entries []UpstreamFile
		if json.Unmarshal(raw, &entries) != nil {
			return false, listingError(ReasonUpstreamInvalid)
		}
		for _, entry := range entries {
			if err := addUpstream(files, entry); err != nil {
				return false, err
			}
		}
		return false, nil
	})
	if errors.Is(err, errPageLimit) {
		return nil, listingError(ReasonUpstreamInvalid)
	}
	if err != nil {
		return nil, err
	}
	return files, nil
}

// hubClient reads JSON pages below one repository URL.
type hubClient struct {
	client *http.Client
	base   *url.URL
	token  string
}

type hubRef struct {
	Name         string `json:"name"`
	TargetCommit string `json:"targetCommit"`
}

// onBranch needs the revision to be the head of a branch or a tag, or to be
// in the bounded history of a branch.
func (h hubClient) onBranch(ctx context.Context, revision string) error {
	var refs struct {
		Branches []hubRef `json:"branches"`
		Tags     []hubRef `json:"tags"`
	}
	if _, _, err := h.pages(ctx, "/refs", 1, func(raw []byte) (bool, error) {
		if json.Unmarshal(raw, &refs) != nil {
			return false, listingError(ReasonUpstreamInvalid)
		}
		return false, nil
	}); err != nil {
		if errors.Is(err, errPageLimit) {
			return listingError(ReasonUpstreamInvalid)
		}
		return err
	}
	head, err := headOf(refs.Branches, refs.Tags, revision)
	if err != nil || head {
		return err
	}
	budget := maxHistoryPages
	for _, branch := range refs.Branches {
		found, err := h.history(ctx, branch.Name, revision, &budget)
		if err != nil || found {
			return err
		}
	}
	return errNotOnBranch
}

// headOf needs at least one branch and a name and a commit for each ref. It
// reports whether a branch or a tag points at the revision.
func headOf(branches, tags []hubRef, revision string) (bool, error) {
	if len(branches) == 0 {
		return false, listingError(ReasonUpstreamInvalid)
	}
	head := false
	for _, ref := range append(append([]hubRef(nil), branches...), tags...) {
		if ref.Name == "" || !sha1Pattern.MatchString(ref.TargetCommit) {
			return false, listingError(ReasonUpstreamInvalid)
		}
		head = head || ref.TargetCommit == revision
	}
	return head, nil
}

// history looks for the revision in the commit history of one branch.
func (h hubClient) history(ctx context.Context, branch, revision string, budget *int) (bool, error) {
	if *budget <= 0 {
		return false, listingError(ReasonHistoryTooLong)
	}
	used, found, err := h.pages(ctx, "/commits/"+url.PathEscape(branch), *budget, func(raw []byte) (bool, error) {
		var commits []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &commits) != nil {
			return false, listingError(ReasonUpstreamInvalid)
		}
		for _, commit := range commits {
			if commit.ID == revision {
				return true, nil
			}
		}
		return false, nil
	})
	*budget -= used
	if errors.Is(err, errPageLimit) {
		return false, listingError(ReasonHistoryTooLong)
	}
	return found, err
}

// errPageLimit marks a paged list with more pages than the caller allows.
var errPageLimit = errors.New("too many pages")

// pages reads at most limit pages that start at the repository URL plus
// suffix. It follows next-page links only to the same scheme, host and path.
// visit returns true to stop. pages gives the number of pages read and
// whether visit stopped.
func (h hubClient) pages(ctx context.Context, suffix string, limit int, visit func([]byte) (bool, error)) (int, bool, error) {
	first, err := url.Parse(h.base.String() + suffix)
	if err != nil {
		return 0, false, listingError(ReasonUpstreamInvalid)
	}
	next := first.String()
	used := 0
	for next != "" {
		if used == limit {
			return used, false, errPageLimit
		}
		raw, link, err := h.get(ctx, next)
		used++
		if err != nil {
			return used, false, err
		}
		done, err := visit(raw)
		if err != nil || done {
			return used, done, err
		}
		if next, err = nextPage(first, link); err != nil {
			return used, false, err
		}
	}
	return used, false, nil
}

// get reads one bounded JSON page. The token goes only to the hub host.
func (h hubClient) get(ctx context.Context, page string) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
	if err != nil || request.URL.Host != h.base.Host || request.URL.Scheme != "https" {
		return nil, "", listingError(ReasonUpstreamInvalid)
	}
	request.Header.Set("Accept", "application/json")
	if h.token != "" {
		request.Header.Set("Authorization", "Bearer "+h.token)
	}
	response, err := h.client.Do(request)
	if err != nil {
		return nil, "", listingError(ReasonUpstreamUnreachable)
	}
	defer response.Body.Close()
	if reason := statusReason(response); reason != "" {
		return nil, "", listingError(reason)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxListingPageBytes+1))
	if err != nil {
		return nil, "", listingError(ReasonUpstreamUnreachable)
	}
	if len(raw) > maxListingPageBytes || !utf8.Valid(raw) {
		return nil, "", listingError(ReasonUpstreamInvalid)
	}
	return raw, response.Header.Get("Link"), nil
}

// statusReason maps an HTTP status to a reason. The hub can answer 401 or
// 403 with an error code for a repository or revision that does not exist.
func statusReason(response *http.Response) string {
	status := response.StatusCode
	switch code := response.Header.Get("X-Error-Code"); {
	case status == http.StatusOK:
		return ""
	case status == http.StatusNotFound, code == "RepoNotFound", code == "RevisionNotFound":
		return ReasonUpstreamNotFound
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ReasonUpstreamDenied
	case status >= 300 && status < 400:
		return ReasonUpstreamMoved
	case status >= 500 || status == http.StatusTooManyRequests || status == http.StatusRequestTimeout:
		return ReasonUpstreamUnreachable
	}
	return ReasonUpstreamInvalid
}

func addUpstream(files map[string]UpstreamFile, entry UpstreamFile) error {
	if entry.Type == "directory" {
		return nil
	}
	lfs := entry.LFS == nil || (sha256Pattern.MatchString(entry.LFS.OID) && entry.LFS.Size >= 0)
	if entry.Type != "file" || !filepath.IsLocal(entry.Path) || !sha1Pattern.MatchString(entry.OID) || entry.Size < 0 || !lfs {
		return listingError(ReasonUpstreamInvalid)
	}
	if _, duplicate := files[entry.Path]; duplicate || len(files) >= maxUpstreamFiles {
		return listingError(ReasonUpstreamInvalid)
	}
	files[entry.Path] = entry
	return nil
}

// nextPage accepts only a link to the same tree on the same host.
func nextPage(first *url.URL, header string) (string, error) {
	match := nextLinkPattern.FindStringSubmatch(header)
	if match == nil {
		return "", nil
	}
	next, err := url.Parse(match[1])
	if err != nil || next.Scheme != first.Scheme || next.Host != first.Host || next.Path != first.Path {
		return "", listingError(ReasonUpstreamInvalid)
	}
	return next.String(), nil
}

// Reasons for a failed identity check.
const (
	ReasonNotUpstream      = "file-not-upstream"
	ReasonIdentityMismatch = "upstream-identity-mismatch"
)

// checkIdentity needs each local file, in name order, to match the upstream
// git object at the pinned revision. An LFS file matches by its SHA-256,
// which is the inventory digest. Other files match by their git blob ID
// (SHA-1). gitBlobID hashes the same bytes after it confirms that they still
// match the inventory SHA-256.
func checkIdentity(root string, digests map[string]string, upstream map[string]UpstreamFile) (string, error) {
	for _, name := range sortedNames(digests) {
		digest := digests[name]
		entry, found := upstream[name]
		if !found {
			return ReasonNotUpstream, nil
		}
		if entry.LFS != nil {
			if entry.LFS.OID != digest {
				return ReasonIdentityMismatch, nil
			}
			continue
		}
		if entry.Size > maxGitBlobBytes {
			return ReasonIdentityMismatch, nil
		}
		blob, err := gitBlobID(filepath.Join(root, name), entry.Size, digest)
		if err != nil {
			return "", err
		}
		if blob == "" || blob != entry.OID {
			return ReasonIdentityMismatch, nil
		}
	}
	return "", nil
}

// gitBlobID hashes "blob <size>\x00" and the content. A local size other
// than the upstream size gives an empty ID, which is a mismatch, before any
// read. The content must still match the inventory.
func gitBlobID(file string, size int64, digest string) (string, error) {
	f, err := openRegular(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", errUnreadable
	}
	if info.Size() != size {
		return "", nil
	}
	content, err := io.ReadAll(io.LimitReader(f, size+1))
	if err != nil {
		return "", errUnreadable
	}
	if sum := sha256.Sum256(content); hex.EncodeToString(sum[:]) != digest {
		return "", errChanged
	}
	// Git blob IDs are SHA-1; the source publishes no other digest for small files.
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil)), nil
}
