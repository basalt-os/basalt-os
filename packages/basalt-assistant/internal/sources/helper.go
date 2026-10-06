package sources

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/knowledge"
)

// MaxFetch bounds a key, a .repo or a .flatpakrepo download.
const MaxFetch = 1 << 20

// Fetcher downloads over https; tests replace it.
type Fetcher func(ctx context.Context, url string) ([]byte, error)

// HTTPFetch is the real fetcher: https only (also after redirects), 30
// seconds, at most MaxFetch bytes.
func HTTPFetch(ctx context.Context, url string) ([]byte, error) {
	if err := CheckURL(url); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cl := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("redirect to plain http refused")
		}
		if len(via) > 5 {
			return errors.New("too many redirects")
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "basalt-assistant")
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxFetch+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFetch {
		return nil, fmt.Errorf("%s: larger than %d bytes", url, MaxFetch)
	}
	return b, nil
}

// expand replaces dnf's $releasever and $basearch in a URL with this
// system's, for downloads made here.
func expand(url string) string {
	rel, arch := "44", "x86_64"
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "VERSION_ID="); ok {
				rel = strings.Trim(v, `"`)
			}
		}
	}
	return strings.NewReplacer("$releasever", rel, "$basearch", arch).Replace(url)
}

// Key is a downloaded signing key.
type Key struct {
	Data        []byte
	Fingerprint string
	Owner       string
	// Flatpakrepo is the .flatpakrepo file a Flatpak key came in.
	Flatpakrepo []byte
}

// FetchKey downloads a source's key (for a Flatpak remote, the
// .flatpakrepo and the key inside it) and reads it. The file must hold
// exactly one primary key: rpm would trust every key in it.
func FetchKey(ctx context.Context, fetch Fetcher, kind, url, keyURL string) (Key, error) {
	var k Key
	var err error
	if kind == KindFlatpak {
		k.Flatpakrepo, err = fetch(ctx, url)
		if err != nil {
			return k, err
		}
		k.Data, err = FlatpakrepoKey(k.Flatpakrepo)
	} else {
		k.Data, err = fetch(ctx, expand(keyURL))
	}
	if err != nil {
		return k, err
	}
	ks, err := knowledge.CertKeys(k.Data)
	if err != nil {
		return k, fmt.Errorf("the signing key: %w", err)
	}
	if len(ks) != 1 {
		return k, fmt.Errorf("the key file holds %d keys; a source must have exactly one", len(ks))
	}
	k.Fingerprint = ks[0].Fingerprint
	if len(ks[0].UserIDs) > 0 {
		k.Owner = ks[0].UserIDs[0]
	}
	return k, nil
}

// FlatpakrepoKey returns the key of a .flatpakrepo file (GPGKey=base64).
func FlatpakrepoKey(b []byte) ([]byte, error) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<16), MaxFetch)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "GPGKey="); ok {
			return base64.StdEncoding.DecodeString(strings.TrimSpace(v))
		}
	}
	return nil, errors.New("the .flatpakrepo file has no GPGKey: Flatpak remotes without a key are not added")
}

// RepoFile is a parsed .repo file of a custom source: its first section.
type RepoFile struct {
	ID, Name, URLType, URL, KeyURL string
	GPGCheck, RepoGPGCheck         bool
}

// ParseRepoFile reads the first repository of a .repo file a person gave
// by URL. It refuses what Basalt never adds: no key, a key that is not an
// https URL, signature checks off, plain http.
func ParseRepoFile(b []byte) (RepoFile, error) {
	tmp, err := os.CreateTemp("", "basalt-repo-*.repo")
	if err != nil {
		return RepoFile{}, err
	}
	defer os.Remove(tmp.Name())
	_, _ = tmp.Write(b)
	tmp.Close()
	secs := parseINI(tmp.Name())
	if len(secs) == 0 {
		return RepoFile{}, errors.New("the .repo file has no repository")
	}
	s := secs[0]
	r := RepoFile{ID: strings.ToLower(s.ID), Name: s.Keys["name"], GPGCheck: truthy(s.Keys["gpgcheck"]), RepoGPGCheck: truthy(s.Keys["repo_gpgcheck"])}
	switch {
	case s.Keys["baseurl"] != "":
		r.URLType, r.URL = URLBase, strings.Fields(strings.ReplaceAll(s.Keys["baseurl"], ",", " "))[0]
	case s.Keys["metalink"] != "":
		r.URLType, r.URL = URLMetalink, s.Keys["metalink"]
	default:
		return r, errors.New("the repository has no baseurl or metalink")
	}
	if err := CheckURL(r.URL); err != nil {
		return r, err
	}
	keys := strings.Fields(strings.ReplaceAll(s.Keys["gpgkey"], ",", " "))
	if len(keys) == 0 {
		return r, errors.New("the repository names no signing key (gpgkey): sources without a key are not added")
	}
	r.KeyURL = keys[0]
	if err := CheckURL(r.KeyURL); err != nil {
		return r, fmt.Errorf("its key: %w", err)
	}
	if !r.GPGCheck {
		return r, errors.New("the repository turns package signature checks off (gpgcheck=0): it is not added")
	}
	if r.Name == "" {
		r.Name = r.ID
	}
	return r, nil
}

// Writer carries what `basalt __source` needs; tests replace Run.
type Writer struct {
	Paths Paths
	Fetch Fetcher
	// Run runs a program (rpm --import, flatpak, restorecon).
	Run func(ctx context.Context, argv ...string) (string, error)
	Now func() time.Time
}

