package selinux

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// DefaultStoreDir holds the root command line's policy answers.
const DefaultStoreDir = "/var/cache/basalt-assistant/policy"

// Store keeps policy answers across runs of the root command line, so that
// `basalt why` asked twice about the same event (or about another unit
// running the same program) does not walk the policy again. An answer is
// only valid for the policy it came from: entries are keyed on the boot ID
// and the kernel's policy load counter, and anything written for another
// boot or load is ignored and removed.
//
// Trust: only the root command line uses a store (never the confined daemon
// or the MCP server), and it reads a file only when the directory and the
// file belong to root, are not writable by anyone else, the file is a plain
// file with one link opened without following symbolic links, and its
// SELinux type is not one of the assistant daemon's own types (which the
// confined daemon may write). A file that fails any check is ignored, so the
// worst case is a slower diagnosis.
type Store struct {
	Dir string
	// Owner is the uid the directory and files must belong to (0; tests
	// use their own).
	Owner int
	// BootID returns the kernel's boot ID (tests replace it).
	BootID func() (string, bool)
	// Label returns a file's SELinux context ("" when unknown); tests
	// replace it.
	Label func(path string) string
}

// NewStore returns the root store in dir.
func NewStore(dir string) *Store { return &Store{Dir: dir} }

const (
	storeVersion    = 1
	storeMaxBytes   = 8 << 20
	storeMaxEntries = 4096
)

type storeFile struct {
	Version int               `json:"version"`
	Boot    string            `json:"boot"`
	Load    string            `json:"load"`
	Answers map[string]string `json:"answers"`
}

func (s *Store) bootID() (string, bool) {
	if s.BootID != nil {
		return s.BootID()
	}
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", false
	}
	id := strings.TrimSpace(string(b))
	return id, id != ""
}

func (s *Store) label(p string) string {
	if s.Label != nil {
		return s.Label(p)
	}
	buf := make([]byte, 256)
	n, err := syscall.Getxattr(p, "security.selinux", buf)
	if err != nil || n <= 0 {
		return ""
	}
	return strings.TrimRight(string(buf[:n]), "\x00")
}

var reSafeName = regexp.MustCompile(`[^A-Za-z0-9-]`)

func (s *Store) file(load string) (string, string, bool) {
	boot, ok := s.bootID()
	if !ok || load == "" {
		return "", "", false
	}
	name := reSafeName.ReplaceAllString(boot, "") + "-" + reSafeName.ReplaceAllString(load, "") + ".json"
	return filepath.Join(s.Dir, name), boot, true
}

// daemonType reports an SELinux type the confined assistant may write.
func daemonType(ctx string) bool {
	t := Type(ctx)
	return strings.HasPrefix(t, "basalt_assistant") || strings.HasPrefix(t, "basalt_mcp")
}

// trustedDir checks that a directory belongs to the owner, is a directory
// (not a link to one) and that nobody else may write it.
func (s *Store) trustedDir(dir string, private bool) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(sys.Uid) != s.Owner {
		return fmt.Errorf("%s does not belong to uid %d", dir, s.Owner)
	}
	mask := os.FileMode(0o022)
	if private {
		mask = 0o077
	}
	if st.Mode().Perm()&mask != 0 {
		return fmt.Errorf("%s is mode %04o", dir, st.Mode().Perm())
	}
	if daemonType(s.label(dir)) {
		return fmt.Errorf("%s has a type the daemon may write", dir)
	}
	return nil
}

func (s *Store) trustedTree() error {
	if err := s.trustedDir(filepath.Dir(s.Dir), false); err != nil {
		return err
	}
	return s.trustedDir(s.Dir, true)
}

// Load returns the answers stored for the loaded policy load, or nothing
// when there are none or they cannot be trusted.
func (s *Store) Load(load string) map[string]string {
	p, boot, ok := s.file(load)
	if !ok || s.trustedTree() != nil {
		return nil
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 || st.Size() > storeMaxBytes {
		return nil
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(sys.Uid) != s.Owner || sys.Nlink != 1 {
		return nil
	}
	if daemonType(s.label(p)) {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(f, storeMaxBytes))
	if err != nil {
		return nil
	}
	var sf storeFile
	if json.Unmarshal(b, &sf) != nil || sf.Version != storeVersion || sf.Boot != boot || sf.Load != load {
		return nil
	}
	return sf.Answers
}

// Save replaces the stored answers for this policy load (atomically) and
// removes the files of other loads.
func (s *Store) Save(load string, answers map[string]string) error {
	p, boot, ok := s.file(load)
	if !ok {
		return errors.New("no boot ID or policy load identity")
	}
	if len(answers) > storeMaxEntries {
		return errors.New("too many answers to store")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if err := s.trustedTree(); err != nil {
		return err
	}
	b, err := json.Marshal(storeFile{Version: storeVersion, Boot: boot, Load: load, Answers: answers})
	if err != nil {
		return err
	}
	if len(b) > storeMaxBytes {
		return errors.New("answers too large to store")
	}
	tmp, err := os.CreateTemp(s.Dir, ".answers-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return err
	}
	old, _ := filepath.Glob(filepath.Join(s.Dir, "*.json"))
	for _, o := range old {
		if o != p {
			os.Remove(o)
		}
	}
	return nil
}
