package main

import (
	"math/rand"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/explain"
)

// Every generated target (the templates with random phrasings) passes the
// faithfulness check the humanize layer applies to a model: a model that
// learns the targets is never trained toward text the check rejects.
func TestRenderTargetsAreFaithful(t *testing.T) {
	for _, p := range []pools{trainPools, heldoutPools} {
		g := genCtx{r: rand.New(rand.NewSource(3)), p: p}
		for i := 0; i < 3000; i++ {
			f, acts := g.sample()
			txt := explain.WriteVariant(f, acts, g.r).Prose()
			if bad := explain.NewAllowed(f, acts).Check(txt, explain.DefaultMaxChars); len(bad) > 0 {
				t.Fatalf("%s/%s: %v\n%s", f.Kind, f.Cause, bad, txt)
			}
		}
	}
}
