#!/usr/bin/env python3
"""Turn lab captures into basalt-case/v1 lines (docs/eval-suite.md).

    eval/tools/capture-to-cases.py CAPTURE_DIR [PROPOSALS_JSON] > eval/cases/lab.jsonl

CAPTURE_DIR holds the files of scripts/lab/eval-capture.sh (one scenario
each, `basalt why --json` as root). PROPOSALS_JSON (optional) is a JSON
object {file name: proposal} of the proposals the daemon stored during the
same run; they are the confined daemon's view of the same events (fewer
probes) and become cases of source "lab-daemon".

The expected answers come from the scenario (the failure was caused on
purpose), never from the rules' output.
"""
import glob
import json
import os
import sys

RECORDED = "2026-10-03"

# Label of every AVC finding the diagnosers made, by scenario: (substring of
# the finding's subject, expected avc.class). The first match wins; a
# finding without a match takes the scenario's expected avc.class.
AVC_LABELS = {
    "nginx-shadow": [("shadow_t", "suspicious"), ("httpd_config_t", "unknown")],
    # DAC denial: the label checks of /bin and /var find nothing SELinux denies.
    "dac-permission": [("", "unknown")],
}

# DSL goal of the case (what a structured command or the translator emits).
def goal(subject):
    return "why(unit=%s)" % subject


def journal_facts(lines):
    """Same as decide.JournalFacts: last 8 lines, first line, 300 bytes."""
    if not lines:
        return None
    out = []
    for line in lines[-8:]:
        line = line.split("\n", 1)[0]
        out.append(line.encode()[:300].decode(errors="ignore"))
    return {"journal": out}


def on(features):
    return {k: True for k, v in sorted((features or {}).items()) if v}


def avc_questions(scenario, decisions, default):
    qs = []
    for d in decisions or []:
        q = d["question"]
        want = default
        for sub, label in AVC_LABELS.get(scenario, []):
            if sub in q.get("subject", ""):
                want = label
                break
        if want is None:
            continue
        qs.append({"id": "avc.class", "subject": q.get("subject", ""), "features": on(q.get("features")), "want": want})
    return qs


def from_capture(path):
    c = json.load(open(path))
    w = c.get("why") or {}
    exp = c["expected"]
    qs = []
    if "unit.cause" in exp:
        q = {"id": "unit.cause", "subject": c["subject"], "features": on(w.get("features")), "want": exp["unit.cause"]}
        f = journal_facts(w.get("journal"))
        if f:
            q["facts"] = f
        qs.append(q)
    qs += avc_questions(c["scenario"], w.get("avc_decisions"), exp.get("avc.class"))
    evidence = {k: w[k] for k in ("journal", "state", "config_check", "ports", "deps", "domain") if w.get(k)}
    if w.get("avcs"):
        evidence["avcs"] = [{"raw": a["group"]["avc"].get("raw", ""), "count": a["group"].get("count", 1),
                             "explanation": a.get("explanation", "")} for a in w["avcs"]]
    if evidence.get("state"):
        keep = ("ActiveState", "SubState", "Result", "ExecMainCode", "ExecMainStatus", "NRestarts", "Requires", "LoadState")
        evidence["state"] = {k: evidence["state"][k] for k in keep if k in evidence["state"]}
    return c["scenario"], {
        "schema": "basalt-case/v1",
        "id": "lab-" + c["scenario"],
        "kind": "unit_failure" if "unit.cause" in exp else "selinux_denial",
        "subject": c["subject"],
        "goal": goal(c["subject"]),
        "evidence": evidence,
        "questions": qs,
        "expected": {"diagnosis": c["diagnosis"], "cause": exp.get("unit.cause") or exp.get("avc.class"),
                     "actions": c["actions"], "proposal": "propose" if c["actions"] else "review"},
        "provenance": {"source": "lab", "ref": "scripts/lab/eval-capture.sh " + c["scenario"].replace("-", "_"),
                       "recorded": RECORDED, "method": "failure caused on purpose on a lab VM (Fedora 44, SELinux enforcing); "
                       "basalt why --json as root", "labels": "by construction"},
        "license": "Apache-2.0",
    }, w.get("journal")


# The daemon's proposals of the same run (and of the milestone 2a run):
# proposal file -> (scenario it came from, expected unit.cause, expected
# avc.class or None).
DAEMON = {
    "p-596a94.json": ("nginx-directive", "config_error", None),
    "p-747694.json": ("nginx-dependency", "dependency_failed", None),
    "p-f1af4d.json": ("nginx-port-conflict", "port_conflict", None),
    "p-2bdfb8.json": ("nginx-moved-dir", "selinux_denial", "mislabeled"),
    "p-83f8e4.json": ("nginx-dependency", "unknown", None),  # basalt-lab-fail1 (/bin/false)
    "p-07f6e4.json": ("exec-missing", "missing_file", None),
    "p-250907.json": ("crash-segv", "crashed", None),  # first report, before the core dump was processed
    "p-827976.json": ("crash-segv", "crashed", None),
    "p-afcc76.json": ("crash-abrt", "crashed", None),
    "p-73e7ed.json": ("crash-kill", "crashed", None),
    "p-bfc6fa.json": ("dep-custom", "unknown", None),  # basalt-lab-fail2: application error
    "p-d2e90c.json": ("dep-custom", "dependency_failed", None),
    "p-49b3e0.json": ("disk-full", "disk_full", None),
    "p-171df7.json": ("generic-error", "unknown", None),
    "p-f7c38b.json": ("dac-permission", "unknown", "unknown"),
    "p-ed3fdb.json": ("start-timeout", "unknown", None),
    "p-151dfa.json": ("app-config", "config_error", None),
    "p-20c712.json": ("app-missing-conf", "missing_file", None),
    "p-cfba7d.json": ("port-22", "port_conflict", None),
    # Milestone 2a scenarios (scripts/lab/assistant-test.sh), no journal kept.
    "p-0235db.json": ("m2a-config", "config_error", None),
    "p-97dfa5.json": ("m2a-selinux", "selinux_denial", "mislabeled"),
    "p-3a8682.json": ("m2a-port", "selinux_denial", "port"),
    # The confinement probe: the assistant's own domain denied writes.
    "p-15cce8.json": ("m2a-confine", "selinux_denial", "suspicious"),
    "p-3e5146.json": ("m2a-confine", "selinux_denial", "suspicious"),
}


