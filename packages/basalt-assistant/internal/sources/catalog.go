// Package sources knows the software sources (package repositories and
// Flatpak remotes) Basalt OS can add besides its own channels: a small
// catalog of well-known sources, each pinned here with its official URL
// and the fingerprint of its signing key, plus custom sources a person
// adds by URL. Adding a source makes its signing key a new trust root, so
// it is always a proposal (source.add) the person confirms with an
// administrator's password; this package checks keys and writes the
// repository files only inside `basalt __source`, which runs as one of a
// confirmed proposal's commands.
package sources

import (
	"fmt"
	"regexp"
	"strings"
)

// Kinds of source.
const (
	KindRPM     = "rpm"     // a dnf repository
	KindFlatpak = "flatpak" // a system-wide Flatpak remote
)

// URL types of a source: an RPM repository by base URL or metalink, a
// Flatpak remote by its .flatpakrepo file.
const (
	URLBase        = "baseurl"
	URLMetalink    = "metalink"
	URLFlatpakrepo = "flatpakrepo"
)

// Catalog ids that are not pinned entries.
const (
	CatalogCustom = "custom" // a .repo URL, or a base URL plus a key URL, typed by the person
	CatalogCopr   = "copr"   // a Fedora COPR project by name: its key is per project, shown, not pinned
)

// KeyEmbedded is the key_url of a Flatpak remote: its key is inside the
// .flatpakrepo file.
const KeyEmbedded = "embedded"

// Repo is one repository (or remote) of a catalog entry.
type Repo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URLType string `json:"url_type"`
	URL     string `json:"url"`
}

// Entry is a well-known source, pinned: what it is, where it is published
// and the fingerprint of the key that signs it, as its publisher documents
// it. A source.add for an entry must carry exactly these values.
type Entry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Publisher   string `json:"publisher"`
	Description string `json:"description"`
	Homepage    string `json:"homepage"`
	KeyURL      string `json:"key_url"`
	// Fingerprint of the primary key (40 hex digits, upper case).
	Fingerprint string `json:"fingerprint"`
	// KeyOwner is the key's user id as published, shown with the
	// fingerprint (the key fetched at proposal time must say the same).
	KeyOwner string `json:"key_owner"`
	// RepoGPGCheck: the source signs its repository metadata. Sources that
	// do not (RPM Fusion) are still package-signed (gpgcheck=1) and use a
	// metalink, which carries checksums of the metadata over HTTPS.
	RepoGPGCheck bool   `json:"repo_gpgcheck"`
	Repos        []Repo `json:"repos"`
}

