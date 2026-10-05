// Command basalt-installer installs Basalt OS on one disk from a
// declarative plan. One engine, three ways in:
//
//	basalt-installer tui                     text wizard (console, serial)
//	basalt-installer serve                   local JSON API for the GUI
//	basalt-installer install --plan FILE     non-interactive, from a plan file
//
// and the tools around them: plan template, validate and preview (the exact
// step list, nothing executed), facts, log verify.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/api"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/auditlog"
	// Before Bubble Tea's initialization: no terminal queries on a serial
	// console (see the package).
	_ "github.com/basalt-os/basalt-os/packages/basalt-installer/internal/earlyterm"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/engine"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/i18n"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/session"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/steps"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/tui"
)

// Version is set at build time (-ldflags "-X main.Version=...").
var Version = "dev"

const usage = `basalt-installer: install Basalt OS on one disk from a declarative plan.

Usage:
  basalt-installer tui [--demo] [--attach]           text wizard (console or serial); --attach follows
                                                     the installation of a running serve
  basalt-installer serve [--socket P] [--allow-user U] [--demo]
                                                     local JSON API for the GUI
  basalt-installer install --plan FILE --confirm DISK [--recovery-key-out FILE] [--no-finish]
                                                     non-interactive install
  basalt-installer plan template [--disk /dev/vda]   a commented plan to start from (YAML)
  basalt-installer plan validate FILE [--offline]    check a plan (against this machine unless --offline)
  basalt-installer plan preview FILE [--facts F]     the exact steps, nothing executed
  basalt-installer client OP [JSON] [--socket P]     one call to the API (scripts, debugging)
  basalt-installer facts                             what the installer sees (JSON)
  basalt-installer log verify FILE                   check an install log's hash chain
  basalt-installer version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "tui", "serve", "install":
		if err := checkDomain(); err != nil {
			fmt.Fprintln(os.Stderr, "basalt-installer:", err)
			os.Exit(1)
		}
		privateMounts()
	}
	var err error
	switch os.Args[1] {
	case "tui":
		err = cmdTUI(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "install":
		err = cmdInstall(os.Args[2:])
	case "plan":
		err = cmdPlan(os.Args[2:])
	case "facts":
		err = cmdFacts()
	case "client":
		err = cmdClient(os.Args[2:])
	case "log":
		err = cmdLog(os.Args[2:])
	case "version", "--version":
		fmt.Println("basalt-installer", Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "basalt-installer:", err)
		os.Exit(1)
	}
}

func finisher(ctx context.Context, action string) error {
	switch action {
	case "reboot", "poweroff":
		return exec.CommandContext(ctx, "systemctl", action).Run()
	}
	return nil
}

func newSession(demo bool) (*session.Session, func(), error) {
	opt := session.Options{
		Prober:      probe.Prober{},
		Steps:       steps.Options{InstallerVersion: Version, MediaDir: steps.DefaultMediaDir},
		ZoneinfoDir: "/usr/share/zoneinfo",
		Finisher:    finisher,
	}
	cleanup := func() {}
	if demo {
		dir, err := os.MkdirTemp("", "basalt-installer-demo-")
		if err != nil {
			return nil, nil, err
		}
		cleanup = func() { os.RemoveAll(dir) }
		opt.Prober = demoProber()
		opt.Steps.Root, opt.Steps.Work, opt.Steps.MediaDir = filepath.Join(dir, "sysroot"), filepath.Join(dir, "work"), filepath.Join(dir, "media")
		opt.Engine = engine.Options{Runner: &demoRunner{root: dir, fake: engine.FakeRunner{Delay: 120 * time.Millisecond,
			Captured: "fjkldhgr-uvbntckr-hnilbvcj-ldiekgnb-rtfhduje-cvjgnbtr-ikluhdcb-nvrkrfdh",
			Output:   map[string][]string{"dnf --assumeyes": demoDNF()}}},
			LogDir: filepath.Join(dir, "log"), LockPath: filepath.Join(dir, "lock")}
		opt.Finisher = func(context.Context, string) error { return nil }
		// The demo's USB stick is a directory of the demo.
		opt.KeyWriter = func(ctx context.Context, m probe.KeyMedium, name string, content []byte) (string, error) {
			m.Mountpoint = filepath.Join(dir, "usb-"+filepath.Base(m.Path))
			if err := os.MkdirAll(m.Mountpoint, 0o700); err != nil {
				return "", err
			}
			return session.MountAndWrite(ctx, m, name, content)
		}
		// The demo's existing /home opens with any passphrase.
		opt.HomeInspector = func(_ context.Context, part probe.Partition, _ string) (session.HomeInspection, error) {
			return session.HomeInspection{Device: part.Path, LUKS: true, UUID: part.UUID, FSType: "ext4",
				Owners: []session.HomeOwner{{Name: "ana", UID: 1000, GID: 1000}}}, nil
		}
		opt.Cmdline = filepath.Join(dir, "cmdline")
		_ = os.WriteFile(opt.Cmdline, []byte("basalt.inst.repo=media basalt.inst.hostname=basalt-demo"), 0o644)
	}
	return session.New(opt), cleanup, nil
}

func cmdTUI(args []string) error {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	demo := fs.Bool("demo", false, "fake machine and fake commands: nothing is written")
	ascii := fs.Bool("ascii", false, "ASCII borders and bars (default on serial lines)")
	attach := fs.Bool("attach", false, "follow the installation that `basalt-installer serve` runs")
	sock := fs.String("socket", "/run/basalt-installer-api/api.sock", "Unix socket of serve (with --attach)")
	_ = fs.Parse(args)
	if !*demo && os.Geteuid() != 0 {
		return errors.New("run as root (or try --demo)")
	}
	ss, cleanup, err := newSession(*demo)
	if err != nil {
		return err
	}
	defer cleanup()
	return tui.Run(tui.Options{Session: ss, Version: Version, ASCII: *ascii || tui.IsSerial(), Attach: *attach, Socket: *sock})
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	sock := fs.String("socket", "/run/basalt-installer-api/api.sock", "Unix socket path")
	allow := fs.String("allow-user", "", "user (besides root) allowed on the socket: the GUI session user")
	demo := fs.Bool("demo", false, "fake machine and fake commands: nothing is written")
	_ = fs.Parse(args)
	if !*demo && os.Geteuid() != 0 {
		return errors.New("run as root (or try --demo)")
	}
	ss, cleanup, err := newSession(*demo)
	if err != nil {
		return err
	}
	defer cleanup()
	srv := &api.Server{Session: ss}
	gid := -1
	if *demo {
		srv.AllowUIDs = append(srv.AllowUIDs, os.Getuid())
	}
	if *allow != "" {
		u, err := user.Lookup(*allow)
		if err != nil {
			return err
		}
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ = strconv.Atoi(u.Gid)
		srv.AllowUIDs = append(srv.AllowUIDs, uid)
	}
	if err := os.MkdirAll(filepath.Dir(*sock), 0o755); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "basalt-installer %s: API on %s\n", Version, *sock)
	if ss.UnattendedRequested() {
		// basalt.inst.plan with basalt.inst.confirm on the boot line: this
		// service runs the installation; the text installers follow it.
		go func() {
			ss.RunUnattended(ctx, 2*time.Minute)
			st := ss.Status().Unattended
			fmt.Fprintf(os.Stderr, "unattended installation: %s %s\n", st.State, st.Error)
		}()
	}
	return srv.Listen(ctx, *sock, gid)
}

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	planFile := fs.String("plan", "", "plan file (YAML or JSON)")
	confirm := fs.String("confirm", "", "the target disk's name (vda, nvme0n1): confirms that it is erased")
	keyOut := fs.String("recovery-key-out", "", "write the recovery key to this file (mode 0400); \"-\" prints it")
	noFinish := fs.Bool("no-finish", false, "do not reboot or power off at the end")
	_ = fs.Parse(args)
	if *planFile == "" {
		return errors.New("--plan is required")
	}
	if os.Geteuid() != 0 {
		return errors.New("run as root")
	}
	p, err := plan.Load(*planFile)
	if err != nil {
		return err
	}
	if p.Encrypted() && *keyOut == "" && p.Encryption.RecoveryKeyMedia == "" {
		return errors.New(i18n.T("the disk is encrypted: give --recovery-key-out FILE (or - to print it), or name a USB stick in the plan (encryption.recovery_key_media), so the recovery key is not lost"))
	}
	ss, _, err := newSession(false)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pv, err := ss.MakePreview(ctx, p)
	for _, i := range pv.Issues {
		fmt.Fprintln(os.Stderr, i.String())
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "plan %s: %d steps (preview: basalt-installer plan preview %s)\n", pv.Token, len(pv.Steps), *planFile)
	events, unsub := ss.Subscribe()
	defer unsub()
	var keyWhere []string
	if err := ss.Install(ctx, pv.Token, *confirm); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = ss.Cancel()
	}()
	for e := range events {
		switch e.Type {
		case engine.EvStarted:
			fmt.Fprintf(os.Stderr, "log: %s\n", e.LogPath)
		case engine.EvStepStart:
			fmt.Fprintf(os.Stderr, "[%d/%d] %s\n  $ %s\n", e.Index, e.Total, e.Title, e.Command)
		case engine.EvOutput:
			fmt.Fprintf(os.Stderr, "    %s\n", e.Text)
		case engine.EvWarning:
			fmt.Fprintf(os.Stderr, "warning: %s\n", e.Text)
		case engine.EvRollback:
			fmt.Fprintf(os.Stderr, "rollback: %s%s\n", e.Command, e.Text)
		case session.EvKeySaved:
			keyWhere = append(keyWhere, e.Text)
			fmt.Fprintf(os.Stderr, i18n.T("recovery key written to %s")+"\n", e.Text)
		case engine.EvSecret:
			if e.Kind == engine.SecretRecKey {
				if *keyOut == "" {
					// The plan's USB stick gets it when the installation is done.
					continue
				}
				if err := writeKey(*keyOut, e.Secret); err != nil {
					fmt.Fprintf(os.Stderr, "error: could not store the recovery key: %v\n", err)
					_ = ss.Cancel()
					continue
				}
				first, _, _ := strings.Cut(e.Secret, "-")
				if err := ss.AckRecoveryKey(first); err != nil {
					return err
				}
				if *keyOut != "-" {
					keyWhere = append(keyWhere, *keyOut)
					fmt.Fprintf(os.Stderr, i18n.T("recovery key written to %s")+"\n", *keyOut)
				} else {
					keyWhere = append(keyWhere, i18n.T("printed on standard output"))
				}
			}
		case engine.EvDone:
			if !e.OK {
				return errors.New(e.Error)
			}
			st := ss.Status()
			fmt.Fprint(os.Stderr, installSummary(pv, st, keyWhere, e.LogPath))
			if k := ss.RecoveryKey(); k != "" {
				// The plan's USB stick could not take the key: rather than
				// lose it, print it once.
				fmt.Fprintln(os.Stderr, i18n.T("The recovery key could not be written to the USB stick the plan names. Store this key now:"))
				fmt.Printf("RECOVERY KEY: %s\n", k)
				first, _, _ := strings.Cut(k, "-")
				_ = ss.AckRecoveryKey(first)
			}
			if *noFinish {
				return nil
			}
			return ss.Finish(ctx, "")
		}
	}
	return errors.New("the installation ended without a result")
}

func writeKey(path, key string) error {
	if path == "-" {
		fmt.Printf("RECOVERY KEY: %s\n", key)
		return nil
	}
	return os.WriteFile(path, []byte(key+"\n"), 0o400)
}

// parseInterspersed parses flags given before or after the positional
// arguments (`plan validate FILE --offline`, as the usage shows them); the
// standard flag package stops at the first positional argument.
func parseInterspersed(fs *flag.FlagSet, args []string) {
	var pos []string
	for {
		_ = fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	_ = fs.Parse(pos)
}

func cmdPlan(args []string) error {
	if len(args) < 1 {
		return errors.New("plan template|validate|preview")
	}
	switch args[0] {
	case "template":
		fs := flag.NewFlagSet("template", flag.ExitOnError)
		disk := fs.String("disk", "/dev/vda", "target disk")
		_ = fs.Parse(args[1:])
		fmt.Print(template(*disk))
		return nil
	case "validate":
		fs := flag.NewFlagSet("validate", flag.ExitOnError)
		offline := fs.Bool("offline", false, "do not check against this machine")
		parseInterspersed(fs, args[1:])
		if fs.NArg() != 1 {
			return errors.New("plan validate FILE")
		}
		p, err := plan.Load(fs.Arg(0))
		if err != nil {
			return err
		}
		var f *probe.Facts
		if !*offline {
			facts, err := probe.Prober{}.Probe(context.Background())
			if err != nil {
				return err
			}
			f = &facts
		}
		is := plan.Validate(p, f, "/usr/share/zoneinfo")
		for _, i := range is {
			fmt.Println(i.String())
		}
		if err := is.Err(); err != nil {
			return errors.New("the plan is not valid")
		}
		fmt.Println("plan ok")
		return nil
	case "preview":
		fs := flag.NewFlagSet("preview", flag.ExitOnError)
		factsFile := fs.String("facts", "", "machine facts (JSON from `basalt-installer facts`) instead of probing this machine")
		asJSON := fs.Bool("json", false, "the steps as JSON")
		parseInterspersed(fs, args[1:])
		if fs.NArg() != 1 {
			return errors.New("plan preview FILE")
		}
		p, err := plan.Load(fs.Arg(0))
		if err != nil {
			return err
		}
		var f probe.Facts
		if *factsFile != "" {
			data, err := os.ReadFile(*factsFile)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &f); err != nil {
				return err
			}
		} else if f, err = (probe.Prober{}).Probe(context.Background()); err != nil {
			return err
		}
		is := plan.Validate(p, &f, "")
		for _, i := range is {
			fmt.Fprintln(os.Stderr, i.String())
		}
		r, err := steps.Resolve(p, f, steps.Options{InstallerVersion: Version})
		if err != nil {
			return err
		}
		list, err := steps.Generate(r)
		if err != nil {
			return err
		}
		if *asJSON {
			b, _ := json.MarshalIndent(list, "", "  ")
			fmt.Println(string(b))
			return nil
		}
		fmt.Printf("# %s\n# profile %s (%s); identifiers are generated again for the real install\n\n", p.Summary(), r.Profile, r.ProfileReason)
		fmt.Print(steps.Preview(list))
		return nil
	}
	return fmt.Errorf("unknown plan command %q", args[0])
}

func cmdFacts() error {
	f, err := probe.Prober{}.Probe(context.Background())
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	fmt.Println(string(b))
	return nil
}

func cmdLog(args []string) error {
	if len(args) != 2 || args[0] != "verify" {
		return errors.New("log verify FILE")
	}
	data, err := os.ReadFile(args[1])
	if err != nil {
		return err
	}
	n, err := auditlog.Verify(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%s: %w", args[1], err)
	}
	fmt.Printf("%s: %d records, chain intact\n", args[1], n)
	return nil
}

func template(disk string) string {
	// Pad the disk so its comment lines up with the others (column 26).
	diskField := disk + strings.Repeat(" ", max(18-len(disk), 2))
	return `# Basalt OS install plan (basalt-install-plan/v1).
