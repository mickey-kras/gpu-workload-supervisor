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

// HubListing reads the recursive tree of a source from a hub API. It follows
// only next-page links to the same tree URL and bounds every page.
func HubListing(client *http.Client, hub string) Listing {
	return func(ctx context.Context, source Source) (map[string]UpstreamFile, error) {
		if !source.valid() {
			return nil, errors.New("model source needs a repository and a 40-hex commit")
		}
		first, err := url.Parse(hub + "/api/models/" + source.Repo + "/tree/" + source.Revision + "?recursive=true")
		if err != nil || first.Scheme != "https" {
			return nil, errors.New("model hub URL is invalid")
		}
		files := map[string]UpstreamFile{}
		next := first.String()
		for page := 0; next != ""; page++ {
			if page == maxListingPages {
				return nil, errors.New("upstream listing has too many pages")
			}
			if next, err = readPage(ctx, client, first, next, files); err != nil {
				return nil, err
			}
		}
		return files, nil
	}
}

func readPage(ctx context.Context, client *http.Client, first *url.URL, page string, files map[string]UpstreamFile) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
	if err != nil {
		return "", errors.New("upstream listing request is invalid")
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return "", errors.New("upstream listing is unreachable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upstream listing failed with HTTP status %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxListingPageBytes+1))
	if err != nil || len(raw) > maxListingPageBytes || !utf8.Valid(raw) {
		return "", errors.New("upstream listing is unreadable or too large")
	}
	var entries []UpstreamFile
	if err := json.Unmarshal(raw, &entries); err != nil {
		return "", errors.New("upstream listing is not valid JSON")
	}
	for _, entry := range entries {
		if err := addUpstream(files, entry); err != nil {
			return "", err
		}
	}
	return nextPage(first, response.Header.Get("Link"))
}

func addUpstream(files map[string]UpstreamFile, entry UpstreamFile) error {
	if entry.Type == "directory" {
		return nil
	}
	lfs := entry.LFS == nil || (sha256Pattern.MatchString(entry.LFS.OID) && entry.LFS.Size >= 0)
	if entry.Type != "file" || !filepath.IsLocal(entry.Path) || !sha1Pattern.MatchString(entry.OID) || entry.Size < 0 || !lfs {
		return errors.New("upstream listing has an invalid entry")
	}
	if _, duplicate := files[entry.Path]; duplicate || len(files) >= maxUpstreamFiles {
		return errors.New("upstream listing has duplicate or too many entries")
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
		return "", errors.New("upstream listing links to another location")
	}
	return next.String(), nil
}

// Reasons for a failed identity check.
const (
	ReasonNotUpstream      = "file-not-upstream"
	ReasonIdentityMismatch = "upstream-identity-mismatch"
)

// checkIdentity needs each local file to match the upstream git object at
// the pinned revision. An LFS file matches by its SHA-256, which is the
// inventory digest. Other files match by their git blob ID, which this
// function computes from the same bytes as a fresh SHA-256.
func checkIdentity(root string, digests map[string]string, upstream map[string]UpstreamFile) (string, error) {
	for name, digest := range digests {
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
		if blob != entry.OID {
			return ReasonIdentityMismatch, nil
		}
	}
	return "", nil
}

// gitBlobID hashes "blob <size>\x00" and the content. A size other than the
// upstream size is a mismatch. The content must still match the inventory.
func gitBlobID(file string, size int64, digest string) (string, error) {
	f, err := openRegular(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, maxGitBlobBytes+1))
	if err != nil {
		return "", errors.New("model file is unreadable")
	}
	if sum := sha256.Sum256(content); hex.EncodeToString(sum[:]) != digest {
		return "", errChanged
	}
	if int64(len(content)) != size {
		return "", nil
	}
	// Git blob IDs are SHA-1; the source publishes no other digest for small files.
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil)), nil
}
