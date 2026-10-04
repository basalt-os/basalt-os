package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeSources serves files and command outputs from maps.
func fakeSources(files map[string]string, cmds map[string]string) Sources {
	return Sources{
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		},
		Exists: func(p string) bool { _, ok := files[p]; return ok },
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if v, ok := cmds[name]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("not found")
		},
	}
}

func TestCollectOS(t *testing.T) {
	src := fakeSources(map[string]string{
		"/etc/os-release":            "NAME=\"Basalt OS\"\nVERSION=\"44 (Basalt 0.0.1)\"\nID=basalt\nBASALT_VERSION=0.0.1\nBUILD_ID=20261004.1\n# comment\n",
		"/proc/sys/kernel/osrelease": "6.17.1-300.fc44.x86_64\n",
	}, nil)
	o, err := CollectOS(src)
	if err != nil {
		t.Fatal(err)
	}
	want := OSInfo{Name: "Basalt OS", Version: "44 (Basalt 0.0.1)", BasaltVersion: "0.0.1", BuildID: "20261004.1", Kernel: "6.17.1-300.fc44.x86_64"}
	if *o != want {
		t.Fatalf("%+v", *o)
	}
}

func TestCollectPackagesAndHardware(t *testing.T) {
	src := fakeSources(map[string]string{
		"/proc/cpuinfo":       "processor\t: 0\nmodel name\t: AMD   Ryzen 7 5800X\nprocessor\t: 1\nmodel name\t: AMD Ryzen 7 5800X\n",
		"/proc/meminfo":       "MemTotal:       16314880 kB\nMemFree: 1 kB\n",
		"/sys/firmware/efi":   "",
		"/sys/class/tpm/tpm0": "",
	}, map[string]string{
		"rpm":                 "basalt-release 44-7.fc44\nbasalt-assistant 0.9.0-1.fc44\n\n",
		"systemd-detect-virt": "kvm\n",
	})
	pkgs, err := CollectPackages(context.Background(), src)
	if err != nil || strings.Join(pkgs, "|") != "basalt-assistant 0.9.0-1.fc44|basalt-release 44-7.fc44" {
		t.Fatalf("%v %v", pkgs, err)
	}
	h := CollectHardware(context.Background(), src, "amd64")
	if h.CPU != "AMD Ryzen 7 5800X" || h.Cores != 2 || h.MemoryGiB != 15.6 || h.Virtualization != "kvm" || h.Firmware != "uefi" || !h.TPM {
		t.Fatalf("%+v", *h)
	}
}

func TestCollectFindings(t *testing.T) {
	line := func(title, subject string) string {
		data, _ := json.Marshal(map[string]any{"proposal": "p-1", "title": title, "kind": "unit", "subject": subject, "severity": 3})
		e, _ := json.Marshal(map[string]string{
			"_SYSTEMD_UNIT": "basalt-assistantd.service", "BASALT_AUDIT_TYPE": "finding",
			"BASALT_AUDIT_DATA": string(data), "__REALTIME_TIMESTAMP": "1791150000000000", "_HOSTNAME": "alice-laptop",
		})
		return string(e)
	}
	out := line("nginx.service stopped", "nginx.service") + "\n" + line("disk filling", "/") + "\n"
	fs, err := CollectFindings(context.Background(), fakeSources(nil, map[string]string{"journalctl": out}), 7*24*time.Hour)
	if err != nil || len(fs) != 2 {
		t.Fatalf("%v %v", fs, err)
	}
	if fs[0].Title != "nginx.service stopped" || fs[0].Kind != "unit" || fs[0].Time == "" {
		t.Fatalf("%+v", fs[0])
	}
	b, _ := json.Marshal(fs)
	if strings.Contains(string(b), "alice-laptop") {
		t.Fatal("the journal's host name must not reach a finding")
	}
}

