package collect

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/record"
)

// SnapshotInfo is snapper's info.xml.
type SnapshotInfo struct {
	Type        string `xml:"type"`
	Num         int    `xml:"num"`
	Date        string `xml:"date"`
	Description string `xml:"description"`
	Cleanup     string `xml:"cleanup"`
	PreNum      int    `xml:"pre_num"`
	UserData    []struct {
		Key   string `xml:"key"`
		Value string `xml:"value"`
	} `xml:"userdata"`
}

var (
	writableCopy = regexp.MustCompile(`writable copy of #(\d+)`)
	rollbackDesc = regexp.MustCompile(`(?i)rollback`)
)

// FromSnapshot maps a new snapshot to a record: a rollback or a plain
// snapshot (before and after a dnf transaction, a manual one). snapper
// rollback makes two snapshots: a read-only backup of the current root
// (with a cleanup algorithm) and a writable copy of the chosen snapshot,
// which becomes the next root and has no cleanup algorithm. Its default
// description is "writable copy of #N"; basalt-rollback passes its own
// description, so a rollback description without a cleanup algorithm also
// marks the new root.
func FromSnapshot(si SnapshotInfo) record.Record {
	r := record.Record{Producer: "snapper", UID: 0, Outcome: "ok", Subject: record.Subject{App: "snapper"}}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", si.Date, time.UTC); err == nil {
		r.Time = t.UTC().Format(time.RFC3339Nano)
	}
	d := map[string]any{"number": si.Num, "type": si.Type, "description": si.Description, "cleanup": si.Cleanup}
	if si.PreNum > 0 {
		d["pre_num"] = si.PreNum
	}
	for _, u := range si.UserData {
		d["userdata_"+u.Key] = u.Value
	}
	switch m := writableCopy.FindStringSubmatch(si.Description); {
	case m != nil:
		r.Event = "snapshot.rollback"
		d["target"], _ = strconv.Atoi(m[1])
		d["new_root"] = si.Num
	case rollbackDesc.MatchString(si.Description) && si.Cleanup == "":
		r.Event = "snapshot.rollback"
		d["new_root"] = si.Num
	default:
		r.Event = "snapshot.create"
	}
	r.Data = data(d)
	return r
}

// ReadSnapshots returns the snapshots under dir (/.snapshots) by number.
func ReadSnapshots(dir string) []SnapshotInfo {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []SnapshotInfo
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "info.xml"))
		if err != nil {
			continue
		}
		var si SnapshotInfo
		if xml.Unmarshal(b, &si) != nil || si.Num != n {
			continue
		}
		out = append(out, si)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Num < out[j].Num })
	return out
}

// Snapper polls dir for snapshots newer than the last one recorded
// (stateFile holds its number; the first run records none, only the
// baseline).
func Snapper(dir, stateFile string, every time.Duration, sink Sink, logf func(string, ...any), stop <-chan struct{}) {
	last := -1
	if b, err := os.ReadFile(stateFile); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			last = n
		}
	}
	for {
		snaps := ReadSnapshots(dir)
		if last < 0 {
			last = 0
			if len(snaps) > 0 {
				last = snaps[len(snaps)-1].Num
			}
			_ = os.WriteFile(stateFile, []byte(strconv.Itoa(last)+"\n"), 0o600)
		}
		for _, si := range snaps {
			if si.Num <= last {
				continue
			}
			if err := sink("snapper", FromSnapshot(si)); err != nil {
				logf("snapper collector: %v", err)
				break
			}
			last = si.Num
			_ = os.WriteFile(stateFile, []byte(strconv.Itoa(last)+"\n"), 0o600)
		}
		select {
		case <-stop:
			return
		case <-time.After(every):
		}
	}
}
