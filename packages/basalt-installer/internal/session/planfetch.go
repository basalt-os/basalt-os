package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/i18n"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
)

// A plan sets root's SSH keys, password hashes and repositories, so where
// it comes from is a trust decision:
//
//   - a file on the live system or the media is read as is;
//   - an https URL is fetched with certificate checks, and redirects may
//     not leave https or the host the boot line names;
//   - a plain http URL is refused unless the boot line also carries
//     basalt.inst.plan.sha256=<hex>, the SHA-256 of the plan file.
//
// When the checksum is given (for a file, http or https), the plan is
// verified against it before it is parsed, and redirects are then allowed
// because the content is pinned.

// planMaxBytes is the largest plan accepted.
const planMaxBytes = 1 << 20

// planFetchTimeout bounds one attempt to fetch a plan over the network.
const planFetchTimeout = 20 * time.Second

// planTransport carries plan requests (nil: http.DefaultTransport). Tests
// set it to trust their own TLS server.
var planTransport http.RoundTripper

// planRefusedError is a plan that the integrity rules reject: retrying
// cannot help, and nothing from it is used.
type planRefusedError struct{ msg string }

func (e *planRefusedError) Error() string { return e.msg }

func refusePlan(format string, a ...any) error {
	return &planRefusedError{msg: fmt.Sprintf(format, a...)}
}

// isPlanRefused reports whether err is a refusal (not a network or
// parse error that a later attempt may not hit).
func isPlanRefused(err error) bool {
	var r *planRefusedError
	return errors.As(err, &r)
}

// parsePlanSHA256 decodes basalt.inst.plan.sha256 ("" means not given).
func parsePlanSHA256(v string) ([]byte, error) {
	if v == "" {
		return nil, nil
	}
	sum, err := hex.DecodeString(v)
	if err != nil || len(sum) != sha256.Size {
		return nil, refusePlan(i18n.T("basalt.inst.plan.sha256=%s is not a SHA-256 checksum (64 hexadecimal digits)"), v)
	}
	return sum, nil
}

// loadPlan reads a plan from a file or an http(s) URL (at most 1 MiB),
// following the rules above. sum is the basalt.inst.plan.sha256 value.
func loadPlan(ctx context.Context, src, sum string) (plan.Plan, error) {
	want, err := parsePlanSHA256(sum)
	if err != nil {
		return plan.Plan{}, err
	}
	if !isURL(src) {
		if want == nil {
			return plan.Load(src)
		}
		data, err := readLimited(func() (io.ReadCloser, error) { return os.Open(src) })
		if err != nil {
			return plan.Plan{}, err
		}
		if err := checkPlanSum(src, data, want); err != nil {
			return plan.Plan{}, err
		}
		return plan.Parse(data, filepath.Ext(src))
	}
	u, err := url.Parse(src)
	if err != nil {
		return plan.Plan{}, refusePlan(i18n.T("%s is not a valid URL: %v"), src, err)
	}
	if u.Scheme == "http" && want == nil {
		return plan.Plan{}, refusePlan("%s", i18n.T("a plan over plain http can be changed by anyone on the network: use an https URL, or add basalt.inst.plan.sha256=<SHA-256 of the plan file> to the boot line"))
	}
	ctx, cancel := context.WithTimeout(ctx, planFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return plan.Plan{}, err
	}
	client := &http.Client{Transport: planTransport, CheckRedirect: planRedirectPolicy(want != nil)}
	data, err := readLimited(func() (io.ReadCloser, error) {
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("HTTP %s", resp.Status)
		}
		return resp.Body, nil
	})
	if err != nil {
		return plan.Plan{}, err
	}
	if err := checkPlanSum(src, data, want); err != nil {
		return plan.Plan{}, err
	}
	return plan.Parse(data, path.Ext(u.Path))
}

// readLimited reads at most planMaxBytes from what open returns and
// refuses anything larger, instead of parsing a cut plan.
func readLimited(open func() (io.ReadCloser, error)) ([]byte, error) {
	r, err := open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, planMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > planMaxBytes {
		return nil, refusePlan("%s", i18n.T("the plan is larger than 1 MiB"))
	}
	return data, nil
}

// checkPlanSum compares the plan with the pinned checksum (none: nothing
// to check).
func checkPlanSum(src string, data, want []byte) error {
	if want == nil {
		return nil
	}
	if got := sha256.Sum256(data); !bytes.Equal(got[:], want) {
		return refusePlan(i18n.T("the SHA-256 of %s is %x, but basalt.inst.plan.sha256 is %x: the plan was changed or is not the one meant"), src, got, want)
	}
	return nil
}

// planRedirectPolicy allows redirects freely when the plan's checksum is
// pinned. Otherwise a redirect must stay on https and on the same host
// and port: a downgrade to http, or another host, could hand over a plan
// nobody checked.
func planRedirectPolicy(pinned bool) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if pinned {
			return nil
		}
		first := via[0].URL
		if req.URL.Scheme != "https" {
			return refusePlan(i18n.T("the server redirected the plan from %s to %s, which is not https: add basalt.inst.plan.sha256=<SHA-256 of the plan file> to the boot line to allow it"), first.Redacted(), req.URL.Redacted())
		}
		if !sameHostPort(first, req.URL) {
			return refusePlan(i18n.T("the server redirected the plan from %s to another host, %s: add basalt.inst.plan.sha256=<SHA-256 of the plan file> to the boot line to allow it"), first.Redacted(), req.URL.Redacted())
		}
		return nil
	}
}

// sameHostPort compares host names (case-insensitively, without a final
// dot) and ports (the scheme's default when none is given).
func sameHostPort(a, b *url.URL) bool {
	host := func(u *url.URL) string { return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".") }
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "http" {
			return "80"
		}
		return "443"
	}
	return net.JoinHostPort(host(a), port(a)) == net.JoinHostPort(host(b), port(b))
}

func isURL(s string) bool { return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") }
