package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/feedback"
)

// feedbackServer records what it receives and answers like the service.
type feedbackServer struct {
	*httptest.Server
	got [][]byte
}

func newFeedbackServer(t *testing.T) *feedbackServer {
	fs := &feedbackServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fs.got = append(fs.got, b)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true,"id":"20261004T153012Z-1a2b3c4d"}`))
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (fs *feedbackServer) endpoint() string { return fs.URL + "/v1/feedback" }

func fakeFeedbackIO(input string, interactive bool, client *http.Client) (feedbackIO, *bytes.Buffer) {
	out := &bytes.Buffer{}
	src := feedback.Sources{
		ReadFile: func(p string) ([]byte, error) {
			switch p {
			case "/etc/os-release":
				return []byte("NAME=\"Basalt OS\"\nVERSION=\"44 (Basalt 0.0.1)\"\nBASALT_VERSION=0.0.1\n"), nil
			case "/proc/sys/kernel/osrelease":
				return []byte("6.17.1-300.fc44.x86_64\n"), nil
			case "/proc/cpuinfo":
				return []byte("processor : 0\nmodel name : Test CPU\n"), nil
			}
			return nil, os.ErrNotExist
		},
		Exists: func(string) bool { return false },
		Run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "rpm" {
				return []byte("basalt-assistant 0.9.0-1.fc44\n"), nil
			}
			return nil, errors.New("no")
		},
	}
	return feedbackIO{
		in: bufio.NewReader(strings.NewReader(input)), out: out, interactive: interactive, src: src,
		scrubber: feedback.NewScrubber([]string{"mybox"}, []string{"carol"}),
		client:   client, edit: func(string) error { return errors.New("no editor") },
		arch: "amd64", findings: time.Hour,
	}, out
}

func TestFeedbackInteractiveAsksForEachPart(t *testing.T) {
	t.Setenv("LANGUAGE", "")
	t.Setenv("LC_ALL", "C")
	srv := newFeedbackServer(t)
	// kind 1 (bug); the message (two lines, then an empty line); no e-mail;
	// the OS part yes, packages no, hardware no, findings (none readable);
	// then send.
	input := "1\nThe installer froze on mybox\nfor user carol\n\n\ny\nn\nn\nyes\n"
	f, out := fakeFeedbackIO(input, true, srv.Client())
	o := opts{args: []string{"feedback"}}
	if err := runFeedback(context.Background(), o, srv.endpoint(), "0.9.0-1", f); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(srv.got) != 1 {
		t.Fatalf("sent %d times\n%s", len(srv.got), out)
	}
	var r feedback.Report
	if err := json.Unmarshal(srv.got[0], &r); err != nil {
		t.Fatal(err)
	}
	if r.Kind != "bug" || r.Message != "The installer froze on [redacted]\nfor user [redacted]" || r.Source != "cli" || r.Client != "basalt-assistant/0.9.0-1" {
		t.Fatalf("%+v", r)
	}
	if r.System == nil || r.System.OS == nil || r.System.OS.Name != "Basalt OS" || r.System.Packages != nil || r.System.Hardware != nil {
		t.Fatalf("only the accepted part may be sent: %s", srv.got[0])
	}
	text := out.String()
	for _, want := range []string{"Include this? [y/N]", "This is exactly what will be sent to", "1 value was changed to hide personal data", "Sent. Thank you. Reference: 20261004T153012Z-1a2b3c4d"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	// What was shown is what was sent.
	if !strings.Contains(text, `"message": "The installer froze on [redacted]\nfor user [redacted]"`) {
		t.Errorf("the preview must show the scrubbed message:\n%s", text)
	}
}

func TestFeedbackInteractiveCancel(t *testing.T) {
	srv := newFeedbackServer(t)
	f, out := fakeFeedbackIO("n\nn\nn\n\n", true, srv.Client())
	o := opts{args: []string{"feedback", "dark", "mode"}, kind: "idea", email: "me@example.org"}
	err := runFeedback(context.Background(), o, srv.endpoint(), "t", f)
	if !errors.Is(err, errNotSent) || len(srv.got) != 0 || !strings.Contains(out.String(), "Nothing was sent.") {
		t.Fatalf("%v %d\n%s", err, len(srv.got), out)
	}
}

var reCode = regexp.MustCompile(`--yes --confirm ([0-9a-f]{8})`)

func TestFeedbackPreviewThenConfirmedSend(t *testing.T) {
	srv := newFeedbackServer(t)
	base := opts{args: []string{"feedback", "basalt why nginx says unknown"}, kind: "bug", include: "os,packages", includeSet: true, source: "voice"}

	o := base
	o.preview = true
	f, out := fakeFeedbackIO("", false, srv.Client())
	if err := runFeedback(context.Background(), o, srv.endpoint(), "t", f); err != nil {
		t.Fatal(err)
	}
	m := reCode.FindStringSubmatch(out.String())
	if m == nil || len(srv.got) != 0 {
		t.Fatalf("preview must print the code and send nothing:\n%s", out)
	}

	o = base
	o.yes, o.confirm = true, "00000000"
	f, out = fakeFeedbackIO("", false, srv.Client())
	if err := runFeedback(context.Background(), o, srv.endpoint(), "t", f); !errors.Is(err, errNotSent) || len(srv.got) != 0 {
		t.Fatalf("a wrong code must send nothing: %v\n%s", err, out)
	}

	o.confirm = m[1]
	f, out = fakeFeedbackIO("", false, srv.Client())
	if err := runFeedback(context.Background(), o, srv.endpoint(), "t", f); err != nil || len(srv.got) != 1 {
		t.Fatalf("%v %d\n%s", err, len(srv.got), out)
	}
	var r feedback.Report
	_ = json.Unmarshal(srv.got[0], &r)
	if r.Source != "voice" || r.System == nil || len(r.System.Packages) != 1 || r.System.OS == nil {
		t.Fatalf("%s", srv.got[0])
	}
	if feedback.Code(srv.got[0]) != m[1] {
		t.Fatal("the payload sent differs from the one previewed")
	}
}

func TestFeedbackPreviewJSON(t *testing.T) {
	f, out := fakeFeedbackIO("", false, nil)
	o := opts{args: []string{"feedback", "x"}, kind: "other", preview: true, json: true}
	if err := runFeedback(context.Background(), o, "https://feedback.example.org/v1/feedback", "t", f); err != nil {
		t.Fatal(err)
	}
	var v struct {
		Code   string          `json:"code"`
		Report feedback.Report `json:"report"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil || len(v.Code) != 8 || v.Report.Message != "x" || v.Report.System != nil {
		t.Fatalf("%v %s", err, out)
	}
}

