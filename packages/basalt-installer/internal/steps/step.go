package steps

import (
	"fmt"
	"strings"

	"github.com/tui-tools/tui-kit/runner"
)

// Step is one action of the installation: a command (argv, never a shell
// string) or a file the engine writes. Secrets (passwords, the temporary
// LUKS key, the recovery key) travel in fields that are never serialized
// and are replaced by a description in the preview and the log.
type Step struct {
	ID    string   `json:"id"`
	Phase string   `json:"phase"`
	Title string   `json:"title"`
	Argv  []string `json:"argv,omitempty"`
	// Env adds variables to the command's environment.
	Env []EnvVar `json:"env,omitempty"`
	// Stdin is written to the command's standard input (a secret when
	// StdinShown differs from it).
	Stdin      string `json:"-"`
	StdinShown string `json:"stdin,omitempty"`
	// Write makes the step a file operation instead of a command.
	Write *FileWrite `json:"write,omitempty"`
	// Capture stores the command's standard output as the named secret
	// (recovery_key); the output is then neither shown nor logged.
	Capture string `json:"capture,omitempty"`
	// Undo runs on rollback when this step succeeded and a later one failed.
	Undo []string `json:"undo,omitempty"`
	// Release names an earlier step whose Undo this step performs, so the
	// rollback does not run it twice.
	Release string `json:"release,omitempty"`
	// Optional steps may fail: the failure is a warning, not an abort.
	Optional bool `json:"optional,omitempty"`
	// Progress names an output parser that reports progress ("dnf").
	Progress string `json:"progress,omitempty"`
	// Retries reruns a failed command this many times, 2 seconds apart
	// (device teardown that udev or the kernel may still hold briefly).
	Retries int `json:"retries,omitempty"`
	// KeepAttrsUnder makes the engine clear the append-only and immutable
	// attributes of the files below this directory for the command and
	// restore them afterwards (setfiles cannot relabel such files).
	KeepAttrsUnder string `json:"keep_attrs_under,omitempty"`
	// Weight is the step's share of the progress bar (default 1).
	Weight int `json:"weight"`
	// Note explains a step whose reason is not obvious from the command.
	Note string `json:"note,omitempty"`
}

// EnvVar is one environment variable of a command.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"-"`
	// Shown is what the preview prints for the value.
	Shown string `json:"value"`
}

// FileWrite is a file operation done by the engine itself.
type FileWrite struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	// Content is the file's content; Shown is what the preview prints
	// (equal to Content unless the content is secret).
	Content string `json:"-"`
	Shown   string `json:"content"`
	// RandomChars fills the file with this many random alphanumeric
	// characters, generated at run time (the temporary LUKS key).
	RandomChars int `json:"random_chars,omitempty"`
	// FromSecret fills the file with a captured secret (recovery_key).
	FromSecret string `json:"from_secret,omitempty"`
	// FromLog fills the file with the install log written so far.
	FromLog bool `json:"from_log,omitempty"`
	// Symlink makes Path a symbolic link to this target instead.
	Symlink string `json:"symlink,omitempty"`
}

// Command renders the step the way the preview, the TUI confirmation and
// the log show it: the exact argv, shell-quoted (tui-kit's quoting, so it
// reads the same as in every tui-tools confirmation), with secrets
// described instead of printed.
func (s Step) Command() string {
	if s.Write != nil {
		w := s.Write
		switch {
		case w.Symlink != "":
			return fmt.Sprintf("symlink %s -> %s", w.Path, w.Symlink)
		case w.RandomChars > 0:
			return fmt.Sprintf("write %s (mode %04o): %d random characters", w.Path, w.Mode, w.RandomChars)
		case w.FromSecret != "":
			return fmt.Sprintf("write %s (mode %04o): the %s", w.Path, w.Mode, strings.ReplaceAll(w.FromSecret, "_", " "))
		case w.FromLog:
			return fmt.Sprintf("write %s (mode %04o): the install log so far", w.Path, w.Mode)
		}
		return fmt.Sprintf("write %s (mode %04o, %d bytes)", w.Path, w.Mode, len(w.Content))
	}
	var b strings.Builder
	for _, e := range s.Env {
		b.WriteString(e.Name + "=" + runner.Quote(e.Shown) + " ")
	}
	b.WriteString(runner.Join(s.Argv))
	if s.StdinShown != "" {
		b.WriteString("  <<< " + s.StdinShown)
	}
	return b.String()
}

// Preview renders a step list as text: one block per step, numbered, with
// the phase, the title, the command and the content of every file written.
func Preview(list []Step) string {
	var b strings.Builder
	phase := ""
	for i, s := range list {
		if s.Phase != phase {
			phase = s.Phase
			fmt.Fprintf(&b, "\n== %s\n", phase)
		}
		fmt.Fprintf(&b, "%3d. %s", i+1, s.Title)
		if s.Optional {
			b.WriteString(" (may fail)")
		}
		b.WriteString("\n")
		if s.Note != "" {
			fmt.Fprintf(&b, "     # %s\n", s.Note)
		}
		fmt.Fprintf(&b, "     $ %s\n", s.Command())
		if s.Write != nil && s.Write.Symlink == "" && s.Write.RandomChars == 0 && s.Write.FromSecret == "" && !s.Write.FromLog {
			for _, line := range strings.Split(strings.TrimRight(s.Write.Shown, "\n"), "\n") {
				fmt.Fprintf(&b, "       | %s\n", line)
			}
		}
	}
	return strings.TrimLeft(b.String(), "\n")
}
