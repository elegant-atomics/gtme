// Package adapterinstall implements the client side of the bindings registry
// (SPEC §8 `gtme adapters`, ADR-042): URL-addressed refs, tarball fetch over
// HTTPS at a pinned commit, the content hash, and the `.source.json` record
// written beside an installed binding. The policy (verify-before-install, the
// verbs) lives in internal/cli; this package only moves and names bytes.
package adapterinstall

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/elegant-atomics/gtme/internal/secrets"
)

// Ref is one parsed binding address:
// github.com/<owner>/<repo>/<path>[@<tag|branch|sha>] (SPEC §8).
type Ref struct {
	Owner string
	Repo  string
	Path  string // directory inside the repository holding binding.yaml
	Ref   string // as given; empty means the repository's HEAD
}

// URL is the repository half of the address, as the index records it.
func (r Ref) URL() string { return "github.com/" + r.Owner + "/" + r.Repo }

// String prints the address back the way §8 writes it.
func (r Ref) String() string {
	s := r.URL() + "/" + r.Path
	if r.Ref != "" {
		s += "@" + r.Ref
	}
	return s
}

// ParseRef parses the §8 address form. The host is always github.com — the
// registry is deliberately not a general fetcher; a binding elsewhere is
// installed by hand on the discovery path.
func ParseRef(s string) (Ref, error) {
	var r Ref
	rest := s
	if i := strings.LastIndexByte(rest, '@'); i >= 0 {
		r.Ref = rest[i+1:]
		rest = rest[:i]
		if r.Ref == "" {
			return r, fmt.Errorf("adapters: %q: empty ref after @", s)
		}
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 4 || parts[0] != "github.com" {
		return r, fmt.Errorf("adapters: %q is not github.com/<owner>/<repo>/<path>[@ref]", s)
	}
	for _, p := range parts[1:] {
		if p == "" || p == "." || p == ".." {
			return r, fmt.Errorf("adapters: %q: empty or relative path segment", s)
		}
	}
	r.Owner, r.Repo = parts[1], parts[2]
	r.Path = strings.Join(parts[3:], "/")
	return r, nil
}

// APIBase and CodeloadBase are the two GitHub endpoints the fetch touches.
// Overridable so the M19 offline acceptance can stand up a local tarball
// server without changing the address grammar (implementation decision,
// DECISIONS.md M19 internals).
func APIBase() string {
	if v := os.Getenv("GTME_GITHUB_API"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "https://api.github.com"
}

func CodeloadBase() string {
	if v := os.Getenv("GTME_GITHUB_CODELOAD"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "https://codeload.github.com"
}

// token is the optional GITHUB_TOKEN for private repositories: the secrets
// store first (SPEC §8: "stored with `gtme secret`"), the environment second.
func token() string {
	if v, ok := secrets.Lookup("GITHUB_TOKEN"); ok && v != "" {
		return v
	}
	return os.Getenv("GITHUB_TOKEN")
}

var client = &http.Client{Timeout: 60 * time.Second}

func get(url string, accept string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if t := token(); t != "" && authorized(url) {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	return client.Do(req)
}

// authorized reports whether the URL belongs to a host the GITHUB_TOKEN is
// meant for (the API and codeload). Raw content hosts never get the token:
// the registry index is public, and raw.githubusercontent.com answers an
// invalid bearer token with 404, turning a working fetch into a phantom
// missing index (#26).
func authorized(url string) bool {
	return strings.HasPrefix(url, APIBase()+"/") || strings.HasPrefix(url, CodeloadBase()+"/")
}

// ResolveCommit resolves the ref (or the repository HEAD) to a commit sha via
// the commits endpoint, so the install is pinned by construction.
func ResolveCommit(r Ref) (string, error) {
	ref := r.Ref
	if ref == "" {
		ref = "HEAD"
	}
	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s", APIBase(), r.Owner, r.Repo, ref)
	resp, err := get(url, "application/vnd.github+json")
	if err != nil {
		return "", fmt.Errorf("adapters: resolving %s: %w", r.String(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("adapters: resolving %s: %s from %s", r.String(), resp.Status, url)
	}
	var doc struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", fmt.Errorf("adapters: resolving %s: %w", r.String(), err)
	}
	if doc.SHA == "" {
		return "", fmt.Errorf("adapters: resolving %s: no commit sha in response", r.String())
	}
	return doc.SHA, nil
}

// Fetch limits: a binding is a YAML file and some fixtures; anything bigger
// is not a binding.
const (
	maxFileBytes  = 4 << 20
	maxTotalBytes = 32 << 20
	maxFiles      = 256
)

// FetchDir downloads the repository tarball at the resolved commit and
// extracts only the binding's directory into a fresh temp dir. The caller
// removes the returned directory.
func FetchDir(r Ref, commit string) (string, error) {
	url := fmt.Sprintf("%s/%s/%s/tar.gz/%s", CodeloadBase(), r.Owner, r.Repo, commit)
	resp, err := get(url, "")
	if err != nil {
		return "", fmt.Errorf("adapters: fetching %s: %w", r.String(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("adapters: fetching %s: %s from %s", r.String(), resp.Status, url)
	}

	dir, err := os.MkdirTemp("", "gtme-adapter-fetch-*")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()

	gz, err := gzip.NewReader(io.LimitReader(resp.Body, maxTotalBytes))
	if err != nil {
		return "", fmt.Errorf("adapters: %s: not a gzip tarball: %w", r.String(), err)
	}
	tr := tar.NewReader(gz)
	files, total := 0, int64(0)
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("adapters: reading tarball for %s: %w", r.String(), err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Strip the tarball's single top-level directory (<repo>-<ref-ish>/).
		name := strings.TrimPrefix(hdr.Name, "./")
		i := strings.IndexByte(name, '/')
		if i < 0 {
			continue
		}
		rest := name[i+1:]
		if rest != r.Path && !strings.HasPrefix(rest, r.Path+"/") {
			continue
		}
		rel := strings.TrimPrefix(rest, r.Path+"/")
		if rel == "" || !filepath.IsLocal(rel) {
			continue
		}
		if files++; files > maxFiles {
			return "", fmt.Errorf("adapters: %s: more than %d files — not a binding directory", r.String(), maxFiles)
		}
		if hdr.Size > maxFileBytes {
			return "", fmt.Errorf("adapters: %s: %s is larger than %d bytes — not binding material", r.String(), rel, maxFileBytes)
		}
		if total += hdr.Size; total > maxTotalBytes {
			return "", fmt.Errorf("adapters: %s: binding directory exceeds %d bytes", r.String(), maxTotalBytes)
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxFileBytes+1))
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return "", err
		}
		found = true
	}
	if !found {
		return "", fmt.Errorf("adapters: %s: path %q not found in the repository at %s", r.String(), r.Path, commit[:min(12, len(commit))])
	}
	ok = true
	return dir, nil
}

// ContentHash is the content sha256 `.source.json` and the index record
// (SPEC §6, §8): sha256 over every file in the binding directory except
// `.source.json` itself, sorted by slash path, each written as
// path NUL body NUL. The registry's CI computes the same rule.
func ContentHash(dir string) (string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == SourceFile {
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, rel := range paths {
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(body)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SourceFile records where an installed binding came from (SPEC §6, §8).
const SourceFile = ".source.json"

// Source is the `.source.json` document: the §8 quartet (ref as given,
// resolved commit, content sha256, install time) plus the repository url and
// path, which `adapters update` needs to re-fetch (M19 internals).
type Source struct {
	URL         string `json:"url"`
	Path        string `json:"path"`
	Ref         string `json:"ref"`
	Commit      string `json:"commit"`
	SHA256      string `json:"sha256"`
	InstalledAt string `json:"installed_at"`
}

// ReadSource loads a directory's `.source.json`; (nil, nil) means the binding
// was installed by hand.
func ReadSource(dir string) (*Source, error) {
	raw, err := os.ReadFile(filepath.Join(dir, SourceFile))
	if err != nil {
		return nil, nil
	}
	var s Source
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("adapters: parsing %s: %w", filepath.Join(dir, SourceFile), err)
	}
	return &s, nil
}

// WriteSource records the pin beside the installed binding.
func WriteSource(dir string, s Source) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, SourceFile), append(raw, '\n'), 0o644)
}
