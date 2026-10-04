package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/feedback"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/i18n"
)

// errNotSent ends `basalt feedback` without sending; its message was
// already shown to the person.
var errNotSent = errors.New("nothing was sent")

// feedbackIO is what the feedback flow talks to (replaced in tests).
type feedbackIO struct {
	in          *bufio.Reader
	out         io.Writer
	interactive bool // stdin and stdout are a terminal
	src         feedback.Sources
	scrubber    *feedback.Scrubber
	client      *http.Client
	edit        func(path string) error // opens the person's editor
	arch        string
	findings    time.Duration // how far back findings are read
}

// feedback is `basalt feedback`: an opt-in report to the Basalt OS project.
//
// It runs in the person's own session, never in the confined daemon (which
// has no network). A confined agent session cannot send one either: the
// feedback service is on no agent allowlist (docs/network.md), so its
// connection is dropped. The future voice action "send feedback" of the
// Basalt shell uses the non-interactive form: --preview prints the payload
// and its confirmation code for the shell's confirmation sheet, and only
// --yes --confirm CODE with that code sends it (source "voice").
func (a *app) feedback(ctx context.Context) error {
	interactive := false
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 && a.tty {
		interactive = true
	}
	src := feedback.RealSources()
	hosts, users := feedback.LocalNames(src)
	fio := feedbackIO{
		in: bufio.NewReader(os.Stdin), out: a.out, interactive: interactive, src: src,
		scrubber: feedback.NewScrubber(hosts, users),
		client:   &http.Client{Timeout: a.cfg.FeedbackTimeout},
		edit:     runEditor, arch: runtime.GOARCH, findings: 7 * 24 * time.Hour,
	}
	return runFeedback(ctx, a.o, a.cfg.FeedbackEndpoint, a.version, fio)
}

func runFeedback(ctx context.Context, o opts, endpoint, version string, f feedbackIO) error {
	say := func(s string) { fmt.Fprintln(f.out, s) }
	if endpoint == "" {
		say(i18n.T("Sending feedback is turned off on this system ([feedback] endpoint is empty in /etc/basalt/assistant.conf). You can write to feedback@basalt-os.org instead."))
		return errNotSent
	}
	source := o.source
	if source == "" {
		source = "cli"
	}
	if source != "cli" && source != "voice" {
		return fmt.Errorf("--source %q (cli or voice)", source)
	}
	if o.kind != "" && !validKind(o.kind) {
		return fmt.Errorf("--kind %q (bug, idea or other)", o.kind)
	}
	parts, err := feedback.ParseParts(o.include)
	if err != nil {
		return err
	}
	r := &feedback.Report{
		Kind: o.kind, Message: strings.TrimSpace(strings.Join(o.args[1:], " ")), Email: strings.TrimSpace(o.email),
		Source: source, Client: "basalt-assistant/" + version, Lang: i18n.Lang(),
	}

	if !f.interactive {
		if r.Kind == "" || r.Message == "" {
			say(i18n.T("Without a terminal, give the message and --kind, choose the parts with --include, look at the report with --preview and send it with --yes --confirm CODE."))
			return errNotSent
		}
	} else {
		say(i18n.T("Send feedback to the Basalt OS project."))
		say(i18n.T("Nothing leaves this computer until you have seen exactly what will be sent and said yes."))
		say("")
		if r.Kind == "" {
			if r.Kind, err = askKind(f); err != nil {
				return err
			}
		}
		if r.Message == "" {
			say(i18n.T("Write your message. End it with an empty line."))
			r.Message = readParagraph(f.in)
		}
		if r.Email == "" && o.email == "" {
			fmt.Fprint(f.out, i18n.T("E-mail address for a reply (optional, press Enter to skip): "))
			r.Email = readLine(f.in)
		}
	}

	// The optional parts: given with --include, or offered one by one.
	var sys feedback.System
	offer := !o.includeSet && f.interactive
	candidates := parts
	if offer {
		candidates = feedback.Parts
	}
	changed := 0
	for _, p := range candidates {
		v, ok := collectPart(ctx, f, p, &sys)
		if !ok {
			continue
		}
		// Scrub the part before it is shown: what the person sees is what
		// would be sent.
		n := f.scrubber.ScrubReport(&feedback.Report{System: partOnly(&sys, p)})
		if offer {
			say("")
			say(partLabel(p))
			printIndentedJSON(f.out, v)
			fmt.Fprint(f.out, i18n.T("Include this? [y/N] "))
			if !isYes(readLine(f.in)) {
				dropPart(&sys, p)
				continue
			}
		}
		changed += n
	}
	if sys.OS != nil || sys.Packages != nil || sys.Hardware != nil || sys.Findings != nil {
		r.System = &sys
	}
	if m := f.scrubber.Scrub(r.Message); m != r.Message {
		r.Message, changed = m, changed+1
	}

	for {
		if err := feedback.Validate(r); err != nil {
			say(refusalText(err))
			return errNotSent
		}
		payload, err := feedback.Payload(r)
		if err != nil {
			return err
		}
		code := feedback.Code(payload)

		if o.preview {
			if o.json {
				return printJSONTo(f.out, map[string]any{"endpoint": endpoint, "report": r, "code": code})
			}
			showReport(f, endpoint, r, changed)
			fmt.Fprintf(f.out, i18n.T("To send exactly this report, run the same command with --yes --confirm %s instead of --preview.")+"\n", code)
			return nil
		}
		if o.yes {
			if o.confirm != code {
				say(i18n.T("The confirmation code does not match this report. Nothing was sent. Run the command with --preview to see the report and its code."))
				return errNotSent
			}
			return send(ctx, f, endpoint, payload, o.json)
		}
		if !f.interactive {
			say(i18n.T("Without a terminal, give the message and --kind, choose the parts with --include, look at the report with --preview and send it with --yes --confirm CODE."))
			return errNotSent
		}

		showReport(f, endpoint, r, changed)
		fmt.Fprint(f.out, i18n.T("Send it? Type yes to send, edit to change it, or press Enter to cancel: "))
		switch ans := strings.ToLower(readLine(f.in)); {
		case isYesWord(ans):
			return send(ctx, f, endpoint, payload, o.json)
		case ans == "edit" || ans == strings.ToLower(i18n.T("edit")):
			if err := editReport(f, r); err != nil {
				fmt.Fprintf(f.out, i18n.T("The report could not be changed: %v")+"\n", err)
			}
			changed = 0
		default:
			say(i18n.T("Nothing was sent."))
			return errNotSent
		}
	}
}

