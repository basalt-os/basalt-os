#!/usr/bin/env python3
"""Generate labeled decision cases (basalt-case/v1.1) from templates.

    eval/tools/generate-cases.py [--seed N] | basalt-eval derive > eval/cases/generated.jsonl

The lab captures (eval/cases/lab.jsonl) are few; these cases widen the
suite with the same kinds of events: journal messages in the shape real
programs print them (taken from the lab captures and the programs'
sources), the structural findings the diagnosers would add (unit state,
denials for the unit's domain, label checks, config checkers, port owners)
and deliberate noise: probes the confined daemon cannot run, evidence that
points the wrong way, failures whose true cause has no option of its own.
Every label is known by construction. The journal features and the facts
a model sees are filled in by `basalt-eval derive`, which runs the same
code as `basalt why` (internal/diag, internal/decide).

Deterministic for a given seed; generator version in every case.

generate-cases/2 (same seed, same random draws as /1): the second large
holder of a disk case is 1 GiB plus up to 500 MiB, as intended; /1 shifted
the whole sum (operator precedence), which made it larger than the holder
named by the label.

generate-cases/3 (same seed, same random draws as /2): a DAC permission
case carries dac_denied, which the diagnosers set (root view and daemon)
since they check the mode and owner of the denied path against the unit's
User= (before, they only knew that a label check found nothing).
"""
import argparse
import json
import random

VERSION = "generate-cases/3"
SCHEMA = "basalt-case/v1.1"

SERVICES = ["nginx", "httpd", "haproxy", "postfix", "named", "postgresql", "mariadb", "redis", "grafana-server",
            "myapp", "backup-agent", "billing-worker", "chronyd", "smb", "dovecot", "caddy", "gitea", "prometheus"]
DIRS = ["/srv/www", "/srv/app/uploads", "/data/logs", "/opt/app/cache", "/var/www/shop", "/srv/media", "/home/app/public"]
PORTS = [80, 443, 8080, 8443, 3000, 5432, 6379, 9090, 8085, 8181, 25, 53]


def ts(r):
    return "%02d:%02d:%02d" % (r.randint(0, 23), r.randint(0, 59), r.randint(0, 59))


def failed_lines(r, unit, result="exit-code", status="1/FAILURE"):
    t = ts(r)
    return [
        "%s %s: Main process exited, code=exited, status=%s" % (t, unit, status),
        "%s %s: Failed with result '%s'." % (t, unit, result),
        "%s Failed to start %s." % (t, unit),
    ]


def unit_case(r, n, cause, svc, lines, features, diagnosis, notes=""):
    unit = svc + ".service"
    ft = dict(features)
    return {
        "schema": SCHEMA,
        "id": "gen-unit-%03d-%s" % (n, cause),
        "kind": "unit_failure",
        "subject": unit,
        "goal": "why(unit=%s)" % unit,
        "evidence": {"journal": lines},
        "questions": [{"id": "unit.cause", "subject": unit, "features": ft, "want": cause, "derive": True}],
        "expected": {"diagnosis": diagnosis, "cause": cause, "actions": [], "proposal": "review"},
        "provenance": {"source": "generated", "ref": VERSION, "recorded": "2026-10-03",
                       "method": "template; structural findings as the diagnosers set them; journal features derived by basalt why's code",
                       "labels": "by construction", "notes": notes},
        "license": "Apache-2.0",
    }


