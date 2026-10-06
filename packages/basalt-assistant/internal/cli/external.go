package cli

import (
	"os"
	"path/filepath"
	"regexp"
)

// The basalt command hands subcommands it does not have to other Basalt
// programs, so `basalt ledger verify` runs `basalt-ledger verify`. Only an
// executable named basalt-<name> in one of these fixed directories is
// considered; PATH and the environment never take part, so nothing a user
// or an agent puts on PATH can be reached through the basalt command.
var externalDirs = []string{"/usr/libexec/basalt", "/usr/bin"}

// builtins are the basalt command's own subcommands (dispatch and Main);
// they always win over an external program of the same name.
var builtins = map[string]bool{"help": true, "version": true, "status": true, "why": true, "fix": true,
	"snapshots": true, "snapshot": true, "disk": true, "pending": true, "show": true, "apply": true,
	"ignore": true, "confirm": true, "submit": true, "audit": true, "ask": true, "feedback": true, "drivers": true,
	"updates": true, "update": true, "channels": true, "channel": true, "__source": true}

// internalHelpers live in /usr/libexec/basalt but are not commands for
// people: the assistant's daemon and helpers its services run.
var internalHelpers = map[string]bool{"assistantd": true, "notify": true, "policy-query": true}

var externalNameRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// External returns the program that runs `basalt NAME`, when NAME is not
// one of the basalt command's own subcommands and an executable regular
// file basalt-NAME exists in one of the fixed directories.
func External(name string) (string, bool) {
	return externalIn(name, externalDirs)
}

func externalIn(name string, dirs []string) (string, bool) {
	if builtins[name] || internalHelpers[name] || len(name) > 40 || !externalNameRe.MatchString(name) {
		return "", false
	}
	for _, d := range dirs {
		p := filepath.Join(d, "basalt-"+name)
		st, err := os.Stat(p)
		if err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}
