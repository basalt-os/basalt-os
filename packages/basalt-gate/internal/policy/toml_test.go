package policy

import (
	"reflect"
	"testing"
)

func TestParseTOML(t *testing.T) {
	m, err := ParseTOML(`
# a comment
[[rule]]
id = "r-1"   # trailing comment
n = 42
neg = -3
yes = true
no = false
list = [
  "a",
  'b',   # literal
]
inline = { path_beneath = ["~/x"], k = 1, e = {} }
esc = "q\"\\\né"

[[rule]]
id = "r-2"
`)
	if err != nil {
		t.Fatal(err)
	}
	rules := m["rule"].([]any)
	if len(rules) != 2 {
		t.Fatalf("%v", m)
	}
	r := rules[0].(map[string]any)
	want := map[string]any{"id": "r-1", "n": int64(42), "neg": int64(-3), "yes": true, "no": false,
		"list": []any{"a", "b"}, "inline": map[string]any{"path_beneath": []any{"~/x"}, "k": int64(1), "e": map[string]any{}},
		"esc": "q\"\\\né"}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("got  %#v\nwant %#v", r, want)
	}
}

func TestParseTOMLErrors(t *testing.T) {
	for _, src := range []string{
		"a.b = 1",
		"x = 1.5",
		"x = 1979-05-27",
		`x = """multi"""`,
		"x = \"unterminated",
		"x = 1\nx = 2",
		"[t]\n[t]",
		"x = [1, 2",
		"x = { a = 1",
		"x = 1 y = 2",
		"= 1",
		`x = "\q"`,
	} {
		if _, err := ParseTOML(src); err == nil {
			t.Errorf("accepted %q", src)
		}
	}
}