// Catalog is the closed list of well-known sources. Fingerprints were
// checked against each publisher's documentation (2026-10-06):
// rpmfusion.org/keys, the Google Linux repositories page, Microsoft's
// packages.microsoft.com docs, docs.docker.com and flathub.org.
var Catalog = []Entry{
	{ID: "flathub", Name: "Flathub", Kind: KindFlatpak, Publisher: "Flathub",
		Description: "apps for every Linux desktop, packaged as Flatpaks",
		Homepage:    "https://flathub.org", KeyURL: KeyEmbedded,
		Fingerprint: "6E5C05D979C76DAF93C081354184DD4D907A7CAE", KeyOwner: "Flathub Repo Signing Key <flathub@flathub.org>",
		RepoGPGCheck: true,
		Repos:        []Repo{{ID: "flathub", Name: "Flathub", URLType: URLFlatpakrepo, URL: "https://dl.flathub.org/repo/flathub.flatpakrepo"}}},
	{ID: "rpmfusion-free", Name: "RPM Fusion (free)", Kind: KindRPM, Publisher: "RPM Fusion",
		Description: "free software Fedora does not ship, for example multimedia codecs",
		Homepage:    "https://rpmfusion.org",
		KeyURL:      "https://rpmfusion.org/keys?action=AttachFile&do=get&target=RPM-GPG-KEY-rpmfusion-free-fedora-2020",
		Fingerprint: "E9A491A3DE247814E7E067EAE06F8ECDD651FF2E", KeyOwner: "RPM Fusion free repository for Fedora (2020) <rpmfusion-buildsys@lists.rpmfusion.org>",
		Repos: []Repo{
			{ID: "rpmfusion-free", Name: "RPM Fusion for Fedora $releasever - Free", URLType: URLMetalink,
				URL: "https://mirrors.rpmfusion.org/metalink?repo=free-fedora-$releasever&arch=$basearch"},
			{ID: "rpmfusion-free-updates", Name: "RPM Fusion for Fedora $releasever - Free - Updates", URLType: URLMetalink,
				URL: "https://mirrors.rpmfusion.org/metalink?repo=free-fedora-updates-released-$releasever&arch=$basearch"},
		}},
	{ID: "rpmfusion-nonfree", Name: "RPM Fusion (nonfree)", Kind: KindRPM, Publisher: "RPM Fusion",
		Description: "software that is not free but may be redistributed",
		Homepage:    "https://rpmfusion.org",
		KeyURL:      "https://rpmfusion.org/keys?action=AttachFile&do=get&target=RPM-GPG-KEY-rpmfusion-nonfree-fedora-2020",
		Fingerprint: "79BDB88F9BBF73910FD4095B6A2AF96194843C65", KeyOwner: "RPM Fusion nonfree repository for Fedora (2020) <rpmfusion-buildsys@lists.rpmfusion.org>",
		Repos: []Repo{
			{ID: "rpmfusion-nonfree", Name: "RPM Fusion for Fedora $releasever - Nonfree", URLType: URLMetalink,
				URL: "https://mirrors.rpmfusion.org/metalink?repo=nonfree-fedora-$releasever&arch=$basearch"},
			{ID: "rpmfusion-nonfree-updates", Name: "RPM Fusion for Fedora $releasever - Nonfree - Updates", URLType: URLMetalink,
				URL: "https://mirrors.rpmfusion.org/metalink?repo=nonfree-fedora-updates-released-$releasever&arch=$basearch"},
		}},
	{ID: "google-chrome", Name: "Google Chrome", Kind: KindRPM, Publisher: "Google",
		Description: "the Google Chrome web browser, from Google",
		Homepage:    "https://www.google.com/chrome/", KeyURL: "https://dl.google.com/linux/linux_signing_key.pub",
		Fingerprint: "EB4C1BFD4F042F6DDDCCEC917721F63BD38B4796", KeyOwner: "Google Inc. (Linux Packages Signing Authority) <linux-packages-keymaster@google.com>",
		RepoGPGCheck: true,
		Repos:        []Repo{{ID: "google-chrome", Name: "Google Chrome", URLType: URLBase, URL: "https://dl.google.com/linux/chrome/rpm/stable/$basearch"}}},
	{ID: "vscode", Name: "Visual Studio Code", Kind: KindRPM, Publisher: "Microsoft",
		Description: "the Visual Studio Code editor, from Microsoft",
		Homepage:    "https://code.visualstudio.com", KeyURL: "https://packages.microsoft.com/keys/microsoft.asc",
		Fingerprint: "BC528686B50D79E339D3721CEB3E94ADBE1229CF", KeyOwner: "Microsoft (Release signing) <gpgsecurity@microsoft.com>",
		RepoGPGCheck: true,
		Repos:        []Repo{{ID: "vscode", Name: "Visual Studio Code", URLType: URLBase, URL: "https://packages.microsoft.com/yumrepos/vscode"}}},
	{ID: "docker-ce", Name: "Docker CE", Kind: KindRPM, Publisher: "Docker",
		Description: "the Docker Engine and its tools, from Docker",
		Homepage:    "https://docs.docker.com/engine/install/fedora/", KeyURL: "https://download.docker.com/linux/fedora/gpg",
		Fingerprint: "060A61C51B558A7F742B77AAC52FEB6B621E9F35", KeyOwner: "Docker Release (CE rpm) <docker@docker.com>",
		RepoGPGCheck: true,
		Repos:        []Repo{{ID: "docker-ce-stable", Name: "Docker CE Stable - $basearch", URLType: URLBase, URL: "https://download.docker.com/linux/fedora/$releasever/$basearch/stable"}}},
}

