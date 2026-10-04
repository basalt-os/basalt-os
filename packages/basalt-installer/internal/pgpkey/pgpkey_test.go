package pgpkey

import (
	"os"
	"testing"
)

func TestFingerprintOfThePinnedKey(t *testing.T) {
	data, err := os.ReadFile("../steps/RPM-GPG-KEY-tui-tools")
	if err != nil {
		t.Fatal(err)
	}
	fpr, err := Fingerprint(data)
	if err != nil {
		t.Fatal(err)
	}
	// Same value as `gpg --show-keys --with-colons` prints.
	if fpr != "767CFB337B01F32FFC073F3F389120B277E4FB44" {
		t.Fatalf("fingerprint %s", fpr)
	}
}

func TestRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "hello", "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nAAAA\n-----END PGP PUBLIC KEY BLOCK-----\n"} {
		if _, err := Fingerprint([]byte(in)); err == nil {
			t.Fatalf("no error for %q", in)
		}
	}
}