def gen_unit(r, n):
    cause = ["config_error", "selinux_denial", "port_conflict", "dependency_failed", "disk_full",
             "missing_file", "crashed", "unknown"][n % 8]
    svc = r.choice(SERVICES)
    unit = svc + ".service"
    confined = r.random() < 0.35  # the daemon's view: no config checker, no port owners
    ft = {"unit_failed": True}
    notes = "confined view" if confined else "root view"
    t = ts(r)
    if cause == "config_error":
        f = "/etc/%s/%s.conf" % (svc, svc)
        line = r.randint(3, 220)
        msg = r.choice([
            '%s %s: [emerg] unknown directive "proxy_bufer_size" in %s:%d' % (t, svc, f, line),
            "%s %s: %s:%d: unknown option 'allow-recursoin'" % (t, svc, f, line),
            "%s %s: [ALERT] parsing [%s:%d] : unknown keyword 'timeout_connect' in 'defaults' section" % (t, svc, f, line),
            "%s %s: fatal: %s, line %d: missing '=' after attribute name: \"smtpd_tls_cert\"" % (t, svc, f, line),
            "%s %s: error: invalid value 'yess' for option 'enabled' in %s:%d" % (t, svc, f, line),
            "%s %s: Syntax error on line %d of %s: Invalid command 'SSLEngin'" % (t, svc, line, f),
        ])
        lines = [msg] + failed_lines(r, unit)
        if not confined:
            ft["config_check_failed" if r.random() < 0.8 else "config_check_runtime"] = True
        return unit_case(r, n, cause, svc, lines, ft, "%s rejects its configuration (%s line %d)" % (svc, f, line), notes)
    if cause == "selinux_denial":
        d = r.choice(DIRS)
        lines = ['%s %s: open() "%s/access.log" failed (13: Permission denied)' % (t, svc, d)] + failed_lines(r, unit)
        how = r.random()
        if how < 0.5:
            ft["avc_for_domain"] = True
            notes += "; AVC logged"
        elif how < 0.85 and not confined:
            ft["path_label_problem"] = True
            if r.random() < 0.6:
                ft["path_mislabeled"] = True
            notes += "; dontaudit, found by the label check"
        else:
            notes += "; hidden denial, no label check: only 'Permission denied' (hard)"
        if not confined:
            ft["config_check_passed"] = True
        return unit_case(r, n, cause, svc, lines, ft, "SELinux denies %s access to %s" % (svc, d), notes)
    if cause == "port_conflict":
        p = r.choice(PORTS)
        msg = r.choice([
            "%s %s: [emerg] bind() to 0.0.0.0:%d failed (98: Address already in use)" % (t, svc, p),
            "%s %s: listen tcp 0.0.0.0:%d: bind: address already in use" % (t, svc, p),
            "%s %s: could not bind IPv4 address \"0.0.0.0\": Address already in use" % (t, svc),
            "%s %s: Error: EADDRINUSE: address already in use :::%d" % (t, svc, p),
        ])
        lines = [msg] + failed_lines(r, unit)
        if not confined and r.random() < 0.7:
            ft["port_owner_found"] = True
        return unit_case(r, n, cause, svc, lines, ft, "port %d is used by another process" % p, notes)
    if cause == "dependency_failed":
        dep = r.choice(["network-online.target", "postgresql.service", "var-lib-data.mount", "redis.service", "vault-agent.service"])
        lines = ["%s Dependency failed for %s." % (t, unit), "%s %s: Job %s/start failed with result 'dependency'." % (t, unit, unit)]
        ft.pop("unit_failed")
        return unit_case(r, n, cause, svc, lines, ft, "%s, required by %s, failed" % (dep, unit), notes)
    if cause == "disk_full":
        msg = r.choice([
            '%s %s: FATAL:  could not write to file "pg_wal/xlogtemp.%d": No space left on device' % (t, svc, r.randint(100, 9999)),
            "%s %s: write error: No space left on device" % (t, svc),
            "%s %s: Failed to write PID file: No space left on device" % (t, svc),
            "%s %s: error: cannot save snapshot: write /var/lib/%s/dump.rdb: no space left on device" % (t, svc, svc),
        ])
        lines = [msg] + failed_lines(r, unit)
        if r.random() < 0.5:
            ft["disk_full"] = True
        return unit_case(r, n, cause, svc, lines, ft, "a file system %s writes to is full" % svc, notes)
    if cause == "missing_file":
        msg = r.choice([
            "%s %s: Failed to locate executable /usr/local/bin/%s: No such file or directory" % (t, unit, svc),
            "%s %s: Failed at step EXEC spawning /opt/%s/bin/run: No such file or directory" % (t, unit, svc),
            "%s %s: cat: /etc/%s/secret.key: No such file or directory" % (t, svc, svc),
            "%s %s: error: open /var/lib/%s/tls/server.pem: no such file or directory" % (t, svc, svc),
            "%s %s: can't open '/etc/%s/env': No such file or directory" % (t, svc, svc),
        ])
        status = "203/EXEC" if "EXEC" in msg or "executable" in msg else "1/FAILURE"
        lines = [msg] + failed_lines(r, unit, status=status)
        return unit_case(r, n, cause, svc, lines, ft, "a file %s needs does not exist" % svc, notes)
    if cause == "crashed":
        kind = r.choice(["segv", "abrt", "kill", "oom"])
        if kind == "oom":
            lines = ["%s %s: A process of this unit has been killed by the OOM killer." % (t, unit),
                     "%s %s: Main process exited, code=killed, status=9/KILL" % (t, unit),
                     "%s %s: Failed with result 'oom-kill'." % (t, unit)]
            notes += "; OOM kill (result oom-kill, not in the signal list)"
        else:
            sig = {"segv": "11/SEGV", "abrt": "6/ABRT", "kill": "9/KILL"}[kind]
            code = "dumped" if kind != "kill" else "killed"
            lines = ["%s %s: Main process exited, code=%s, status=%s" % (t, unit, code, sig),
                     "%s %s: Failed with result '%s'." % (t, unit, "core-dump" if code == "dumped" else "signal")]
            ft["signal_or_core"] = True
        return unit_case(r, n, cause, svc, lines, ft, "%s was killed (%s)" % (svc, kind), notes)
    # unknown: application errors, timeouts, DAC permission problems
    kind = r.choice(["app", "timeout", "dac", "exit"])
    if kind == "app":
        lines = ["%s %s: error: upstream %s returned HTTP 503, giving up" % (t, svc, r.choice(["auth", "license", "api"]))] + failed_lines(r, unit)
    elif kind == "timeout":
        lines = ["%s %s: start operation timed out. Terminating." % (t, unit), "%s %s: Failed with result 'timeout'." % (t, unit)]
    elif kind == "dac":
        lines = ["%s %s: /var/lib/%s/state: Permission denied" % (t, svc, svc)] + failed_lines(r, unit)
        # Both views check the mode and owner (stat, id): the daemon may.
        ft["dac_denied"] = True
        notes += "; DAC (file mode and owner checked against User=)"
    else:
        lines = failed_lines(r, unit)
    return unit_case(r, n, "unknown", svc, lines, ft, "no known cause class (%s)" % kind, notes)