func validKind(k string) bool {
	for _, v := range feedback.Kinds {
		if k == v {
			return true
		}
	}
	return false
}

func askKind(f feedbackIO) (string, error) {
	for i := 0; i < 3; i++ {
		fmt.Fprintln(f.out, i18n.T("What is it about?"))
		fmt.Fprintln(f.out, "  1  "+i18n.T("something is broken"))
		fmt.Fprintln(f.out, "  2  "+i18n.T("an idea"))
		fmt.Fprintln(f.out, "  3  "+i18n.T("something else"))
		fmt.Fprint(f.out, i18n.T("Choose 1, 2 or 3: "))
		switch readLine(f.in) {
		case "1", "bug":
			return "bug", nil
		case "2", "idea":
			return "idea", nil
		case "3", "other":
			return "other", nil
		}
	}
	fmt.Fprintln(f.out, i18n.T("Nothing was sent."))
	return "", errNotSent
}

func partLabel(p feedback.Part) string {
	switch p {
	case feedback.PartOS:
		return i18n.T("The Basalt OS version and the kernel release:")
	case feedback.PartPackages:
		return i18n.T("The versions of the installed basalt-* packages:")
	case feedback.PartHardware:
		return i18n.T("A hardware summary (processor, memory, firmware, virtual machine or not, TPM), nothing that identifies the machine:")
	}
	return i18n.T("The assistant's findings of the last 7 days:")
}

// collectPart reads one part into sys and returns it for display; ok is
// false when there is nothing to offer (the reason was printed).
func collectPart(ctx context.Context, f feedbackIO, p feedback.Part, sys *feedback.System) (any, bool) {
	switch p {
	case feedback.PartOS:
		o, err := feedback.CollectOS(f.src)
		if err != nil {
			fmt.Fprintf(f.out, i18n.T("The system version could not be read: %v")+"\n", err)
			return nil, false
		}
		sys.OS = o
		return o, true
	case feedback.PartPackages:
		pkgs, err := feedback.CollectPackages(ctx, f.src)
		if err != nil || len(pkgs) == 0 {
			fmt.Fprintln(f.out, i18n.T("No basalt-* packages were found."))
			return nil, false
		}
		sys.Packages = pkgs
		return pkgs, true
	case feedback.PartHardware:
		h := feedback.CollectHardware(ctx, f.src, f.arch)
		sys.Hardware = h
		return h, true
	case feedback.PartFindings:
		fs, err := feedback.CollectFindings(ctx, f.src, f.findings)
		if err != nil || len(fs) == 0 {
			if os.Geteuid() != 0 {
				fmt.Fprintln(f.out, i18n.T("No findings of the assistant are readable here. They are in the system journal; run sudo basalt feedback to include them."))
			} else {
				fmt.Fprintln(f.out, i18n.T("The assistant has no findings from the last 7 days."))
			}
			return nil, false
		}
		sys.Findings = fs
		return fs, true
	}
	return nil, false
}

// partOnly is a System holding only part p of sys (sharing its values, so
// scrubbing it scrubs sys).
func partOnly(sys *feedback.System, p feedback.Part) *feedback.System {
	switch p {
	case feedback.PartOS:
		return &feedback.System{OS: sys.OS}
	case feedback.PartPackages:
		return &feedback.System{Packages: sys.Packages}
	case feedback.PartHardware:
		return &feedback.System{Hardware: sys.Hardware}
	}
	return &feedback.System{Findings: sys.Findings}
}

