#!/usr/bin/env bash
# Check the security controls catalog against the repository.
#
#   scripts/ci/security-controls-check.sh            check (CI, via scripts/ci/lint.sh)
#   scripts/ci/security-controls-check.sh --render   regenerate docs/security/controls.md
#   scripts/ci/security-controls-check.sh --summary  print the number of controls by status
#
# The catalog is docs/security/controls.yaml. The check fails when:
#   - a control ID is malformed or used twice, or a required field is missing
#     or has an unknown value (status, verification type);
#   - an implemented or partial control names a path in this repository that
#     does not exist, or has no verification;
#   - an automated verification (test, ci, lab, suite) names a file that does
#     not exist or a text that does not appear in it;
#   - a manual verification has no "### <ID>" section in docs/security/audit-guide.md,
#     or an implemented or partial control has no such section at all;
#   - a mapping names an unknown control;
#   - docs/security/controls.md differs from what --render writes;
#   - prose under docs/security/ breaks the public writing rules (no em or
#     en dashes, no bold, no ellipsis; code blocks are not checked).
#
# References to other Basalt OS repositories ({repo: NAME, ...}) are checked
# when BASALT_EXTERNAL_ROOT points at a directory holding their checkouts
# (for example the parent directory of this checkout); otherwise they are
# listed as not checked, which is not a failure.
#
# Needs python3 with PyYAML (python3-pyyaml on Fedora).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODE=check
case "${1:-}" in
  "") ;;
  --render) MODE=render ;;
  --summary) MODE=summary ;;
  -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
  *) echo "usage: $0 [--render|--summary]" >&2; exit 2 ;;
esac

exec python3 - "$REPO_ROOT" "$MODE" "${BASALT_EXTERNAL_ROOT:-}" <<'PY'
import os
import re
import sys

try:
    import yaml
except ImportError:
    sys.exit("security-controls-check: python3 PyYAML is required (dnf install python3-pyyaml)")

root, mode, external_root = sys.argv[1], sys.argv[2], sys.argv[3]
sec = os.path.join(root, "docs", "security")
catalog_path = os.path.join(sec, "controls.yaml")
md_path = os.path.join(sec, "controls.md")
guide_path = os.path.join(sec, "audit-guide.md")

ID_RE = re.compile(r"^BSC-([A-Z]+)-(\d{3})$")
STATUSES = ("implemented", "partial", "planned", "retired")
VTYPES = ("test", "ci", "lab", "suite", "manual")
VTYPE_LABEL = {"test": "Test", "ci": "CI", "lab": "Lab", "suite": "Audit suite", "manual": "Manual"}
REQUIRED = ("id", "title", "requirement", "rationale", "status", "adr", "implemented_in", "verification")

errors = []
unchecked = []


def err(msg):
    errors.append(msg)


with open(catalog_path, encoding="utf-8") as f:
    data = yaml.safe_load(f)

areas = data.get("areas") or {}
controls = data.get("controls") or []
mappings = data.get("mappings") or []


def ext_dir(repo):
    if not external_root:
        return None
    d = os.path.join(external_root, repo)
    return d if os.path.isdir(d) else None


def path_exists(base, rel):
    rel = rel.rstrip("/")
    return os.path.exists(os.path.join(base, rel))


def text_in(base, rel, needle):
    p = os.path.join(base, rel)
    files = []
    if os.path.isdir(p):
        for dp, dns, fns in os.walk(p):
            dns[:] = [d for d in dns if d not in (".git", "node_modules", "vendor")]
            files += [os.path.join(dp, fn) for fn in fns]
    elif os.path.isfile(p):
        files = [p]
    for fp in files:
        try:
            with open(fp, encoding="utf-8", errors="replace") as fh:
                if needle in fh.read():
                    return True
        except OSError:
            pass
    return False


guide_ids = set()
if os.path.isfile(guide_path):
    with open(guide_path, encoding="utf-8") as fh:
        for line in fh:
            m = re.match(r"^### (BSC-[A-Z]+-\d{3})\s*$", line.rstrip("\n"))
            if m:
                guide_ids.add(m.group(1))
else:
    err("docs/security/audit-guide.md is missing")

