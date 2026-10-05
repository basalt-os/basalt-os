package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/i18n"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/session"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/steps"
)

// installSummary is what `install --plan` prints at the end: what was
// installed where, how it unlocks, who can log in, which repositories it
// uses, where the recovery key went, how long it took and where the log is.
func installSummary(pv session.Preview, st session.Status, keyWhere []string, logPath string) string {
	r := pv.Resolved
	p := r.Plan
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, "  "+format+"\n", a...) }
	b.WriteString("\n" + i18n.T("Basalt OS is installed.") + "\n")
	disk := r.Disk.Path + ", " + probe.HumanSize(r.Disk.SizeBytes)
	if r.Disk.Model != "" {
		disk += ", " + r.Disk.Model
	}
	line(i18n.T("Disk:         %s"), disk)
	if len(r.Kept) > 0 {
		var news []string
		for _, np := range r.NewParts {
			news = append(news, np.Dev)
		}
		line(i18n.T("Partitions:   %d kept as they were, new %s"), len(r.Kept), strings.Join(news, " "))
	}
	line(i18n.T("Edition:      %s, profile %s (%s)"), p.Edition, r.Profile, r.ProfileReason)
	line(i18n.T("Host name:    %s"), p.Hostname)
	switch {
	case !p.Encrypted():
		line("%s", i18n.T("Encryption:   none (plain btrfs)"))
	case p.EncryptsHome():
		line(i18n.T("Encryption:   /home only, LUKS2, unlocks with %s"), p.Encryption.Unlock)
	default:
		line(i18n.T("Encryption:   LUKS2, unlocks with %s"), p.Encryption.Unlock)
	}
	if e := r.Existing; e != nil {
		line(i18n.T("Home:         existing %s (%s), kept as it was"), e.Device, e.FSType)
	}
	switch {
	case !r.TPM2:
		line("%s", i18n.T("TPM:          none found"))
	case r.EnrollTPM:
		line(i18n.T("TPM:          2.0, key sealed to PCR 7 (Secure Boot %s)"), r.SecureBoot)
	default:
		line("%s", i18n.T("TPM:          2.0, not used by this plan"))
	}
	root := i18n.T("locked")
	if n := len(p.Accounts.Root.SSHKeys); n > 0 {
		root = fmt.Sprintf(i18n.N("%d SSH key", "%d SSH keys", n), n)
	}
	if p.Accounts.Root.Password != "" || p.Accounts.Root.PasswordHash != "" {
		root += ", " + i18n.T("password")
	}
	line(i18n.T("Root:         %s"), root)
	if u := p.Accounts.User; u != nil {
		var how []string
		if u.Password != "" || u.PasswordHash != "" {
			how = append(how, i18n.T("password"))
		}
		if n := len(u.SSHKeys); n > 0 {
			how = append(how, fmt.Sprintf(i18n.N("%d SSH key", "%d SSH keys", n), n))
		}
		role := i18n.T("user")
		if p.IsAdminUser() {
			role = i18n.T("administrator")
		}
		line(i18n.T("User:         %s (%s): %s"), u.Name, role, strings.Join(how, ", "))
		if u.HomeDir != "" || u.UID != 0 {
			line(i18n.T("              home %s, uid %d, gid %d"), u.HomeDir, u.UID, u.GID)
		}
	}
	net := p.Network.Mode
	if p.Network.Mode == "static" {
		net = fmt.Sprintf("static %s on %s", p.Network.Address, p.Network.Interface)
	}
	line(i18n.T("Network:      %s"), net)
	basalt, tools := steps.InstalledRepoURLs(r)
	line(i18n.T("Repositories: basalt %s"), basalt)
	if p.Repos.Tools == nil || *p.Repos.Tools {
		line(i18n.T("              basalt-tools %s"), tools)
	}
	if t := p.Repos.Testing; t != nil && *t {
		line("%s", i18n.T("              basalt-testing (pre-release packages) on"))
	}
	if t := p.Repos.ThirdParty.TUITools; t == nil || *t {
		line("%s", i18n.T("              tui-tools (third party)"))
	}
	if p.Encrypted() {
		where := strings.Join(keyWhere, "; ")
		if where == "" {
			where = i18n.T("not stored")
		}
		line(i18n.T("Recovery key: %s"), where)
	}
	line(i18n.T("Time:         %s"), (time.Duration(st.Seconds) * time.Second).String())
	line(i18n.T("Install log:  %s (a copy is in /var/log/basalt-installer/ on the new system)"), logPath)
	return b.String()
}

// checkDomain refuses to install from an SELinux domain that cannot enter
// install_t. On the live image the installer's binary is labeled
// install_exec_t, and Fedora's policy lets init (services) and
// unconfined_t (a root login) start it in install_t, the domain of
// Anaconda; a shell in another domain (the systemd debug shell runs in
// initrc_t) would run it there, and the installation would fail later on
// an SELinux denial.
func checkDomain() error {
	enforce, err := os.ReadFile("/sys/fs/selinux/enforce")
	if err != nil || strings.TrimSpace(string(enforce)) != "1" {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	buf := make([]byte, 256)
	n, err := syscall.Getxattr(self, "security.selinux", buf)
	if err != nil || !strings.Contains(string(buf[:n]), ":install_exec_t:") {
		return nil // not the live image's setup
	}
	cur, err := os.ReadFile("/proc/self/attr/current")
	if err != nil {
		return nil
	}
	ctx := strings.TrimRight(string(cur), "\x00\n")
	if strings.Contains(ctx, ":install_t:") {
		return nil
	}
	return fmt.Errorf(i18n.T("this process runs in the SELinux context %s, which cannot enter install_t (the installer's domain); start it as a service: systemd-run --pty --wait --collect basalt-installer %s"), ctx, strings.Join(os.Args[1:], " "))
}
