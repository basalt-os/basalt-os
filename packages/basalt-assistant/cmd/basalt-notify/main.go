// Command basalt-notify delivers the assistant's findings to desktop
// sessions (freedesktop notifications) and to an optional signed webhook.
// It follows the journal records of basalt-assistantd; the journal itself
// always has every finding, so on a server with no desktop and no webhook
// this program has nothing to do and exits at once.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/config"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/notify"
)

var version = "dev"

// stateDir keeps the journal cursor of the last delivered finding.
const stateDir = "/var/lib/basalt-notify"

func main() {
	path := config.DefaultPath
	if len(os.Args) > 2 && os.Args[1] == "--config" {
		path = os.Args[2]
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "basalt-notify:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg); err != nil {
		if errors.Is(err, notify.ErrNothingToDo) {
			fmt.Println("basalt-notify:", err)
			return
		}
		fmt.Fprintln(os.Stderr, "basalt-notify:", err)
		os.Exit(1)
	}
}

type deliverer struct {
	desktop bool
	hook    *notify.Webhook
	events  string // notify or all
}

func run(ctx context.Context, cfg config.Config) error {
	d := deliverer{events: cfg.WebhookEvents}
	switch cfg.NotifyDesktop {
	case "yes":
		d.desktop = true
	case "auto":
		out, _ := exec.CommandContext(ctx, "systemctl", "get-default").Output()
		d.desktop = strings.TrimSpace(string(out)) == "graphical.target"
	}
	if cfg.WebhookURL != "" {
		if err := notify.CheckURL(cfg.WebhookURL); err != nil {
			return err
		}
		secret, err := notify.ReadSecret(cfg.WebhookSecretFile)
		if err != nil {
			return fmt.Errorf("webhook key: %w", err)
		}
		d.hook = &notify.Webhook{URL: cfg.WebhookURL, Secret: secret, Version: version}
		d.hook.Client = &http.Client{Timeout: cfg.WebhookTimeout}
	}
	if !d.desktop && d.hook == nil {
		return notify.ErrNothingToDo
	}
	fmt.Printf("basalt-notify %s: desktop %v, webhook %v\n", version, d.desktop, d.hook != nil)
	return follow(ctx, d)
}

// follow reads the daemon's findings from the journal, from the saved
// cursor on (first start: new findings only), and delivers each one.
func follow(ctx context.Context, d deliverer) error {
	cursorFile := filepath.Join(stateDir, "cursor")
	args := []string{"--follow", "--output=json", "_SYSTEMD_UNIT=" + notify.DaemonUnit, "BASALT_AUDIT_TYPE=finding"}
	if c, err := os.ReadFile(cursorFile); err == nil && len(strings.TrimSpace(string(c))) > 0 {
		args = append([]string{"--after-cursor=" + strings.TrimSpace(string(c))}, args...)
	} else {
		// First start: from now on, so enabling a webhook on a machine that
		// has run for a while does not replay old findings.
		args = append([]string{"--lines=0"}, args...)
	}
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		ev, ok, err := notify.ParseEntry(sc.Bytes())
		if err != nil {
			fmt.Fprintln(os.Stderr, "basalt-notify:", err)
		}
		if ok {
			d.deliver(ctx, ev)
		}
		if ev.Cursor != "" {
			saveCursor(cursorFile, ev.Cursor)
		}
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return nil
	}
	if err == nil {
		err = errors.New("journalctl stopped")
	}
	return err
}

func (d deliverer) deliver(ctx context.Context, ev notify.Event) {
	if d.desktop && ev.Notify {
		n, err := desktopNotify(ctx, ev)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "basalt-notify: desktop notification for %s: %v\n", ev.Proposal, err)
		case n > 0:
			fmt.Printf("basalt-notify: %s shown in %d desktop session(s)\n", ev.Proposal, n)
		}
	}
	if d.hook != nil && (ev.Notify || d.events == "all") {
		if err := d.hook.Send(ctx, ev); err != nil {
			fmt.Fprintf(os.Stderr, "basalt-notify: webhook for %s: %v\n", ev.Proposal, err)
		} else {
			fmt.Printf("basalt-notify: %s sent to the webhook\n", ev.Proposal)
		}
	}
}

// desktopNotify shows the event in every local graphical session; with no
// such session it does nothing.
func desktopNotify(ctx context.Context, ev notify.Event) (int, error) {
	out, err := exec.CommandContext(ctx, "loginctl", "list-sessions", "--no-legend").Output()
	if err != nil {
		return 0, err
	}
	props := map[string]map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		p, err := exec.CommandContext(ctx, "loginctl", "show-session", f[0],
			"-p", "Type", "-p", "Class", "-p", "State", "-p", "Remote", "-p", "User", "-p", "Name").Output()
		if err == nil {
			props[f[0]] = notify.ParseProperties(string(p))
		}
	}
	n := 0
	var errs []error
	for _, s := range notify.GraphicalSessions(props) {
		if err := busctlAs(ctx, s.UID, notify.BusctlArgs(ev)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.User, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// busctlAs runs busctl as the session's user against that user's bus.
func busctlAs(ctx context.Context, uid int, args []string) error {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	run := fmt.Sprintf("/run/user/%d", uid)
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "busctl", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + u.HomeDir, "XDG_RUNTIME_DIR=" + run,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + run + "/bus"}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func saveCursor(path, cursor string) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(cursor+"\n"), 0o600); err == nil {
		_ = os.Rename(tmp, path)
	}
}