seen = set()
for i, c in enumerate(controls):
    cid = c.get("id", f"<control #{i + 1}>")
    for k in REQUIRED:
        if k not in c:
            err(f"{cid}: missing field '{k}'")
    m = ID_RE.match(str(cid))
    if not m:
        err(f"{cid}: ID must look like BSC-AREA-NNN")
    elif m.group(1) not in areas:
        err(f"{cid}: unknown area {m.group(1)} (add it to 'areas')")
    if cid in seen:
        err(f"{cid}: duplicate ID")
    seen.add(cid)
    st = c.get("status")
    if st not in STATUSES:
        err(f"{cid}: status must be one of {', '.join(STATUSES)}, got {st!r}")
    for a in c.get("adr") or []:
        if not re.match(r"^\d{4}$", str(a)):
            err(f"{cid}: ADR reference {a!r} must be four digits")
    active = st in ("implemented", "partial")
    if st in ("partial", "planned") and not c.get("gap"):
        err(f"{cid}: a {st} control must say what is missing ('gap')")
    impl = c.get("implemented_in") or []
    if active and not impl:
        err(f"{cid}: an {st} control must name where it is implemented")
    for ref in impl:
        if isinstance(ref, dict):
            repo, rel = ref.get("repo"), ref.get("path")
            if not repo or not rel:
                err(f"{cid}: implemented_in entry needs repo and path: {ref}")
                continue
            base = ext_dir(repo)
            if base is None:
                unchecked.append(f"{cid}: {repo}:{rel}")
            elif active and not path_exists(base, rel):
                err(f"{cid}: {repo}:{rel} does not exist")
        elif active and not path_exists(root, str(ref)):
            err(f"{cid}: {ref} does not exist")
    ver = c.get("verification") or []
    if active and not ver:
        err(f"{cid}: an {st} control needs at least one verification")
    for v in ver:
        t = v.get("type") if isinstance(v, dict) else None
        if t not in VTYPES:
            err(f"{cid}: verification type must be one of {', '.join(VTYPES)}: {v}")
            continue
        if t == "manual":
            g = v.get("guide")
            if g != cid:
                err(f"{cid}: a manual verification must point at its own guide section (guide: {cid})")
            elif g not in guide_ids:
                err(f"{cid}: no '### {cid}' section in docs/security/audit-guide.md")
            continue
        rel, needle = v.get("file"), v.get("match")
        if not rel or not needle:
            err(f"{cid}: {t} verification needs file and match: {v}")
            continue
        repo = v.get("repo")
        base = root
        if repo:
            base = ext_dir(repo)
            if base is None:
                unchecked.append(f"{cid}: {repo}:{rel} ({needle})")
                continue
        if not path_exists(base, rel):
            err(f"{cid}: verification file {(repo + ':') if repo else ''}{rel} does not exist")
        elif not text_in(base, rel, needle):
            err(f"{cid}: {(repo + ':') if repo else ''}{rel} does not contain {needle!r}")
    if active and cid not in guide_ids:
        err(f"{cid}: an {st} control needs a '### {cid}' section in docs/security/audit-guide.md")

for mp in mappings:
    for it in mp.get("items") or []:
        for cid in it.get("controls") or []:
            if cid not in seen:
                err(f"mapping {mp.get('reference')} / {it.get('item')}: unknown control {cid}")

for g in sorted(guide_ids - seen):
    err(f"docs/security/audit-guide.md has a section for {g}, which is not in the catalog")


def fmt_ref(ref):
    if isinstance(ref, dict):
        return f"`{ref['path']}` in {ref['repo']}"
    return f"`{ref}`"


def fmt_ver(v):
    t = v["type"]
    if t == "manual":
        return f"Manual: audit guide, [{v['guide']}](audit-guide.md#{v['guide'].lower()})"
    where = f"`{v['file']}`" + (f" in {v['repo']}" if v.get("repo") else "")
    return f"{VTYPE_LABEL[t]}: {where}, `{v['match']}`"


def one_line(s):
    return " ".join(str(s).split())


