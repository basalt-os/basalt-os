package egress

import (
	"strings"
	"testing"
)

func TestSlice(t *testing.T) {
	if got := SliceName("s-0123456789ab"); got != "basaltagent-0123456789ab.slice" {
		t.Fatal(got)
	}
	args := strings.Join(ScopeArgs("s-0123456789ab", "proxy"), " ")
	if args != "systemd-run --user --scope --quiet --collect --slice=basaltagent-0123456789ab.slice --unit=basaltagent-0123456789ab-proxy --" {
		t.Fatal(args)
	}
	base := "/user.slice/user-1000.slice/user@1000.service/basaltagent.slice/basaltagent-0123456789ab.slice"
	got, err := SessionSlice(base+"/basaltagent-0123456789ab-proxy.scope", "s-0123456789ab")
	if err != nil || got != base {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"/user.slice/user-1000.slice/user@1000.service/app.slice/x.scope", base + "/x.service", base} {
		if _, err := SessionSlice(bad, "s-0123456789ab"); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