# Every field below shows its default; delete what you do not change.
# Check it:    basalt-installer plan validate plan.yaml
# See it:      basalt-installer plan preview plan.yaml   (the exact commands)
# Install it:  basalt-installer install --plan plan.yaml --confirm ` + strings.TrimPrefix(disk, "/dev/") + ` --recovery-key-out /path/key.txt
# Unattended from the boot menu: basalt.inst.plan=URL basalt.inst.confirm=` + strings.TrimPrefix(disk, "/dev/") + `
apiVersion: basalt-install-plan/v1
edition: server
target:
  disk: ` + diskField + `# the whole disk is erased
  wipe: true              # must be true
layout:
  mode: automatic         # automatic | manual (manual: esp_mib, boot_mib, root_gib, subvolumes)
encryption:
  enabled: true
  unlock: tpm2            # tpm2 | tang | tpm2+tang | recovery-only
  # tang: {url: "http://tang.example:7500", thumbprint: "..."}
  # recovery_key_media: KEYS    # label of a USB stick that gets a copy of the recovery key
  # store_recovery_key: false   # true leaves it in /root on the installed disk
profile: auto             # auto (minimal on a VM, standard on bare metal) | minimal | standard
hostname: basalt
timezone: Etc/UTC
locale: en_US.UTF-8
keymap: us
accounts:
  root:
    ssh_keys: []          # ["ssh-ed25519 AAAA... you@host"]
  # user:
  #   name: admin
  #   password_hash: "$y$..."    # mkpasswd -m yescrypt
  #   ssh_keys: []