AVC_TEMPLATES = {
    "mislabeled": [({"default_differs": True, "default_allowed": True, "has_path": True}, "httpd_t|admin_home_t|dir|write|%s"),
                   ({"default_differs": True, "default_allowed": True, "has_path": True, "no_avc": True}, "httpd_t|user_home_t|file|read|%s/index.html"),
                   ({"default_differs": True, "default_allowed": True, "has_path": True}, "postfix_master_t|tmp_t|file|read|/etc/postfix/main.cf")],
    "missing_fcontext": [({"generic_target": True, "candidate_type": True, "has_path": True}, "httpd_t|default_t|dir|search|%s"),
                         ({"generic_target": True, "candidate_type": True, "has_path": True, "no_avc": True}, "httpd_t|default_t|dir|add_name|%s")],
    "port": [({"port_case": True, "port_type_found": True}, "httpd_t|unreserved_port_t|tcp_socket|name_bind|%d"),
             ({"port_case": True, "port_type_found": True, "port_owned_by_other": True}, "httpd_t|intermapper_port_t|tcp_socket|name_bind|%d")],
    "boolean": [({"boolean_off": True}, "httpd_t|http_cache_port_t|tcp_socket|name_connect|%d"),
                ({"boolean_off": True, "has_path": True}, "httpd_t|user_home_dir_t|dir|search|/home/app")],
    "suspicious": [({"sensitive_target": True, "has_path": True}, "httpd_t|shadow_t|file|read|/etc/shadow"),
                   ({"sensitive_target": True, "default_differs": True, "default_allowed": True, "has_path": True}, "nginx_t|ssh_home_t|file|read|/root/.ssh/id_ed25519"),
                   ({"sensitive_target": True}, "postfix_local_t|security_t|security|load_policy|")],
    "unknown": [({"has_path": True, "no_avc": True}, "shell_t|bin_t|dir|search|/bin"),
                ({}, "init_t|unlabeled_t|file|getattr|"),
                ({"candidate_type": True, "has_path": True, "no_avc": True}, "httpd_t|httpd_config_t|file|append|/etc/nginx/nginx.conf"),
                ({"permissive": True, "has_path": True}, "myapp_t|var_lib_t|file|write|/var/lib/myapp/db")],
}


def gen_avc(r, n):
    cls = list(AVC_TEMPLATES)[n % len(AVC_TEMPLATES)]
    ft, subj = r.choice(AVC_TEMPLATES[cls])
    if "%d" in subj:
        subj = subj % r.choice(PORTS)
    elif "%s" in subj:
        subj = subj % r.choice(DIRS)
    return {
        "schema": SCHEMA, "id": "gen-avc-%03d-%s" % (n, cls), "kind": "selinux_denial",
        "subject": subj, "goal": "fix(selinux)", "evidence": {"denial": subj},
        "questions": [{"id": "avc.class", "subject": subj, "features": dict(ft), "want": cls}],
        "expected": {"diagnosis": "denial of class %s" % cls, "cause": cls, "actions": [], "proposal": "review"},
        "provenance": {"source": "generated", "ref": VERSION, "recorded": "2026-10-03",
                       "method": "feature pattern of the SELinux diagnoser for this class", "labels": "by construction", "notes": ""},
        "license": "Apache-2.0",
    }


