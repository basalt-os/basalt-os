package cli

import "testing"

func TestFedoraFromOSRelease(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"NAME=\"Basalt OS\"\nVERSION_ID=44.0\n", "44"},
		{"VERSION_ID=\"45.1\"\n", "45"},
		{"VERSION_ID=44\n", "44"},
		{"NAME=x\n", "44"},
		{"VERSION_ID=rawhide\n", "44"},
	} {
		if got := fedoraFromOSRelease(c.in); got != c.want {
			t.Errorf("fedoraFromOSRelease(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