ssh:
  password_auth: false    # Basalt OS default: public keys only
network:
  mode: dhcp              # dhcp | static (interface, address: 192.0.2.10/24, gateway, dns)
repos:
  basalt:
    url: media            # media (the installer image) or http(s)://... (tree <url>/<release>/<arch>/)
    # installed_url: https://...   # what the installed system uses
  tools: true             # basalt-tools: metadata only, nothing installed unless chosen
  third_party:
    tui_tools: true       # tui-tools, key fingerprint pinned
assistant: true
finish: reboot            # reboot | poweroff | none
`
}

// cmdClient sends one request to a running `basalt-installer serve` and
// prints the answer: `client status`, `client recovery_key`,
// `client ack_recovery_key '{"proof":"..."}'`.
func cmdClient(args []string) error {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	sock := fs.String("socket", "/run/basalt-installer-api/api.sock", "Unix socket path")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return errors.New("client OP [JSON]")
	}
	req := map[string]any{"id": 1, "op": fs.Arg(0)}
	if fs.NArg() > 1 {
		req["args"] = json.RawMessage(fs.Arg(1))
	}
	c, err := net.Dial("unix", *sock)
	if err != nil {
		return err
	}
	defer c.Close()
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return err
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		var resp api.Response
		if json.Unmarshal(sc.Bytes(), &resp) != nil || resp.ID != 1 {
			continue
		}
		out, _ := json.MarshalIndent(resp.Result, "", "  ")
		if !resp.OK {
			return errors.New(resp.Error)
		}
		fmt.Println(string(out))
		return nil
	}
	return errors.New("no answer")
}

// privateMounts re-executes the installer in its own mount namespace
// (propagation from the host only, never back), so the target's mounts,
// including the /dev bind mount, are invisible to every other process and
// cannot be pinned by the mount namespaces of running services: without
// it, a service's copy of /mnt/sysroot/dev keeps the encrypted volume busy
// after the install. Demo mode and non-root runs need no namespace.
func privateMounts() {
	if os.Geteuid() != 0 || os.Getenv("BASALT_INSTALLER_NS") == "1" {
		return
	}
	for _, a := range os.Args[2:] {
		if a == "--demo" || a == "-demo" {
			return
		}
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		fmt.Fprintln(os.Stderr, "basalt-installer: unshare not found, running in the host mount namespace")
		return
	}
	env := append(os.Environ(), "BASALT_INSTALLER_NS=1")
	argv := append([]string{unshare, "--mount", "--propagation", "slave", "--", self}, os.Args[1:]...)
	err = syscall.Exec(unshare, argv, env)
	fmt.Fprintln(os.Stderr, "basalt-installer: could not enter a private mount namespace:", err)
	os.Exit(1)
}
