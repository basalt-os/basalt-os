package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/engine"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/i18n"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
)

// Events of the session itself (besides the engine's).
const (
	EvKeyAcked = "recovery_key_acknowledged"
	EvKeySaved = "recovery_key_saved"
)

// KeyWriter writes the recovery key file to a removable medium and returns
// where it went (for the person). The default mounts the medium in a
// temporary directory of the installer's private mount namespace, writes
// and syncs the file, and unmounts it again.
type KeyWriter func(ctx context.Context, m probe.KeyMedium, name string, content []byte) (string, error)

// KeyMedia lists the removable file systems that can take the recovery key
// now (probed again: a USB stick may just have been plugged in).
func (s *Session) KeyMedia(ctx context.Context) ([]probe.KeyMedium, error) {
	f, err := s.Facts(ctx, true)
	if err != nil {
		return nil, err
	}
	return f.KeyMedia, nil
}

// SaveRecoveryKey writes the recovery key to the removable file system at
// device (a path from KeyMedia). It works only while the key is shown,
// before the acknowledgement; saving is not an acknowledgement, the person
// still confirms they stored it.
func (s *Session) SaveRecoveryKey(ctx context.Context, device string) (string, error) {
	s.mu.Lock()
	key := s.recKey
	pv := s.preview
	s.mu.Unlock()
	if key == "" || pv == nil {
		return "", errors.New(i18n.T("there is no recovery key to save"))
	}
	media, err := s.KeyMedia(ctx)
	if err != nil {
		return "", err
	}
	var m *probe.KeyMedium
	for i := range media {
		if media[i].Path == device {
			m = &media[i]
		}
	}
	if m == nil {
		return "", fmt.Errorf(i18n.T("%s is not a removable file system the key can be written to"), device)
	}
	where, err := s.writeKey(ctx, *m, key, *pv)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.keySaved = append(s.keySaved, where)
	s.broadcast(engine.Event{Type: EvKeySaved, Text: where})
	s.mu.Unlock()
	return where, nil
}

// autoSaveKey writes the key to the medium the plan names
// (encryption.recovery_key_media) after a successful installation; that
// counts as the acknowledgement. A failure leaves the key on the screen.
func (s *Session) autoSaveKey(ctx context.Context, pv Preview) {
	label := pv.Resolved.Plan.Encryption.RecoveryKeyMedia
	s.mu.Lock()
	key := s.recKey
	s.mu.Unlock()
	if label == "" || key == "" {
		return
	}
	fail := func(err error) {
		s.mu.Lock()
		s.broadcast(engine.Event{Type: engine.EvWarning, Text: fmt.Sprintf(i18n.T("The recovery key could not be written to %s (%v). It is shown on the screen instead."), label, err)})
		s.mu.Unlock()
	}
	media, err := s.KeyMedia(ctx)
	if err != nil {
		fail(err)
		return
	}
	m := plan.FindKeyMedium(media, label)
	if m == nil {
		fail(errors.New(i18n.T("no removable file system with that label")))
		return
	}
	where, err := s.writeKey(ctx, *m, key, pv)
	if err != nil {
		fail(err)
		return
	}
	s.mu.Lock()
	s.keySaved = append(s.keySaved, where)
	s.broadcast(engine.Event{Type: EvKeySaved, Text: where})
	s.acked, s.recKey = true, ""
	s.broadcast(engine.Event{Type: EvKeyAcked})
	s.mu.Unlock()
}

func (s *Session) writeKey(ctx context.Context, m probe.KeyMedium, key string, pv Preview) (string, error) {
	r := pv.Resolved
	now := time.Now().UTC()
	name := fmt.Sprintf("basalt-recovery-key-%s-%s.txt", r.Plan.Hostname, now.Format("20060102-150405"))
	content := KeyFileContent(key, r.Plan.Hostname, r.Disk.Path, r.LUKSUUID, now)
	w := s.opt.KeyWriter
	if w == nil {
		w = MountAndWrite
	}
	return w(ctx, m, name, []byte(content))
}

// KeyFileContent is the text of the recovery key file: the key on a line of
// its own, so it can be copied, and what it is for.
func KeyFileContent(key, host, disk, luksUUID string, when time.Time) string {
	var b strings.Builder
	b.WriteString(i18n.T("Basalt OS disk recovery key") + "\n\n")
	b.WriteString(key + "\n\n")
	fmt.Fprintf(&b, i18n.T("Host: %s")+"\n", host)
	fmt.Fprintf(&b, i18n.T("Disk: %s (LUKS2 volume %s)")+"\n", disk, luksUUID)
	fmt.Fprintf(&b, i18n.T("Created: %s")+"\n\n", when.Format(time.RFC3339))
	b.WriteString(i18n.T("Type this key at the disk unlock prompt when the TPM does not open the disk (Secure Boot changed, the disk moved to another machine). Keep it off the machine it protects, and keep this file somewhere safe.") + "\n")
	return b.String()
}

// MountAndWrite is the default KeyWriter.
func MountAndWrite(ctx context.Context, m probe.KeyMedium, name string, content []byte) (string, error) {
	dir := m.Mountpoint
	if dir == "" {
		if err := os.MkdirAll("/run/basalt-installer", 0o700); err != nil {
			return "", err
		}
		tmp, err := os.MkdirTemp("/run/basalt-installer", "key-media-")
		if err != nil {
			return "", err
		}
		defer os.Remove(tmp)
		if out, err := exec.CommandContext(ctx, "mount", "-o", "rw,nosuid,nodev,noexec", m.Path, tmp).CombinedOutput(); err != nil {
			return "", fmt.Errorf("mount %s: %v: %s", m.Path, err, strings.TrimSpace(string(out)))
		}
		defer func() { _ = exec.Command("umount", tmp).Run() }()
		dir = tmp
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	label := m.Label
	if label == "" {
		label = m.Path
	}
	return fmt.Sprintf("%s (%s): %s", m.Path, label, name), nil
}
