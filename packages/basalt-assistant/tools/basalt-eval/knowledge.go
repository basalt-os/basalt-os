package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/knowledge"
)

// runKnowledgeVerify checks a knowledge index directory exactly as the
// assistant does when it loads it: the detached signature of
// manifest.json (manifest.json.sig) by the pinned key, then the SHA-256 of
// every file. scripts/sign-knowledge.sh runs it after signing.
//
//	basalt-eval knowledge-verify -dir DIR [-key FILE] [-signer "PRIMARY SUBKEY"]
func runKnowledgeVerify(args []string) error {
	fs := flag.NewFlagSet("knowledge-verify", flag.ExitOnError)
	dir := fs.String("dir", "", "index directory (manifest.json, manifest.json.sig, cases.jsonl, index.bin)")
	key := fs.String("key", knowledge.DefaultKeyFile, "OpenPGP certificate with the signing subkey")
	signer := fs.String("signer", "", "\"PRIMARY SUBKEY\" fingerprints to trust (default: the OpenBasalt knowledge subkey)")
	_ = fs.Parse(args)
	if *dir == "" {
		return fmt.Errorf("-dir is required")
	}
	tr := knowledge.OpenBasaltKnowledge
	if f := strings.Fields(*signer); len(f) == 2 {
		tr = knowledge.TrustAnchor{Primary: f[0], Signer: f[1]}
	} else if *signer != "" {
		return fmt.Errorf("-signer: two fingerprints")
	}
	ix, err := knowledge.OpenSigned(*dir, knowledge.Verifier{KeyFile: *key, Trust: tr})
	if err != nil {
		return err
	}
	fmt.Printf("ok: %s, dsl %s, signed by %s (primary %s) on %s\n", ix.Version(), ix.Manifest.DSL,
		ix.Signature.Signer, ix.Signature.Primary, ix.Signature.Created.Format("2006-01-02 15:04:05 UTC"))
	return nil
}
