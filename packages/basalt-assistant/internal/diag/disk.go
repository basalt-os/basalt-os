package diag

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
)

// DiskReport is the state of the root file system.
type DiskReport struct {
	FS            FSStat           `json:"fs"`
	UsedPct       float64          `json:"used_pct"`
	Btrfs         map[string]int64 `json:"btrfs,omitempty"` // from btrfs filesystem usage -b
	Snapshots     []SnapSpace      `json:"snapshots,omitempty"`
	SnapshotTotal int64            `json:"snapshot_exclusive_total"`
	Journal       int64            `json:"journal_bytes"`
	PkgCache      int64            `json:"package_cache_bytes"`
	Forecast      Forecast         `json:"forecast"`
	Features      map[string]bool  `json:"features"`
	Decision      decide.Decision  `json:"decision"`
	Explanation   string           `json:"explanation"`
	Evidence      []string         `json:"evidence"`
	Actions       []action.Action  `json:"actions,omitempty"`
	Skipped       []string         `json:"skipped,omitempty"`
}

// SnapSpace is the space only one snapshot holds.
type SnapSpace struct {
	Snapshot  Snapshot `json:"snapshot"`
	Exclusive int64    `json:"exclusive"`
	Total     int64    `json:"total"`
}

// Forecast is a linear fit over stored usage samples.
type Forecast struct {
	Samples     int     `json:"samples"`
	SpanHours   float64 `json:"span_hours"`
	BytesPerDay float64 `json:"bytes_per_day"`
	DaysToFull  float64 `json:"days_to_full"` // -1 when not growing or unknown
	Note        string  `json:"note"`
}

// Sample is one stored usage point.
type Sample struct {
	Time  time.Time `json:"t"`
	Used  uint64    `json:"used"`
	Total uint64    `json:"total"`
}

// Thresholds for the disk features.
type DiskThresholds struct {
	WarnPct, CritPct    float64
	SnapshotsLargeBytes int64
	JournalLargeBytes   int64
	CacheLargeBytes     int64
	JournalVacuumTo     string
}

// DefaultDiskThresholds are used when the configuration has none.
var DefaultDiskThresholds = DiskThresholds{WarnPct: 85, CritPct: 95, SnapshotsLargeBytes: 1 << 30,
	JournalLargeBytes: 512 << 20, CacheLargeBytes: 512 << 20, JournalVacuumTo: "200M"}

// RecordSample appends a usage sample to the history (daemon and CLI).
func (e *Env) RecordSample(s FSStat) {
	if e.HistoryPath == "" {
		return
	}
	f, err := os.OpenFile(e.HistoryPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(Sample{Time: e.now().UTC().Truncate(time.Second), Used: s.Total - s.Free, Total: s.Total})
	_, _ = f.Write(append(b, '\n'))
}

// LoadSamples reads the history, keeping the last 14 days.
func (e *Env) LoadSamples() []Sample {
	f, err := os.Open(e.HistoryPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	cut := e.now().Add(-14 * 24 * time.Hour)
	var out []Sample
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var s Sample
		if json.Unmarshal(sc.Bytes(), &s) == nil && s.Time.After(cut) {
			out = append(out, s)
		}
	}
	return out
}

// Predict fits used bytes over time (least squares) and projects when the
// file system reaches its size.
func Predict(samples []Sample, cur FSStat) Forecast {
	f := Forecast{Samples: len(samples), DaysToFull: -1}
	if len(samples) < 3 {
		f.Note = "not enough history yet (the daemon records a sample every few minutes)"
		return f
	}
	t0 := samples[0].Time
	var sx, sy, sxx, sxy float64
	for _, s := range samples {
		x := s.Time.Sub(t0).Hours() / 24
		y := float64(s.Used)
		sx, sy, sxx, sxy = sx+x, sy+y, sxx+x*x, sxy+x*y
	}
	n := float64(len(samples))
	f.SpanHours = samples[len(samples)-1].Time.Sub(t0).Hours()
	den := n*sxx - sx*sx
	if f.SpanHours < 1 || den == 0 {
		f.Note = "history spans less than an hour"
		return f
	}
	slope := (n*sxy - sx*sy) / den // bytes per day
	f.BytesPerDay = slope
	if slope <= 0 {
		f.Note = "usage is not growing"
		return f
	}
	free := float64(cur.Free)
	f.DaysToFull = free / slope
	f.Note = fmt.Sprintf("growing %s per day; full in about %.1f days at this rate", HumanBytes(int64(slope)), f.DaysToFull)
	return f
}

var reUsage = regexp.MustCompile(`^\s*(Device size|Device allocated|Device unallocated|Used|Free \(estimated\)|Free \(statfs, df\)|Data ratio|Metadata ratio):\s+(\S+)`)

// ParseBtrfsUsage reads `btrfs filesystem usage -b` (raw numbers).
func ParseBtrfsUsage(out string) map[string]int64 {
	m := map[string]int64{}
	for _, l := range strings.Split(out, "\n") {
		if mm := reUsage.FindStringSubmatch(l); mm != nil {
			if n, err := strconv.ParseFloat(mm[2], 64); err == nil {
				key := strings.ToLower(strings.NewReplacer(" ", "_", "(", "", ")", "", ",", "").Replace(mm[1]))
				if strings.HasSuffix(key, "ratio") {
					n *= 100
				}
				m[key] = int64(n)
			}
		}
	}
	return m
}

// ParseFiDu reads `btrfs filesystem du -s --raw` (Total, Exclusive, Set shared, Filename).
func ParseFiDu(out string) (total, exclusive int64, ok bool) {
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) >= 4 && f[0] != "Total" {
			t, e1 := strconv.ParseInt(f[0], 10, 64)
			x, e2 := strconv.ParseInt(f[1], 10, 64)
			if e1 == nil && e2 == nil {
				return t, x, true
			}
		}
	}
	return 0, 0, false
}