// Lookup returns a catalog entry.
func Lookup(id string) (Entry, bool) {
	for _, e := range Catalog {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// LookupRepo returns the catalog entry and repo with this repository id.
func LookupRepo(catalog, repoID string) (Entry, Repo, bool) {
	e, ok := Lookup(catalog)
	if !ok {
		return Entry{}, Repo{}, false
	}
	for _, r := range e.Repos {
		if r.ID == repoID {
			return e, r, true
		}
	}
	return Entry{}, Repo{}, false
}

var (
	// ReID is a source's repository id: lower case, starts with a letter.
	ReID = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,62}$`)
	// reURL: https only, no spaces, quotes or shell characters; dnf's
	// $releasever and $basearch may appear.
	reURL = regexp.MustCompile(`^https://[A-Za-z0-9.-]{1,253}(:[0-9]{1,5})?(/[A-Za-z0-9._~%/:?&=+,@$-]*)?$`)
	reFpr = regexp.MustCompile(`^[0-9A-F]{40}$`)
	// reCopr: OWNER/PROJECT (a group owner starts with @).
	reCopr = regexp.MustCompile(`^@?[A-Za-z0-9][A-Za-z0-9_.-]{0,63}/[A-Za-z0-9][A-Za-z0-9_.+-]{0,99}$`)
	reName = regexp.MustCompile(`^[^\x00-\x1f\x7f\[\]]{1,120}$`)
)

// reserved repository ids: Fedora's and Basalt's own, which a source may
// never shadow.
func reserved(id string) bool {
	for _, p := range []string{"fedora", "updates", "rawhide", "koji"} {
		if id == p || strings.HasPrefix(id, p+"-") {
			return true
		}
	}
	return strings.HasPrefix(id, "basalt")
}

// CheckURL accepts an https URL without characters that could change how
// a repository file or a command line reads it. Plain http is refused:
// the transport is part of the trust (the key comes over it).
func CheckURL(u string) error {
	if strings.HasPrefix(strings.ToLower(u), "http://") {
		return fmt.Errorf("%q uses plain http: only https sources can be added", u)
	}
	if !reURL.MatchString(u) || len(u) > 1000 {
		return fmt.Errorf("%q is not an https URL Basalt accepts", u)
	}
	return nil
}

// NormFingerprint removes spaces and upper-cases a fingerprint.
func NormFingerprint(s string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(s, " ", ""), ":", ""))
}

// Short is the last 16 hex digits of a fingerprint in groups of four, the
// short form shown next to a source ("E4EE D5EC A395 B302").
func Short(fpr string) string {
	f := NormFingerprint(fpr)
	if len(f) < 16 {
		return f
	}
	f = f[len(f)-16:]
	return f[0:4] + " " + f[4:8] + " " + f[8:12] + " " + f[12:16]
}

// Spaced is a full fingerprint in groups of four.
func Spaced(fpr string) string {
	f := NormFingerprint(fpr)
	var parts []string
	for i := 0; i+4 <= len(f); i += 4 {
		parts = append(parts, f[i:i+4])
	}
	return strings.Join(parts, " ")
}

// CoprURLs are the repository and key URLs of a COPR project.
func CoprURLs(project string) (repo, key string, err error) {
	if !reCopr.MatchString(project) {
		return "", "", fmt.Errorf("%q is not a COPR project (OWNER/PROJECT)", project)
	}
	owner, name, _ := strings.Cut(project, "/")
	base := "https://download.copr.fedorainfracloud.org/results/" + owner + "/" + name
	return base + "/fedora-$releasever-$basearch/", base + "/pubkey.gpg", nil
}

// CoprID is the repository id of a COPR project.
func CoprID(project string) string {
	id := "copr-" + strings.ToLower(strings.NewReplacer("/", "-", "@", "group-", "+", "-").Replace(project))
	if len(id) > 63 {
		id = id[:63]
	}
	return strings.TrimRight(id, "-.")
}

// Params are the parameters of a source.add action, typed.
type Params struct {
	ID, Name, Kind, URLType, URL, KeyURL, Fingerprint string
	GPGCheck, RepoGPGCheck, Catalog, Group            string
}

// FromMap reads source.add parameters.
func FromMap(m map[string]string) Params {
	return Params{ID: m["id"], Name: m["name"], Kind: m["kind"], URLType: m["url_type"], URL: m["url"], KeyURL: m["key_url"],
		Fingerprint: m["fingerprint"], GPGCheck: m["gpgcheck"], RepoGPGCheck: m["repo_gpgcheck"], Catalog: m["catalog"], Group: m["group"]}
}

// Map is the parameters as an action's map.
func (p Params) Map() map[string]string {
	return map[string]string{"id": p.ID, "name": p.Name, "kind": p.Kind, "url_type": p.URLType, "url": p.URL, "key_url": p.KeyURL,
		"fingerprint": p.Fingerprint, "gpgcheck": p.GPGCheck, "repo_gpgcheck": p.RepoGPGCheck, "catalog": p.Catalog, "group": p.Group}
}

