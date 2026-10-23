#!/usr/bin/env python3
# Copyright (c) 2026 The Ycash developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or https://www.opensource.org/licenses/mit-license.php .
"""The PR gates of .github/workflows/secops.yml that compare a PR's head with its base (SECURITY-CI.md).

  contrib/secops/new-findings.py govulncheck BASE.json HEAD.json
      BASE/HEAD are `govulncheck -format json ./...` streams. Exit 1 when HEAD has a vulnerability the code
      calls (a symbol-level finding) that BASE does not: the PR adds a reachable vulnerability, through a
      new dependency, a version change or a new call path. Every reachable one is listed either way.

  contrib/secops/new-findings.py gosec BASE.json HEAD.json BASE_ROOT HEAD_ROOT
      BASE/HEAD are `gosec -fmt json` reports of the two trees. Exit 1 when HEAD has a HIGH-severity issue
      that BASE does not. An issue is rule + file (relative to its root) + the flagged code with gosec's
      line numbers stripped, counted as a multiset, so moving code does not make it new and a second
      copy of a flagged line does.

Standard library only; prints a Markdown summary to stdout (the workflow appends it to the step summary).
"""

import collections
import json
import os
import re
import sys


def json_stream(path):
    """govulncheck's -format json is a stream of JSON objects, not one document."""
    with open(path, encoding="utf-8") as f:
        text = f.read()
    dec, i, out = json.JSONDecoder(), 0, []
    while True:
        while i < len(text) and text[i].isspace():
            i += 1
        if i >= len(text):
            return out
        obj, i = dec.raw_decode(text, i)
        out.append(obj)


def reachable(path):
    """OSV id -> (module@version found, fixed version) for every finding whose trace reaches a function."""
    found = {}
    for msg in json_stream(path):
        f = msg.get("finding")
        if not f or not f.get("trace") or not f["trace"][0].get("function"):
            continue
        t = f["trace"][0]
        found.setdefault(f["osv"], ("%s@%s" % (t.get("module", "?"), t.get("version", "?")), f.get("fixed_version", "")))
    return found


def govulncheck(base_path, head_path):
    base, head = reachable(base_path), reachable(head_path)
    new = sorted(set(head) - set(base))
    print("### govulncheck (reachable vulnerabilities)\n")
    print("%d reachable on this PR's head, %d on its base, **%d new**.\n" % (len(head), len(base), len(new)))
    if head:
        print("| | OSV | found in | fixed in |\n|---|---|---|---|")
        for osv in sorted(head):
            mod, fixed = head[osv]
            print("| %s | [%s](https://pkg.go.dev/vuln/%s) | %s | %s |" % ("**new**" if osv in new else "", osv, osv, mod, fixed or "-"))
        print()
    if new:
        print("This PR makes the vulnerabilities marked **new** reachable: upgrade the module (or the Go "
              "toolchain for a standard-library one), or do not call the affected symbol.")
    return 1 if new else 0


LINE_NO = re.compile(r"^\s*\d+:\s?", re.M)


def gosec_issues(path, root):
    with open(path, encoding="utf-8") as f:
        report = json.load(f)
    root = os.path.abspath(root).rstrip("/") + "/"
    out = collections.Counter()
    where = {}
    for i in report.get("Issues") or []:
        if i.get("severity") != "HIGH":
            continue
        rel = i["file"][len(root):] if i["file"].startswith(root) else i["file"]
        key = (i["rule_id"], rel, " ".join(LINE_NO.sub("", i.get("code", "")).split()))
        out[key] += 1
        where.setdefault(key, (i.get("line", "?"), i.get("details", "")))
    return out, where


def gosec(base_path, head_path, base_root, head_root):
    base, _ = gosec_issues(base_path, base_root)
    head, where = gosec_issues(head_path, head_root)
    new = head - base
    print("### gosec (HIGH severity)\n")
    print("%d HIGH on this PR's head, %d on its base, **%d new**.\n" % (sum(head.values()), sum(base.values()), sum(new.values())))
    if new:
        print("| rule | file:line | issue |\n|---|---|---|")
        for (rule, rel, _), n in sorted(new.items()):
            line, details = where[(rule, rel, _)]
            print("| %s | %s:%s | %s%s |" % (rule, rel, line, details, " (x%d)" % n if n > 1 else ""))
        print("\nFix the new issue, or mark it `// #nosec <rule> -- <reason>` where the reason is one a "
              "reviewer can check.")
    return 1 if new else 0


def main(argv):
    if len(argv) == 4 and argv[1] == "govulncheck":
        return govulncheck(argv[2], argv[3])
    if len(argv) == 6 and argv[1] == "gosec":
        return gosec(*argv[2:6])
    sys.stderr.write(__doc__)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv))
