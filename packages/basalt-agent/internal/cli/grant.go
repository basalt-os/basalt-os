package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/egress"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/session"
)

// GrantHelper runs as root through pkexec (polkit action
// org.basalt-os.agent.grant, auth_admin_keep).
const GrantHelper = "/usr/libexec/basalt-agent/basalt-agent-grant"

func cmdGrant(args []string) error {
	if len(args) != 3 || (args[1] != "host" && args[1] != "path") || !session.ValidID(args[0]) {
		return errors.New("usage: basalt-agent grant SESSION host NAME[:PORTS] | path DIR")
	}
	// The session must be the caller's own and running.
	d, err := session.UserDirs()
	if err != nil {
		return err
	}
	found := false
	for _, s := range d.Running() {
		found = found || s.ID == args[0]
	}
	if !found {
		return fmt.Errorf("no running session %s (basalt-agent sessions)", args[0])
	}
	cmd := exec.Command("pkexec", GrantHelper, args[0], args[1], args[2])
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("grant not applied: %w", err)
	}
	return nil
}

// GrantMain is the root helper: it checks who asked (PKEXEC_UID), sends
// the grant to that user's session and logs it to the system journal.
func GrantMain(args []string) int {
	fail := func(format string, a ...any) int {
		msg := fmt.Sprintf(format, a...)
		_ = exec.Command("logger", "-p", "authpriv.warning", "-t", "basalt-agent-grant", "refused: "+msg).Run()
		fmt.Fprintln(os.Stderr, "basalt-agent-grant: "+msg)
		return 1
	}
	if os.Geteuid() != 0 {
		return fail("must run as root through pkexec")
	}
	uid, err := strconv.Atoi(os.Getenv("PKEXEC_UID"))
	if err != nil || uid <= 0 {
		return fail("PKEXEC_UID missing: run it as basalt-agent grant")
	}
	if len(args) > 0 && args[0] == "profile" {
		if err := grantProfile(uid, args[1:]); err != nil {
			return fail("uid %d profile change: %v", uid, err)
		}
		return 0
	}
	if len(args) != 3 || !session.ValidID(args[0]) || (args[1] != "host" && args[1] != "path") || len(args[2]) > 4096 {
		return fail("bad arguments")
	}
	sock := fmt.Sprintf("/run/user/%d/basalt-agent/%s/control.sock", uid, args[0])
	st, err := os.Lstat(sock)
	if err != nil || st.Mode()&os.ModeSocket == 0 {
		return fail("no session %s for uid %d", args[0], uid)
	}
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return fail("session %s: %v", args[0], err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	b, _ := json.Marshal(grantRequest{Kind: args[1], Value: args[2], ByUID: uid})
	if _, err := c.Write(append(b, '\n')); err != nil {
		return fail("session %s: %v", args[0], err)
	}
	var rep grantReply
	if err := json.NewDecoder(c).Decode(&rep); err != nil {
		return fail("session %s: no reply", args[0])
	}
	msg := fmt.Sprintf("uid %d session %s %s %q", uid, args[0], args[1], args[2])
	if !rep.OK {
		return fail("%s: %s", msg, rep.Error)
	}
	// The kernel filter: only root may widen a session's allowlist in
	// basalt-resolver, so the grant goes there from this helper.
	if args[1] == "host" && egress.Available() {
		if _, err := egress.Do(egress.Request{Op: "allow", Session: args[0], Entry: args[2], ByUID: uid}); err != nil {
			return fail("%s: the proxy allows it but the kernel filter refused: %v", msg, err)
		}
	}
	_ = exec.Command("logger", "-p", "authpriv.notice", "-t", "basalt-agent-grant", "granted: "+msg).Run()
	fmt.Printf("granted: %s %s to session %s\n", args[1], args[2], args[0])
	return 0
}