// RealRun runs a program and returns its output.
func RealRun(ctx context.Context, argv ...string) (string, error) {
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // fixed argv
	return strings.TrimSpace(string(out)), err
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".basalt-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// repoText is the .repo file of an RPM source.
func repoText(p Params, keyPath string, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Added by Basalt OS on %s (source.add, %s).\n", now.UTC().Format(time.RFC3339), p.Catalog)
	fmt.Fprintf(&b, "# Signing key %s. Turn it off or remove it in Settings, Updates and channels,\n", Spaced(p.Fingerprint))
	fmt.Fprintf(&b, "# or with: sudo basalt channels remove %s\n", p.Group)
	fmt.Fprintf(&b, "[%s]\nname=%s\n%s=%s\n", p.ID, p.Name, p.URLType, p.URL)
	fmt.Fprintf(&b, "enabled=1\ngpgcheck=1\nrepo_gpgcheck=%s\ngpgkey=file://%s\nskip_if_unavailable=True\nmetadata_expire=6h\n", p.RepoGPGCheck, keyPath)
	return b.String()
}

// Add is `basalt __source add`: it fetches the key again, refuses it
// unless its fingerprint is exactly the one the person confirmed, then
// writes the key, the repository (or adds the Flatpak remote) and the
// record.
func (w Writer) Add(ctx context.Context, p Params) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if HasRecord(w.Paths, p.ID) {
		return fmt.Errorf("the source %s is already added", p.ID)
	}
	k, err := FetchKey(ctx, w.Fetch, p.Kind, p.URL, p.KeyURL)
	if err != nil {
		return err
	}
	if k.Fingerprint != p.Fingerprint {
		return fmt.Errorf("the key now published at the source has fingerprint %s, not %s, which was confirmed: nothing was added",
			Spaced(k.Fingerprint), Spaced(p.Fingerprint))
	}
	now := w.Now()
	switch p.Kind {
	case KindRPM:
		keyPath := w.Paths.KeyPath(p.Group)
		if err := writeAtomic(keyPath, k.Data, 0o644); err != nil {
			return err
		}
		if out, err := w.Run(ctx, "rpm", "--import", keyPath); err != nil {
			return fmt.Errorf("rpm --import: %v: %s", err, out)
		}
		repo := w.Paths.RepoPath(p.ID)
		if _, err := os.Stat(repo); err == nil {
			return fmt.Errorf("%s exists already", repo)
		}
		if err := writeAtomic(repo, []byte(repoText(p, keyPath, now)), 0o644); err != nil {
			return err
		}
		_, _ = w.Run(ctx, "restorecon", keyPath, repo)
	case KindFlatpak:
		tmp, err := os.CreateTemp("", "basalt-*.flatpakrepo")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		_, _ = tmp.Write(k.Flatpakrepo)
		tmp.Close()
		if out, err := w.Run(ctx, w.Paths.Flatpak, "remote-add", "--system", "--if-not-exists", p.ID, tmp.Name()); err != nil {
			return fmt.Errorf("flatpak remote-add: %v: %s", err, out)
		}
	}
	rec := Record{ID: p.ID, Name: p.Name, Kind: p.Kind, Catalog: p.Catalog, Group: p.Group, URLType: p.URLType, URL: p.URL,
		KeyURL: p.KeyURL, Fingerprint: p.Fingerprint, KeyOwner: k.Owner, RepoGPGCheck: p.RepoGPGCheck == "1",
		Added: now.UTC().Format(time.RFC3339)}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := writeAtomic(w.Paths.RecordPath(p.ID), append(b, '\n'), 0o644); err != nil {
		return err
	}
	_, _ = w.Run(ctx, "restorecon", w.Paths.RecordPath(p.ID))
	return nil
}

// Remove is `basalt __source remove`: only a source Basalt added. The
// key file and the key in rpm's database go with the group's last
// repository. A Flatpak remote with apps still installed from it is not
// removed (flatpak refuses; the person removes the apps first).
func (w Writer) Remove(ctx context.Context, id string) error {
	if !HasRecord(w.Paths, id) {
		return fmt.Errorf("%s was not added through Basalt: it is not removed here", id)
	}
	var rec Record
	b, err := os.ReadFile(w.Paths.RecordPath(id))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return err
	}
	switch rec.Kind {
	case KindRPM:
		if err := os.Remove(w.Paths.RepoPath(id)); err != nil && !os.IsNotExist(err) {
			return err
		}
	case KindFlatpak:
		if out, err := w.Run(ctx, w.Paths.Flatpak, "remote-delete", "--system", id); err != nil {
			return fmt.Errorf("flatpak remote-delete: %v: %s", err, out)
		}
	}
	if err := os.Remove(w.Paths.RecordPath(id)); err != nil {
		return err
	}
	if rec.Kind == KindRPM && len(Group(w.Paths, rec.Group)) == 0 {
		_ = os.Remove(w.Paths.KeyPath(rec.Group))
		// Take the key out of rpm's database too: rpmkeys --delete (rpm
		// 4.20 and newer), else the older gpg-pubkey-<last 8 digits> name.
		f := strings.ToLower(rec.Fingerprint)
		if len(f) == 40 {
			if _, err := w.Run(ctx, "rpmkeys", "--delete", f); err != nil {
				_, _ = w.Run(ctx, "rpm", "-e", "--allmatches", "gpg-pubkey-"+f[32:])
			}
		}
	}
	return nil
}

// Stamp records which proposal added a source and who decided, once the
// apply finished (shown on the source's card).
func Stamp(p Paths, id, proposal, by string) error {
	path := p.RecordPath(id)
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return err
	}
	rec.Proposal, rec.By = proposal, by
	out, _ := json.MarshalIndent(rec, "", "  ")
	return writeAtomic(path, append(out, '\n'), 0o644)
}