DNF = [("rollback", {"pre_without_post": True}, "transaction interrupted (pre snapshot without post)"),
       ("rollback", {"scriptlet_failed": True, "post_scriptlet_failed": True, "rpmdb_changed": True}, "%post failed, package installed but not set up"),
       ("rollback", {"rpmdb_changed": True}, "failed after changing packages"),
       ("rollback", {"scriptlet_failed": True, "rpmdb_changed": True}, "a scriptlet failed after packages changed"),
       ("investigate", {"scriptlet_failed": True, "pre_scriptlet_failed": True, "rpmdb_unchanged": True}, "%pre failed, nothing installed"),
       ("investigate", {"rpmdb_unchanged": True}, "failed before changing anything"),
       ("investigate", {}, "failure without evidence of changes")]


def gen_dnf(r, n):
    want, ft, diag = DNF[n % len(DNF)]
    pkg = r.choice(["httpd-2.4.66-1", "kernel-7.2.9-200", "postgresql-server-18.1-1", "myapp-3.2.0-1", "selinux-policy-44.12-1"])
    subj = "dnf -y upgrade %s" % pkg.rsplit("-", 2)[0]
    return {
        "schema": SCHEMA, "id": "gen-dnf-%03d-%s" % (n, want), "kind": "package_transaction",
        "subject": subj, "goal": "transaction(failed)", "evidence": {"transaction": subj},
        "questions": [{"id": "dnf.next", "subject": subj, "features": dict(ft), "want": want}],
        "expected": {"diagnosis": diag, "cause": want, "actions": [], "proposal": "propose" if want == "rollback" else "review"},
        "provenance": {"source": "generated", "ref": VERSION, "recorded": "2026-10-03",
                       "method": "feature pattern of the package transaction diagnoser", "labels": "by construction", "notes": ""},
        "license": "Apache-2.0",
    }


def gen_disk(r, n):
    want = ["snapshots", "journal", "package_cache", "other_data"][n % 4]
    gib = 1 << 30
    sizes = {"snapshots_bytes": r.randint(1, 6) * gib // 4, "journal_bytes": r.randint(50, 400) << 20,
             "package_cache_bytes": r.randint(20, 300) << 20}
    ft = {"disk_warn": True} if r.random() < 0.7 else {"disk_crit": True}
    notes = ""
    if want == "snapshots":
        sizes["snapshots_bytes"] = r.randint(3, 12) * gib
        ft["snapshots_large"] = True
    elif want == "journal":
        sizes["journal_bytes"] = r.randint(2, 6) * gib
        ft["journal_large"] = True
    elif want == "package_cache":
        sizes["package_cache_bytes"] = r.randint(2, 5) * gib
        ft["cache_large"] = True
    if want != "other_data" and r.random() < 0.3:
        # Two thresholds crossed: the larger holder is the answer; the facts
        # say which, the features do not.
        other = r.choice([k for k in ("snapshots_large", "journal_large", "cache_large") if not ft.get(k)])
        ft[other] = True
        key = {"snapshots_large": "snapshots_bytes", "journal_large": "journal_bytes", "cache_large": "package_cache_bytes"}[other]
        sizes[key] = gib + (r.randint(0, 500) << 20)  # parentheses: << binds looser than +
        notes = "two large holders; the sizes decide"
    return {
        "schema": SCHEMA, "id": "gen-disk-%03d-%s" % (n, want), "kind": "disk_pressure",
        "subject": "/", "goal": "disk(mount=/)", "evidence": {"usage": sizes},
        "questions": [{"id": "disk.cause", "subject": "/", "features": ft, "facts": sizes, "want": want}],
        "expected": {"diagnosis": "%s holds the space" % want, "cause": want, "actions": [], "proposal": "review"},
        "provenance": {"source": "generated", "ref": VERSION, "recorded": "2026-10-03",
                       "method": "feature pattern of the disk diagnoser plus sizes", "labels": "by construction", "notes": notes},
        "license": "Apache-2.0",
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seed", type=int, default=20261003)
    ap.add_argument("--unit", type=int, default=96)
    ap.add_argument("--avc", type=int, default=48)
    ap.add_argument("--dnf", type=int, default=21)
    ap.add_argument("--disk", type=int, default=24)
    a = ap.parse_args()
    r = random.Random(a.seed)
    for gen, count in ((gen_unit, a.unit), (gen_avc, a.avc), (gen_dnf, a.dnf), (gen_disk, a.disk)):
        for n in range(count):
            print(json.dumps(gen(r, n), ensure_ascii=False))


if __name__ == "__main__":
    main()
