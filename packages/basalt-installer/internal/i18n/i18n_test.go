package i18n

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// mo builds a little-endian .mo catalog from pairs.
func mo(pairs [][2]string) []byte {
	n := len(pairs)
	hdr := 28
	origT, transT := hdr, hdr+8*n
	data := transT + 8*n
	b := make([]byte, data)
	le := binary.LittleEndian
	le.PutUint32(b[0:], 0x950412de)
	le.PutUint32(b[8:], uint32(n))
	le.PutUint32(b[12:], uint32(origT))
	le.PutUint32(b[16:], uint32(transT))
	for i, p := range pairs {
		for j, s := range p {
			table := origT
			if j == 1 {
				table = transT
			}
			le.PutUint32(b[table+8*i:], uint32(len(s)))
			le.PutUint32(b[table+8*i+4:], uint32(len(b)))
			b = append(b, append([]byte(s), 0)...)
		}
	}
	return b
}

func TestCatalogLookup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pt", "LC_MESSAGES")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, Domain+".mo"), mo([][2]string{{"", "Content-Type: text/plain; charset=UTF-8\n"}, {"Welcome", "Bem-vindo"}}), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LANGUAGE", "")
	t.Setenv("LC_ALL", "pt_BR.UTF-8")
	m := load(dir, languages())
	if m["Welcome"] != "Bem-vindo" {
		t.Fatalf("lookup through the language fallback: %v", m)
	}
	t.Setenv("LC_ALL", "C")
	if len(load(dir, languages())) != 0 {
		t.Fatal("C locale must use the reference text")
	}
}

func TestUntranslatedIsReference(t *testing.T) {
	LocaleDir = t.TempDir()
	if got := T("Nothing changes."); got != "Nothing changes." {
		t.Fatal(got)
	}
}