var reJournalUsage = regexp.MustCompile(`take up ([0-9.]+)([KMGT]?)B?`)

func parseHuman(num, unit string) int64 {
	f, _ := strconv.ParseFloat(num, 64)
	mult := map[string]float64{"": 1, "K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40}[unit]
	return int64(f * mult)
}

// Disk builds the disk report. maxSnaps bounds the per-snapshot scan.
func (e *Env) Disk(ctx context.Context, th DiskThresholds, maxSnaps int) (*DiskReport, error) {
	st, err := e.Statfs("/")
	if err != nil {
		return nil, err
	}
	r := &DiskReport{FS: st, UsedPct: st.UsedPct(), Features: map[string]bool{}}
	r.Evidence = append(r.Evidence, fmt.Sprintf("/: %s used of %s (%.1f %%), %s available",
		HumanBytes(int64(st.Total-st.Free)), HumanBytes(int64(st.Total)), r.UsedPct, HumanBytes(int64(st.Free))))
	if r.UsedPct >= th.WarnPct {
		r.Features["disk_warn"] = true
	}
	if r.UsedPct >= th.CritPct {
		r.Features["disk_crit"] = true
	}

	if !e.Confined {
		if res := e.R.Read(ctx, "btrfs", "filesystem", "usage", "-b", "/"); res.Code == 0 {
			r.Btrfs = ParseBtrfsUsage(res.Out)
		}
	}

	if res := e.R.Read(ctx, "journalctl", "--disk-usage"); res.Code == 0 {
		if m := reJournalUsage.FindStringSubmatch(res.Out); m != nil {
			r.Journal = parseHuman(m[1], m[2])
		}
	}
	if e.Confined {
		r.Skipped = append(r.Skipped, "package cache size (needs `basalt disk` as root)")
	} else if res := e.R.Read(ctx, "du", "-sb", "/var/cache/libdnf5"); res.Code == 0 {
		if f := strings.Fields(res.Out); len(f) > 0 {
			r.PkgCache, _ = strconv.ParseInt(f[0], 10, 64)
		}
	}
	r.Evidence = append(r.Evidence, fmt.Sprintf("journal %s, package cache %s", HumanBytes(r.Journal), HumanBytes(r.PkgCache)))
	if r.Journal >= th.JournalLargeBytes {
		r.Features["journal_large"] = true
	}
	if r.PkgCache >= th.CacheLargeBytes {
		r.Features["cache_large"] = true
	}

	snaps := e.Snapshots(ctx)
	if e.Confined {
		r.Skipped = append(r.Skipped, "space held by each snapshot (needs `basalt disk` as root)")
		r.Evidence = append(r.Evidence, fmt.Sprintf("%d snapshots of the root", len(snaps)))
	} else {
		start := 0
		if maxSnaps > 0 && len(snaps) > maxSnaps {
			start = len(snaps) - maxSnaps
		}
		for _, s := range snaps[start:] {
			res := e.R.Read(ctx, "btrfs", "filesystem", "du", "-s", "--raw", fmt.Sprintf("%s/%d/snapshot", e.SnapshotDir, s.Number))
			if t, x, ok := ParseFiDu(res.Out); ok {
				r.Snapshots = append(r.Snapshots, SnapSpace{Snapshot: s, Exclusive: x, Total: t})
				r.SnapshotTotal += x
			}
		}
		sort.SliceStable(r.Snapshots, func(i, j int) bool { return r.Snapshots[i].Exclusive > r.Snapshots[j].Exclusive })
		r.Evidence = append(r.Evidence, fmt.Sprintf("%d snapshots hold %s exclusively", len(r.Snapshots), HumanBytes(r.SnapshotTotal)))
		if r.SnapshotTotal >= th.SnapshotsLargeBytes {
			r.Features["snapshots_large"] = true
		}
	}

	e.RecordSample(st)
	r.Forecast = Predict(e.LoadSamples(), st)
	if r.Forecast.DaysToFull >= 0 && r.Forecast.DaysToFull < 7 {
		r.Features["disk_warn"] = true
	}
	r.Evidence = append(r.Evidence, "forecast: "+r.Forecast.Note)

	r.Decision = e.ask(ctx, decide.DiskCause("/", r.Features))
	e.diskPlan(r, th)
	return r, nil
}

func (e *Env) diskPlan(r *DiskReport, th DiskThresholds) {
	if !r.Features["disk_warn"] && !r.Features["disk_crit"] {
		r.Explanation = fmt.Sprintf("The root file system is %.0f %% full; nothing to do.", r.UsedPct)
		return
	}
	r.Explanation = fmt.Sprintf("The root file system is %.0f %% full.", r.UsedPct)
	switch r.Decision.Answer.Top {
	case "snapshots":
		// The biggest snapshot that is not marked important, not the
		// initial one, and not a pre snapshot whose post is the newest.
		for _, s := range r.Snapshots {
			if s.Snapshot.Userdata["important"] == "yes" || s.Exclusive < th.SnapshotsLargeBytes/4 {
				continue
			}
			r.Explanation += fmt.Sprintf(" Snapshot %d (%s, %q) alone holds %s; deleting it frees that space (it can no longer be rolled back to).",
				s.Snapshot.Number, s.Snapshot.Date, s.Snapshot.Description, HumanBytes(s.Exclusive))
			r.Actions = append(r.Actions, action.Action{Kind: action.SnapshotDelete, Params: map[string]string{"snapshot": strconv.Itoa(s.Snapshot.Number)}})
			break
		}
	case "journal":
		r.Explanation += fmt.Sprintf(" The journal uses %s; vacuuming archived files to %s is proposed.", HumanBytes(r.Journal), th.JournalVacuumTo)
		r.Actions = append(r.Actions, action.Action{Kind: action.JournalVacuum, Params: map[string]string{"size": th.JournalVacuumTo}})
	case "package_cache":
		r.Explanation += fmt.Sprintf(" Cached packages use %s; cleaning them is proposed.", HumanBytes(r.PkgCache))
		r.Actions = append(r.Actions, action.Action{Kind: action.DnfClean, Params: map[string]string{}})
	default:
		r.Explanation += " Nothing the assistant may reclaim was found (snapshots, journal, package cache); the space is data. Nothing is deleted automatically."
		if e.Confined {
			r.Explanation += " Run `basalt disk` as root to measure the space each snapshot holds."
		}
	}
	if len(r.Actions) == 0 && r.Decision.Answer.Top != "other_data" {
		r.Explanation += " No single item is large enough to propose."
	}
}

// HumanBytes renders a size in binary units.
func HumanBytes(n int64) string {
	f := float64(n)
	for _, u := range []string{"B", "KiB", "MiB", "GiB", "TiB"} {
		if f < 1024 || u == "TiB" {
			if u == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1024
	}
	return ""
}
