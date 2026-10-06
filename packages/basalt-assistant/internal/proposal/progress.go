package proposal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ProgressDir holds the step of each proposal being applied. It is under
// /run on purpose: it is not part of a snapshot (a rollback never brings
// back an apply that looks unfinished) and a restart clears it.
var ProgressDir = "/run/basalt-assistant/progress"

// Progress is where a running apply is.
type Progress struct {
	Proposal string    `json:"proposal"`
	Step     int       `json:"step"`
	Steps    int       `json:"steps"`
	What     string    `json:"what"`
	PID      int       `json:"pid"`
	Started  time.Time `json:"started"`
}

// WriteProgress records a step (best effort); steps == 0 clears it.
func WriteProgress(id string, step, steps int, what string, started time.Time) {
	if !reID.MatchString(id) {
		return
	}
	fn := filepath.Join(ProgressDir, id+".json")
	if steps == 0 {
		_ = os.Remove(fn)
		return
	}
	if err := os.MkdirAll(ProgressDir, 0o755); err != nil {
		return
	}
	b, _ := json.Marshal(Progress{Proposal: id, Step: step, Steps: steps, What: what, PID: os.Getpid(), Started: started.UTC()})
	tmp := fn + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, fn)
	}
}

// ReadProgress returns the step of a proposal being applied, if its
// process is still running.
func ReadProgress(id string) (Progress, bool) {
	var p Progress
	if !reID.MatchString(id) {
		return p, false
	}
	b, err := os.ReadFile(filepath.Join(ProgressDir, id+".json"))
	if err != nil || json.Unmarshal(b, &p) != nil || p.Proposal != id {
		return p, false
	}
	if p.PID > 0 {
		if _, err := os.Stat("/proc/" + strconv.Itoa(p.PID)); err != nil {
			return p, false
		}
	}
	return p, true
}
