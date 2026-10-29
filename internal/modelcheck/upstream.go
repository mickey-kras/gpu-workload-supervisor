package modelcheck

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// DefaultHub is the model hub that publishes upstream git object IDs.
const DefaultHub = "https://huggingface.co"

// Upstream listing bounds.
const (
	maxListingPages     = 64
	maxListingPageBytes = 16 << 20
	maxUpstreamFiles    = 32768
	// maxGitBlobBytes bounds files that the identity check hashes as git
	// blobs. The hub stores larger files in LFS, which publishes SHA-256.
	maxGitBlobBytes = 64 << 20
)

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
)

// upstreamError keeps the reason of a failed listing.
type upstreamError struct{ reason string }

func (e upstreamError) Error() string { return "upstream listing failed: " + e.reason }

func failed(reason string) error { return upstreamError{reason: reason} }

// HubListing reads the recursive tree of a source from a hub API over
// HTTPS. It refuses redirects, follows only next-page links to the same tree
// URL and bounds every page. A redirected repository needs its current name
// in the source.
func HubListing(client *http.Client, hub string) Listing {
	strict := *client
	strict.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return func(ctx context.Context, source Source) (map[string]UpstreamFile, error) {
		if !source.valid() {
			return nil, failed(ReasonUpstreamInvalid)
		}
		first, err := url.Parse(hub + "/api/models/" + source.Repo + "/tree/" + source.Revision + "?recursive=true")
		if err != nil || first.Scheme != "https" {
			return nil, failed(ReasonUpstreamInvalid)
		}
		files := map[string]UpstreamFile{}
		next := first.String()
		for page := 0; next != ""; page++ {
			if page == maxListingPages {
				return nil, failed(ReasonUpstreamInvalid)
			}
			if next, err = readPage(ctx, &strict, first, next, files); err != nil {
				return nil, err
			}
		}
		return files, nil
	}
}

func readPage(ctx context.Context, client *http.Client, first *url.URL, page string, files map[string]UpstreamFile) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
	if err != nil {
		return "", failed(ReasonUpstreamInvalid)
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return "", failed(ReasonUpstreamUnreachable)
	}
	defer response.Body.Close()
	if reason := statusReason(response.StatusCode); reason != "" {
		return "", failed(reason)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxListingPageBytes+1))
	if err != nil {
		return "", failed(ReasonUpstreamUnreachable)
	}
	var entries []UpstreamFile
	if len(raw) > maxListingPageBytes || !utf8.Valid(raw) || json.Unmarshal(raw, &entries) != nil {
		return "", failed(ReasonUpstreamInvalid)
	}
	for _, entry := range entries {
		if err := addUpstream(files, entry); err != nil {
			return "", err
		}
	}
	return nextPage(first, response.Header.Get("Link"))
}

func statusReason(status int) string {
	switch {
	case status == http.StatusOK:
		return ""
	case status == http.StatusNotFound:
		return ReasonUpstreamNotFound
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ReasonUpstreamDenied
	case status >= 500:
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
		return failed(ReasonUpstreamInvalid)
	}
	if _, duplicate := files[entry.Path]; duplicate || len(files) >= maxUpstreamFiles {
		return failed(ReasonUpstreamInvalid)
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
		return "", failed(ReasonUpstreamInvalid)
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