// Validate is the validator of source.add. It refuses: plain http, a
// missing key, signature checks turned off, an id that shadows Fedora or
// Basalt, and any catalog entry whose URL, key or fingerprint differ from
// the pinned values.
func (p Params) Validate() error {
	if !ReID.MatchString(p.ID) || reserved(p.ID) {
		return fmt.Errorf("source id %q (lower case letters, digits, '.', '_', '-'; not a Fedora or Basalt repository)", p.ID)
	}
	if !reName.MatchString(p.Name) {
		return fmt.Errorf("source name %q", p.Name)
	}
	if !ReID.MatchString(p.Group) {
		return fmt.Errorf("source group %q", p.Group)
	}
	if p.GPGCheck != "1" {
		return fmt.Errorf("gpgcheck %q: package signature checks are always on for an added source", p.GPGCheck)
	}
	if p.RepoGPGCheck != "1" && p.RepoGPGCheck != "0" {
		return fmt.Errorf("repo_gpgcheck %q", p.RepoGPGCheck)
	}
	if err := CheckURL(p.URL); err != nil {
		return err
	}
	if !reFpr.MatchString(p.Fingerprint) {
		return fmt.Errorf("fingerprint %q is not a 40 digit key fingerprint: a source needs its signing key", p.Fingerprint)
	}
	switch p.Kind {
	case KindRPM:
		if p.URLType != URLBase && p.URLType != URLMetalink {
			return fmt.Errorf("url_type %q (baseurl or metalink)", p.URLType)
		}
		if p.KeyURL == "" || p.KeyURL == KeyEmbedded {
			return fmt.Errorf("a repository needs the URL of its signing key")
		}
		if err := CheckURL(p.KeyURL); err != nil {
			return fmt.Errorf("key: %w", err)
		}
	case KindFlatpak:
		if p.URLType != URLFlatpakrepo || p.KeyURL != KeyEmbedded {
			return fmt.Errorf("a Flatpak remote is added from its .flatpakrepo file, with the key it carries")
		}
		if p.RepoGPGCheck != "1" {
			return fmt.Errorf("a Flatpak remote is always signature checked")
		}
	default:
		return fmt.Errorf("source kind %q (rpm or flatpak)", p.Kind)
	}
	switch p.Catalog {
	case CatalogCustom:
		if p.Kind != KindRPM {
			return fmt.Errorf("custom sources are dnf repositories")
		}
		// A custom source must sign its repository metadata too.
		if p.RepoGPGCheck != "1" {
			return fmt.Errorf("a custom source must sign its repository metadata (repo_gpgcheck=1)")
		}
	case CatalogCopr:
		if p.Kind != KindRPM || p.URLType != URLBase || !strings.HasPrefix(p.ID, "copr-") {
			return fmt.Errorf("a COPR source is a dnf repository named copr-OWNER-PROJECT")
		}
		project := strings.TrimSuffix(strings.TrimPrefix(p.URL, "https://download.copr.fedorainfracloud.org/results/"), "/fedora-$releasever-$basearch/")
		repo, key, err := CoprURLs(project)
		if err != nil || repo != p.URL || key != p.KeyURL {
			return fmt.Errorf("the COPR repository and key URLs must be the project's own (download.copr.fedorainfracloud.org)")
		}
	default:
		e, r, ok := LookupRepo(p.Catalog, p.ID)
		if !ok {
			return fmt.Errorf("%q is not a repository of the catalog entry %q", p.ID, p.Catalog)
		}
		if e.Kind != p.Kind || r.URLType != p.URLType || r.URL != p.URL || e.KeyURL != p.KeyURL || p.Group != e.ID {
			return fmt.Errorf("%s: the source differs from the one Basalt pins for %s", p.ID, e.Name)
		}
		if p.Fingerprint != e.Fingerprint {
			return fmt.Errorf("%s: key fingerprint %s is not the one Basalt pins for %s (%s)", p.ID, Spaced(p.Fingerprint), e.Name, Spaced(e.Fingerprint))
		}
		if (p.RepoGPGCheck == "1") != e.RepoGPGCheck {
			return fmt.Errorf("%s: repo_gpgcheck differs from the catalog", p.ID)
		}
	}
	return nil
}

// Argv is the command a confirmed source.add runs: the assistant's own
// hidden subcommand, which fetches the key, checks its fingerprint and
// writes the repository (docs/updates.md).
func (p Params) Argv() []string {
	return []string{"basalt", "__source", "add", "--id", p.ID, "--name", p.Name, "--kind", p.Kind, "--url-type", p.URLType,
		"--url", p.URL, "--key-url", p.KeyURL, "--fingerprint", p.Fingerprint, "--repo-gpgcheck", p.RepoGPGCheck,
		"--catalog", p.Catalog, "--group", p.Group}
}