def from_proposal(name, p, journals):
    if name not in DAEMON:
        return None
    scen, cause, avc = DAEMON[name]
    qs = []
    subject = ""
    for d in p.get("decisions") or []:
        q = d["question"]
        if q["id"] == "unit.cause":
            subject = q.get("subject", "")
            qq = {"id": "unit.cause", "subject": subject, "features": on(q.get("features")), "want": cause}
            f = journal_facts(journals.get(scen))
            if f:
                qq["facts"] = f
            qs.append(qq)
        elif q["id"] == "avc.class" and avc:
            qs.append({"id": "avc.class", "subject": q.get("subject", ""), "features": on(q.get("features")), "want": avc})
        elif q["id"] == "disk.cause":
            subject = q.get("subject", "/")
            qs.append({"id": "disk.cause", "subject": subject, "features": on(q.get("features")), "want": "snapshots"})
    if not qs:
        return None
    return {
        "schema": "basalt-case/v1",
        "id": "lab-daemon-" + name.replace(".json", ""),
        "kind": "unit_failure",
        "subject": subject,
        "goal": goal(subject),
        "evidence": {"journal": journals.get(scen) or []},
        "questions": qs,
        "expected": {"diagnosis": p.get("title", ""), "cause": cause, "actions": p.get("actions") or [],
                     "proposal": "propose" if p.get("actions") else "review"},
        "provenance": {"source": "lab-daemon", "ref": scen, "recorded": RECORDED,
                       "method": "proposal stored by the confined daemon (basalt-assistantd) for the same event; "
                       "journal lines borrowed from the root capture of the scenario when there is one",
                       "labels": "by construction"},
        "license": "Apache-2.0",
    }


# Milestone 2a cases with no stored proposal: features as the diagnosers
# set them for these events (internal/diag/dnf.go, disk.go), outcome from
# the milestone 2a lab run.
M2A = [
    ("m2a-dnf-postfail", "dnf.next", "rollback", "dnf -y install basalt-lab-postfail-1-1.noarch.rpm",
     ["scriptlet_failed", "post_scriptlet_failed", "rpmdb_changed"],
     ["WARNING [rpm] %post(basalt-lab-postfail-1-1.noarch) scriptlet failed, exit status 1"],
     "a %post scriptlet failed: the package is installed but not set up; roll back to the pre snapshot",
     [{"kind": "snapshot.rollback", "params": {"snapshot": "63"}}]),
    ("m2a-dnf-prefail", "dnf.next", "investigate", "dnf -y install basalt-lab-broken-1-1.noarch.rpm",
     ["scriptlet_failed", "pre_scriptlet_failed", "rpmdb_unchanged"],
     ["WARNING [rpm] %pre(basalt-lab-broken-1-1.noarch) scriptlet failed, exit status 1"],
     "a %pre scriptlet failed: nothing was installed; nothing to roll back", []),
    ("m2a-disk-daemon", "disk.cause", "snapshots", "/", ["disk_warn"], [],
     "89 % full because a snapshot holds an 11 GiB file; the confined daemon cannot measure snapshot space "
     "(evidence incomplete on purpose: the right answer is not visible to it)", []),
]


def m2a_cases():
    for cid, qid, want, subject, ft, lines, diag, actions in M2A:
        yield {
            "schema": "basalt-case/v1", "id": "lab-" + cid, "kind": "package_transaction" if qid == "dnf.next" else "disk_pressure",
            "subject": subject, "goal": ("transaction(failed)" if qid == "dnf.next" else "disk(mount=/)"),
            "evidence": {"dnf_log": lines} if lines else {},
            "questions": [{"id": qid, "subject": subject, "features": {k: True for k in ft}, "want": want}],
            "expected": {"diagnosis": diag, "cause": want, "actions": actions, "proposal": "propose" if actions else "review"},
            "provenance": {"source": "lab", "ref": "scripts/lab/assistant-test.sh (milestone 2a)", "recorded": RECORDED,
                           "method": "features as set by the diagnosers for the event, outcome from the lab run",
                           "labels": "by construction"},
            "license": "Apache-2.0",
        }


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    journals = {}
    out = []
    for path in sorted(glob.glob(os.path.join(sys.argv[1], "*.json"))):
        scen, case, journal = from_capture(path)
        journals[scen] = journal
        out.append(case)
    if len(sys.argv) > 2:
        props = json.load(open(sys.argv[2]))
        for name in sorted(props):
            c = from_proposal(name, props[name], journals)
            if c:
                out.append(c)
    out.extend(m2a_cases())
    for c in out:
        print(json.dumps(c, ensure_ascii=False, sort_keys=False))


if __name__ == "__main__":
    main()