func TestFeedbackWithoutTerminalNeedsMessageAndKind(t *testing.T) {
	f, out := fakeFeedbackIO("", false, nil)
	err := runFeedback(context.Background(), opts{args: []string{"feedback"}}, "https://x.example/v1/feedback", "t", f)
	if !errors.Is(err, errNotSent) || !strings.Contains(out.String(), "--yes --confirm CODE") {
		t.Fatalf("%v\n%s", err, out)
	}
	f, out = fakeFeedbackIO("", false, nil)
	err = runFeedback(context.Background(), opts{args: []string{"feedback", "hi"}, kind: "bug"}, "https://x.example/v1/feedback", "t", f)
	if !errors.Is(err, errNotSent) {
		t.Fatalf("sending without --yes --confirm must not happen: %v\n%s", err, out)
	}
}

func TestFeedbackOffWhenNoEndpoint(t *testing.T) {
	f, out := fakeFeedbackIO("", true, nil)
	err := runFeedback(context.Background(), opts{args: []string{"feedback"}}, "", "t", f)
	if !errors.Is(err, errNotSent) || !strings.Contains(out.String(), "feedback@basalt-os.org") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestFeedbackRefusalIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"ok":false,"error":"rate_limited"}`))
	}))
	defer srv.Close()
	// No e-mail (an empty line), then send.
	f, out := fakeFeedbackIO("\nyes\n", true, srv.Client())
	o := opts{args: []string{"feedback", "hi"}, kind: "bug", include: "none", includeSet: true}
	err := runFeedback(context.Background(), o, srv.URL+"/v1/feedback", "t", f)
	if !errors.Is(err, errNotSent) || !strings.Contains(out.String(), "too many reports from your network") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestFeedbackEdit(t *testing.T) {
	srv := newFeedbackServer(t)
	f, out := fakeFeedbackIO("\nedit\nyes\n", true, srv.Client())
	f.edit = func(path string) error {
		b, _ := os.ReadFile(path)
		b = bytes.Replace(b, []byte("first try"), []byte("second try"), 1)
		b = bytes.Replace(b, []byte(`"source": "cli"`), []byte(`"source": "web"`), 1)
		return os.WriteFile(path, b, 0o600)
	}
	o := opts{args: []string{"feedback", "first try"}, kind: "idea", include: "none", includeSet: true}
	if err := runFeedback(context.Background(), o, srv.endpoint(), "t", f); err != nil || len(srv.got) != 1 {
		t.Fatalf("%v\n%s", err, out)
	}
	var r feedback.Report
	_ = json.Unmarshal(srv.got[0], &r)
	if r.Message != "second try" || r.Source != "cli" {
		t.Fatalf("%+v", r)
	}
}
