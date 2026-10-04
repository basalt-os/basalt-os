package nft

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestAddScript(t *testing.T) {
	s := Session{Name: "s_b4896c08ff03", Cgroup: "/user.slice/user-1000.slice/user@1000.service/basaltagent.slice/basaltagent-b4896c08ff03.slice",
		DNSPort: 47201, Loopback: false, LogGroup: 47}
	script, err := AddScript(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`socket cgroupv2 level 5 "user.slice/user-1000.slice/user@1000.service/basaltagent.slice/basaltagent-b4896c08ff03.slice" jump s_b4896c08ff03 comment "s_b4896c08ff03"`,
		`ip daddr 127.0.0.53 meta l4proto { tcp, udp } th dport 53 redirect to :47201`,
		`ip daddr . th dport @s_b4896c08ff03_v4 accept`,
		`log group 47 prefix "basalt-egress s_b4896c08ff03 drop"`,
		`log group 47 prefix "basalt-egress s_b4896c08ff03 dns"`,
		"add rule inet basalt_egress s_b4896c08ff03 drop\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if strings.Contains(script, "1024-65535") {
		t.Error("loopback rule present although Loopback is false")
	}
	// The final rule of the session chain must be the drop.
	lines := strings.Split(strings.TrimSpace(script), "\n")
	last := ""
	for _, l := range lines {
		if strings.HasPrefix(l, "add rule inet basalt_egress s_b4896c08ff03 ") {
			last = l
		}
	}
	if !strings.HasSuffix(last, " drop") || strings.Contains(last, "dport") {
		t.Errorf("last session rule %q is not the catch-all drop", last)
	}
	for _, bad := range []Session{
		{Name: "bad name", Cgroup: "/a/b", DNSPort: 1},
		{Name: "s_x", Cgroup: `/a/"b`, DNSPort: 1},
		{Name: "s_x", Cgroup: "relative", DNSPort: 1},
		{Name: "s_x", Cgroup: "/a/b", DNSPort: 0},
	} {
		if _, err := AddScript(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestElementsScript(t *testing.T) {
	sc, err := ElementsScript("s_ab", []Element{{IP: net.ParseIP("1.2.3.4"), Port: 443, Timeout: 90 * time.Second},
		{IP: net.ParseIP("2001:db8::1"), Port: 80, Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"add element inet basalt_egress s_ab_v4 { 1.2.3.4 . 443 timeout 90s }",
		"delete element inet basalt_egress s_ab_v4 { 1.2.3.4 . 443 }", "s_ab_v6 { 2001:db8::1 . 80 timeout 1s }"} {
		if !strings.Contains(sc, want) {
			t.Errorf("missing %q in\n%s", want, sc)
		}
	}
}

func TestPrefix(t *testing.T) {
	n, k, ok := ParsePrefix(LogPrefix("s_abc", "drop") + "\x00")
	if !ok || n != "s_abc" || k != "drop" {
		t.Fatalf("%q %q %v", n, k, ok)
	}
	if _, _, ok := ParsePrefix("other prefix"); ok {
		t.Fatal("foreign prefix accepted")
	}
}

func TestParseState(t *testing.T) {
	js := `{"nftables":[{"metainfo":{}},{"table":{"family":"inet","name":"basalt_egress"}},
{"chain":{"family":"inet","table":"basalt_egress","name":"output"}},
{"chain":{"family":"inet","table":"basalt_egress","name":"s_abc"}},
{"chain":{"family":"inet","table":"basalt_egress","name":"s_abc_nat"}},
{"set":{"family":"inet","table":"basalt_egress","name":"s_abc_v4"}},
{"rule":{"family":"inet","table":"basalt_egress","chain":"output","handle":7,"comment":"s_abc"}},
{"rule":{"family":"inet","table":"basalt_egress","chain":"output_nat","handle":9,"comment":"s_abc"}},
{"rule":{"family":"inet","table":"basalt_egress","chain":"s_abc","handle":11}}]}`
	st, err := ParseState([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Exists || !st.Chains["s_abc"] || !st.Sets["s_abc_v4"] || len(st.Jumps["s_abc"]) != 2 {
		t.Fatalf("%+v", st)
	}
	if got := st.Sessions(); len(got) != 1 || got[0] != "s_abc" {
		t.Fatalf("sessions %v", got)
	}
}

type recRunner struct{ scripts []string }

func (r *recRunner) Apply(s string) error { r.scripts = append(r.scripts, s); return nil }
func (r *recRunner) JSON(args ...string) ([]byte, error) {
	return []byte(`{"nftables":[{"table":{"name":"basalt_egress"}},{"chain":{"name":"s_abc"}},{"chain":{"name":"s_abc_nat"}},
{"set":{"name":"s_abc_v4"}},{"set":{"name":"s_abc_v6"}},{"rule":{"chain":"output","handle":7,"comment":"s_abc"}}]}`), nil
}

func TestRemove(t *testing.T) {
	r := &recRunner{}
	if err := (Manager{R: r}).Remove("s_abc"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.scripts, "")
	for _, want := range []string{"delete rule inet basalt_egress output handle 7", "delete chain inet basalt_egress s_abc\n",
		"delete chain inet basalt_egress s_abc_nat", "delete set inet basalt_egress s_abc_v4", "delete set inet basalt_egress s_abc_v6"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Index(got, "delete rule") > strings.Index(got, "delete chain") {
		t.Error("jumps must be deleted before the chains they target")
	}
}