def render():
    out = []
    w = out.append
    w("# Security controls catalog")
    w("")
    w("<!-- Generated from controls.yaml by scripts/ci/security-controls-check.sh --render. Do not edit by hand. -->")
    w("")
    w(f"Version {data.get('version')}, updated {data.get('updated')}. Each control has a")
    w("stable ID, a requirement, the reason for it, where it is enforced, how it")
    w("is verified and its status. Implemented means enforced in the shipped")
    w("packages and checked; partial means enforced for part of the scope or")
    w("without the full verification; planned means decided or proposed but not")
    w("built, so nothing should rely on it yet. How controls change:")
    w("[change-policy.md](change-policy.md). How to check them yourself:")
    w("[audit-guide.md](audit-guide.md).")
    w("")
    w("## Summary")
    w("")
    w("| Area | Implemented | Partial | Planned | Total |")
    w("|---|---|---|---|---|")
    tot = {"implemented": 0, "partial": 0, "planned": 0, "all": 0}
    for code, name in areas.items():
        cs = [c for c in controls if c["id"].startswith(f"BSC-{code}-") and c["status"] != "retired"]
        n = {s: sum(1 for c in cs if c["status"] == s) for s in ("implemented", "partial", "planned")}
        for s in n:
            tot[s] += n[s]
        tot["all"] += len(cs)
        w(f"| {code}: {name} | {n['implemented']} | {n['partial']} | {n['planned']} | {len(cs)} |")
    w(f"| All | {tot['implemented']} | {tot['partial']} | {tot['planned']} | {tot['all']} |")
    w("")
    for code, name in areas.items():
        cs = [c for c in controls if c["id"].startswith(f"BSC-{code}-")]
        if not cs:
            continue
        w(f"## {code}: {name}")
        w("")
        for c in cs:
            w(f"### {c['id']}")
            w("")
            w(f"{c['title']}.")
            w("")
            adrs = ", ".join(f"ADR {a}" for a in c.get("adr") or []) or "none"
            w(f"Status: {c['status']}. Decided in: {adrs}.")
            w("")
            w(f"Requirement: {one_line(c['requirement'])}")
            w("")
            w(f"Rationale: {one_line(c['rationale'])}")
            w("")
            impl = c.get("implemented_in") or []
            if impl:
                w("Implemented in: " + ", ".join(fmt_ref(r) for r in impl) + ".")
                w("")
            ver = c.get("verification") or []
            if ver:
                w("Verified by:")
                w("")
                for v in ver:
                    w(f"- {fmt_ver(v)}")
                w("")
            if c.get("gap"):
                w(f"Gap: {one_line(c['gap'])}")
                w("")
            if c.get("references"):
                w("References: " + "; ".join(c["references"]) + ".")
                w("")
    if mappings:
        w("## Mapping to known references")
        w("")
        w("These tables help readers who know the references find the related")
        w("controls. They are not a claim of compliance or certification.")
        w("")
        for mp in mappings:
            w(f"### {mp['reference']}")
            w("")
            w("| Item | Controls |")
            w("|---|---|")
            for it in mp.get("items") or []:
                w(f"| {it['item']} | {', '.join(it['controls'])} |")
            w("")
    return "\n".join(out).rstrip("\n") + "\n"


if mode == "summary":
    counts = {}
    for c in controls:
        counts[c.get("status")] = counts.get(c.get("status"), 0) + 1
    print(f"{len(controls)} controls: " + ", ".join(f"{counts.get(s, 0)} {s}" for s in STATUSES if counts.get(s)))
    sys.exit(1 if errors else 0)

if mode == "render":
    if errors:
        for e in errors:
            print(f"error: {e}", file=sys.stderr)
        sys.exit("security-controls-check: fix the catalog before rendering")
    with open(md_path, "w", encoding="utf-8") as fh:
        fh.write(render())
    print(f"wrote {os.path.relpath(md_path, root)}")
    sys.exit(0)

# check mode: controls.md must be current, pages must follow the writing rules
if os.path.isfile(md_path):
    with open(md_path, encoding="utf-8") as fh:
        if fh.read() != render():
            err("docs/security/controls.md is stale: run scripts/ci/security-controls-check.sh --render")
else:
    err("docs/security/controls.md is missing: run scripts/ci/security-controls-check.sh --render")

STYLE = [("—", "em dash"), ("–", "en dash"), ("…", "ellipsis"), ("**", "bold"), ("...", "ellipsis")]
for fn in sorted(os.listdir(sec)):
    if not fn.endswith((".md", ".yaml")):
        continue
    with open(os.path.join(sec, fn), encoding="utf-8") as fh:
        fenced = False
        for n, line in enumerate(fh, 1):
            # The rules apply to prose; commands in code blocks may need "..." (go test ./...).
            if line.lstrip().startswith("```"):
                fenced = not fenced
                continue
            if fenced:
                continue
            for s, what in STYLE:
                if s in line:
                    err(f"docs/security/{fn}:{n}: {what} (public writing rules)")

for u in unchecked:
    print(f"not checked (set BASALT_EXTERNAL_ROOT): {u}", file=sys.stderr)
if errors:
    for e in errors:
        print(f"error: {e}", file=sys.stderr)
    sys.exit(f"security-controls-check: {len(errors)} problem(s) found")
counts = {}
for c in controls:
    counts[c["status"]] = counts.get(c["status"], 0) + 1
print(f"security controls: {len(controls)} checked (" + ", ".join(f"{counts.get(s, 0)} {s}" for s in STATUSES if counts.get(s)) + ")")
PY