func TestScrub(t *testing.T) {
	s := NewScrubber([]string{"alice-laptop.home.example"}, []string{"alice", "Alice Smith", "x", "root"})
	cases := []struct{ in, want string }{
		{"error on alice-laptop.home.example and on alice-laptop", "error on [redacted] and on [redacted]"},
		{"user alice could not log in", "user [redacted] could not log in"},
		{"Alice Smith wrote this", "[redacted] wrote this"},
		{"file /home/alice/notes.txt", "file /home/[redacted]/notes.txt"},
		{"file /root/secret-plan.txt", "file /root/[redacted]"},
		{"from 192.168.1.20 and fe80::1:2:3:4", "from [redacted] and [redacted]"},
		{"nic 52:54:00:12:34:56 up", "nic [redacted] up"},
		{"password=hunter2 token: abc", "password=[redacted] token: [redacted]"},
		{"key ghp_1234567890abcdefABCDEF1234567890abcd", "key [redacted]"},
		{"write to me at someone@example.org", "write to me at [redacted]"},
		// Not names: system words, one-letter names, parts of other words.
		{"root and x and malice and alicex", "root and x and malice and alicex"},
		{"nginx.service failed on Basalt OS 44", "nginx.service failed on Basalt OS 44"},
	}
	for _, c := range cases {
		if got := s.Scrub(c.in); got != c.want {
			t.Errorf("Scrub(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestScrubReportKeepsReplyAddress(t *testing.T) {
	s := NewScrubber([]string{"box1"}, []string{"bob"})
	r := &Report{Kind: "bug", Message: "bob here, box1 broke", Email: "bob@example.org",
		System: &System{OS: &OSInfo{Name: "Basalt OS"}, Findings: []Finding{{Title: "/home/bob/x failed", Subject: "box1"}}}}
	if n := s.ScrubReport(r); n != 3 {
		t.Fatalf("changed %d values: %+v", n, r)
	}
	if r.Email != "bob@example.org" || strings.Contains(r.Message, "bob") || r.System.Findings[0].Subject != "[redacted]" {
		t.Fatalf("%+v %+v", r, r.System.Findings[0])
	}
}

func TestLocalNames(t *testing.T) {
	src := fakeSources(map[string]string{
		"/etc/hostname": "pc-of-carol\n",
		"/etc/passwd":   "root:x:0:0:root:/root:/bin/bash\ncarol:x:1000:1000:Carol Jones,,,:/home/carol:/bin/bash\nnobody:x:65534:65534:Kernel Overflow User:/:/sbin/nologin\nsvc:x:990:990::/:/sbin/nologin\n",
	}, nil)
	hosts, users := LocalNames(src)
	if !contains(hosts, "pc-of-carol") || !contains(users, "carol") || !contains(users, "Carol Jones") || contains(users, "nobody") || contains(users, "svc") {
		t.Fatalf("hosts %v users %v", hosts, users)
	}
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func TestParseParts(t *testing.T) {
	p, err := ParseParts("os, packages,os")
	if err != nil || len(p) != 2 || p[0] != PartOS || p[1] != PartPackages {
		t.Fatalf("%v %v", p, err)
	}
	if p, _ := ParseParts("all"); len(p) != 4 {
		t.Fatal(p)
	}
	if p, _ := ParseParts("none"); p != nil {
		t.Fatal(p)
	}
	if _, err := ParseParts("os,serial"); err == nil {
		t.Fatal("unknown part accepted")
	}
}

func TestValidate(t *testing.T) {
	ok := &Report{Kind: "idea", Message: "more themes"}
	if err := Validate(ok); err != nil {
		t.Fatal(err)
	}
	bad := []struct {
		r    Report
		code string
	}{
		{Report{Kind: "rant", Message: "x"}, "bad_kind"},
		{Report{Kind: "bug", Message: "  "}, "empty_message"},
		{Report{Kind: "bug", Message: strings.Repeat("é", MaxMessage+1)}, "message_too_long"},
		{Report{Kind: "bug", Message: "x", Email: "nope@"}, "bad_email"},
		{Report{Kind: "bug", Message: "x", System: &System{Packages: []string{strings.Repeat("p", MaxSystemJSON)}}}, "system_too_long"},
	}
	for _, c := range bad {
		if err := Validate(&c.r); !IsRefusal(err, c.code) {
			t.Errorf("%s: %v", c.code, err)
		}
	}
	if err := Validate(&Report{Kind: "bug", Message: strings.Repeat("é", MaxMessage)}); err != nil {
		t.Fatal("the limit counts characters, not bytes")
	}
}

func TestCodeBindsThePayload(t *testing.T) {
	r := &Report{Kind: "bug", Message: "a", Source: "cli", Client: "basalt-assistant/test"}
	p1, _ := Payload(r)
	r.Message = "b"
	p2, _ := Payload(r)
	if len(Code(p1)) != 8 || Code(p1) == Code(p2) {
		t.Fatal(Code(p1), Code(p2))
	}
	if strings.Contains(string(p1), `"system"`) || strings.Contains(string(p1), `"email"`) {
		t.Fatalf("parts not accepted must not appear: %s", p1)
	}
}

func TestCheckEndpoint(t *testing.T) {
	for _, ok := range []string{DefaultEndpoint, "http://127.0.0.1:8787/v1/feedback", "http://localhost/x"} {
		if err := CheckEndpoint(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://feedback.example.org/x", "ftp://x/y", "not a url", ""} {
		if CheckEndpoint(bad) == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestSend(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Origin") != "" || r.Header.Get("Cookie") != "" {
			w.WriteHeader(400)
			return
		}
		var rep Report
		_ = json.Unmarshal(got, &rep)
		switch rep.Message {
		case "limit":
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"ok":false,"error":"rate_limited"}`))
		case "broken":
			w.WriteHeader(502)
			_, _ = w.Write([]byte(`<html>bad gateway</html>`))
		default:
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"ok":true,"id":"20261004T153012Z-1a2b3c4d"}`))
		}
	}))
	defer srv.Close()
	ep := srv.URL + "/v1/feedback" // http://127.0.0.1:PORT
	p, _ := Payload(&Report{Kind: "bug", Message: "hello", Source: "cli", Client: "t"})
	id, err := Send(context.Background(), srv.Client(), ep, p)
	if err != nil || id != "20261004T153012Z-1a2b3c4d" || string(got) != string(p) {
		t.Fatalf("%q %v %s", id, err, got)
	}
	p, _ = Payload(&Report{Kind: "bug", Message: "limit"})
	if _, err := Send(context.Background(), srv.Client(), ep, p); !IsRefusal(err, "rate_limited") {
		t.Fatal(err)
	}
	p, _ = Payload(&Report{Kind: "bug", Message: "broken"})
	if _, err := Send(context.Background(), srv.Client(), ep, p); !IsRefusal(err, "http_error") {
		t.Fatal(err)
	}
	if _, err := Send(context.Background(), srv.Client(), "http://example.org/x", p); err == nil {
		t.Fatal("plain http to another host must be refused")
	}
}
