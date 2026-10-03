// Package journal parses `journalctl -o json` output and recognizes the
// systemd and audit records the assistant reacts to.
package journal

import (
	"bufio"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Entry is one journal record, with the fields the assistant uses.
type Entry struct {
	Time       time.Time
	Message    string
	Unit       string // UNIT (PID 1 messages about a unit) or _SYSTEMD_UNIT
	SystemUnit string // _SYSTEMD_UNIT
	Identifier string // SYSLOG_IDENTIFIER
	Priority   int
	Transport  string
	AuditType  int
	MessageID  string
	PID        int
	Comm       string
	Cursor     string
	Fields     map[string]string
}

// Message IDs from systemd's catalog that mark a unit failure.
const (
	MsgUnitFailed      = "d9b373ed55a64feb8242e02dbe79a49c" // unit entered failed state
	MsgUnitResult      = "7ad2d189f7e94e70a38c781354912448" // unit result (Failed with result ...)
	MsgUnitProcessExit = "98e322203f7a4ed290d09fe03c09fe15" // main process exited
	MsgJobDoneFailed   = "be02cf6855d2428ba40df7e9d022f03d" // job failed (Failed to start)
)

// AuditAVC and friends are audit record types carried by the journal.
const (
	AuditAVC            = 1400
	AuditUserAVC        = 1107
	AuditSELinuxErr     = 1401
	AuditSoftwareUpdate = 1138
)

// ParseLine decodes one line of `journalctl -o json`. MESSAGE may be a
// string or, for non-UTF-8 payloads, an array of bytes.
func ParseLine(line []byte) (Entry, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return Entry{}, false
	}
	f := make(map[string]string, len(raw))
	for k, v := range raw {
		f[k] = decodeValue(v)
	}
	e := Entry{
		Message:    f["MESSAGE"],
		SystemUnit: f["_SYSTEMD_UNIT"],
		Identifier: f["SYSLOG_IDENTIFIER"],
		Transport:  f["_TRANSPORT"],
		MessageID:  f["MESSAGE_ID"],
		Comm:       f["_COMM"],
		Cursor:     f["__CURSOR"],
		Priority:   atoi(f["PRIORITY"], 6),
		AuditType:  atoi(f["_AUDIT_TYPE"], 0),
		PID:        atoi(f["_PID"], 0),
		Fields:     f,
	}
	e.Unit = f["UNIT"]
	if e.Unit == "" {
		e.Unit = e.SystemUnit
	}
	if us, err := strconv.ParseInt(f["__REALTIME_TIMESTAMP"], 10, 64); err == nil {
		e.Time = time.UnixMicro(us).UTC()
	}
	return e, true
}

// ParseAll decodes a whole `journalctl -o json` output.
func ParseAll(out string) []Entry {
	var es []Entry
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if e, ok := ParseLine(sc.Bytes()); ok {
			es = append(es, e)
		}
	}
	return es
}

func decodeValue(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var b []byte
	var ints []int
	if json.Unmarshal(v, &ints) == nil {
		b = make([]byte, len(ints))
		for i, n := range ints {
			b[i] = byte(n)
		}
		return string(b)
	}
	return strings.Trim(string(v), `"`)
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// IsAVC reports an SELinux denial record.
func (e Entry) IsAVC() bool {
	if e.Transport == "audit" && (e.AuditType == AuditAVC || e.AuditType == AuditUserAVC) {
		return true
	}
	return e.Transport == "kernel" && strings.Contains(e.Message, "avc:  denied")
}

// FailedUnit returns the unit a PID 1 record reports as failed, if any.
// systemd writes "X.service: Failed with result 'exit-code'." and
// "Failed to start X.service - Description." for the same failure; both are
// accepted so the dedup layer sees one event per unit.
func (e Entry) FailedUnit() (string, bool) {
	if e.PID != 1 && e.Identifier != "systemd" {
		return "", false
	}
	if e.Unit == "" || strings.HasSuffix(e.Unit, ".scope") || strings.HasPrefix(e.Unit, "basalt-assistant") {
		return "", false
	}
	switch e.MessageID {
	case MsgUnitFailed:
		return e.Unit, true
	case MsgUnitResult:
		if strings.Contains(e.Message, "Failed with result") {
			return e.Unit, true
		}
	}
	if e.Fields["JOB_RESULT"] == "failed" || e.Fields["JOB_RESULT"] == "dependency" {
		return e.Unit, true
	}
	if strings.Contains(e.Message, "Failed with result '") {
		return e.Unit, true
	}
	return "", false
}

// IsNoSpace reports an ENOSPC message from any program.
func (e Entry) IsNoSpace() bool {
	return strings.Contains(e.Message, "No space left on device")
}

// SoftwareUpdateFailed reports an rpm audit record of a failed package
// operation (rpm-plugin-audit writes one SOFTWARE_UPDATE per package).
func (e Entry) SoftwareUpdateFailed() (string, bool) {
	if e.Transport != "audit" || e.AuditType != AuditSoftwareUpdate {
		return "", false
	}
	if !strings.Contains(e.Message, "res=failed") && e.Fields["AUDIT_FIELD_RES"] != "failed" {
		return "", false
	}
	sw := strings.Trim(e.Fields["AUDIT_FIELD_SW"], `"`)
	if sw == "" {
		if i := strings.Index(e.Message, "sw=\""); i >= 0 {
			rest := e.Message[i+4:]
			if j := strings.IndexByte(rest, '"'); j >= 0 {
				sw = rest[:j]
			}
		}
	}
	return sw, true
}