func dropPart(sys *feedback.System, p feedback.Part) {
	switch p {
	case feedback.PartOS:
		sys.OS = nil
	case feedback.PartPackages:
		sys.Packages = nil
	case feedback.PartHardware:
		sys.Hardware = nil
	case feedback.PartFindings:
		sys.Findings = nil
	}
}

func showReport(f feedbackIO, endpoint string, r *feedback.Report, changed int) {
	fmt.Fprintln(f.out)
	fmt.Fprintf(f.out, i18n.T("This is exactly what will be sent to %s:")+"\n", endpoint)
	printIndentedJSON(f.out, r)
	if changed > 0 {
		fmt.Fprintf(f.out, i18n.N("%d value was changed to hide personal data (shown as [redacted]).",
			"%d values were changed to hide personal data (shown as [redacted]).", changed)+"\n", changed)
	}
	if r.Email != "" {
		fmt.Fprintln(f.out, i18n.T("Your e-mail address is sent as you typed it, so the project can reply."))
	}
	fmt.Fprintln(f.out, i18n.T("The service keeps the report and the time it arrived; it does not keep your IP address."))
}

func send(ctx context.Context, f feedbackIO, endpoint string, payload []byte, asJSON bool) error {
	id, err := feedback.Send(ctx, f.client, endpoint, payload)
	if err != nil {
		var re *feedback.Error
		if errors.As(err, &re) {
			fmt.Fprintln(f.out, refusalText(err))
		} else {
			fmt.Fprintf(f.out, i18n.T("The feedback service could not be reached: %v. Nothing was sent; you can write to feedback@basalt-os.org instead.")+"\n", err)
		}
		return errNotSent
	}
	if asJSON {
		return printJSONTo(f.out, map[string]any{"ok": true, "id": id})
	}
	fmt.Fprintf(f.out, i18n.T("Sent. Thank you. Reference: %s")+"\n", id)
	return nil
}

// refusalText turns a service or validation code into words for people.
func refusalText(err error) string {
	var e *feedback.Error
	if !errors.As(err, &e) {
		return err.Error()
	}
	switch e.Code {
	case "empty_message":
		return i18n.T("The message is empty. Nothing was sent.")
	case "message_too_long":
		return fmt.Sprintf(i18n.T("The message is longer than %d characters. Nothing was sent."), feedback.MaxMessage)
	case "bad_email":
		return i18n.T("The e-mail address does not look right. Nothing was sent.")
	case "bad_kind":
		return i18n.T("The kind must be bug, idea or other. Nothing was sent.")
	case "system_too_long":
		return i18n.T("The system details are too large to send; leave out a part. Nothing was sent.")
	case "rate_limited":
		return i18n.T("The feedback service received too many reports from your network in the last hour. Nothing was sent; please try again later.")
	case "busy":
		return i18n.T("The feedback service received a lot of reports today. Nothing was sent; please try again tomorrow, or write to feedback@basalt-os.org.")
	}
	return fmt.Sprintf(i18n.T("The feedback service refused the report (%s). Nothing was sent; you can write to feedback@basalt-os.org instead."), e.Code)
}

// editReport lets the person change the report in their editor. The
// edited text is sent as they leave it (it is not scrubbed again).
func editReport(f feedbackIO, r *feedback.Report) error {
	tmp, err := os.CreateTemp("", "basalt-feedback-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if err := f.edit(tmp.Name()); err != nil {
		return err
	}
	b, err := os.ReadFile(tmp.Name())
	if err != nil {
		return err
	}
	var n feedback.Report
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&n); err != nil {
		return err
	}
	// Who sends it is not the person's to change.
	n.Source, n.Client, n.Lang = r.Source, r.Client, r.Lang
	*r = n
	return nil
}

func runEditor(path string) error {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		for _, c := range []string{"nano", "vi"} {
			if p, err := exec.LookPath(c); err == nil {
				ed = p
				break
			}
		}
	}
	if ed == "" {
		return errors.New("no editor (set EDITOR)")
	}
	f := strings.Fields(ed)
	cmd := exec.Command(f[0], append(f[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func readLine(in *bufio.Reader) string {
	s, _ := in.ReadString('\n')
	return strings.TrimSpace(s)
}

// readParagraph reads lines until an empty line or the end of input.
func readParagraph(in *bufio.Reader) string {
	var lines []string
	for {
		s, err := in.ReadString('\n')
		s = strings.TrimRight(s, "\r\n")
		if strings.TrimSpace(s) == "" && (len(lines) > 0 || err != nil) {
			break
		}
		if strings.TrimSpace(s) != "" || len(lines) > 0 {
			lines = append(lines, s)
		}
		if err != nil {
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func isYes(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "y" || s == strings.ToLower(i18n.T("y")) || isYesWord(s)
}

func isYesWord(s string) bool {
	return s == "yes" || s == strings.ToLower(i18n.T("yes"))
}

func printIndentedJSON(w io.Writer, v any) {
	b, err := json.MarshalIndent(v, "  ", "  ")
	if err != nil {
		return
	}
	fmt.Fprintln(w, "  "+string(b))
}

func printJSONTo(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
