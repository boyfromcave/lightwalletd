# Security CI

lightwalletd-dd is an internet-facing gRPC server, so every PR, every push to `upgrade/vault` and a
nightly run go through the checks below. Three workflows hold them: `semgrep.yml`, `codeql.yml` and
`secops.yml`. Every tool runs at an exact version that is checked before it runs: a sha256 for
release binaries, `--require-hashes` for PyPI and the Go checksum database for `go install`. Every
action is pinned to a commit SHA. Results show up in each run's step summary and, as SARIF, under
**Security → Code scanning**, filed by tool name. The only bot PRs these checks open are the Semgrep
bucket PRs (at most four) and Dependabot's grouped security PRs (at most one per ecosystem).

| Leg | Workflow / job | What fails a PR | Full scan (push, nightly) |
|---|---|---|---|
| Semgrep | `semgrep.yml` | a new **critical** finding (diff-aware against the base) | full scan, then `sync`: one rolling PR per bucket (critical/high/medium/low) with an SLA ledger |
| CodeQL (`go`, `actions`, security-extended) | `codeql.yml` | nothing: alerts go to code scanning, whose PR check shows the new ones | weekly + push |
| zizmor (workflow security) | `secops.yml` / `zizmor` | any finding in any workflow or `dependabot.yml` | the same |
| gitleaks | `secops.yml` / `gitleaks` | a secret in the PR's own commits | full history (nightly, dispatch); the pushed commits (push) |
| OSV-Scanner | `secops.yml` / `osv-scanner` | a known vulnerability in the go.mod/go.sum dependency set that `osv-scanner.toml` does not accept | the same |
| govulncheck | `secops.yml` / `govulncheck` | a **reachable** vulnerability (one the code calls) that the base branch does not have | report only: every reachable one, in the summary and code scanning |
| gosec | `secops.yml` / `gosec` | a new HIGH-severity issue compared with the base | report only (SARIF) |
| Semgrep rule tests | `secops.yml` / `semgrep-rule-tests` | a local rule that misses or over-matches its test cases | the same |
| Dependabot | `.github/dependabot.yml` | n/a | security updates only, grouped (gomod, docker, github-actions) |

## Local rules (`contrib/semgrep/rules/lightwalletd.yaml`)

- `lightwalletd.rpc.node-wallet-or-write-method` (critical): the server never names a wallet-context
  or state-changing node RPC (`yed_mint`, `vault_lock`, `z_sendmany`, `dumpprivkey`, ...).
  `common/yellowback.go` `CallYed` and its allow-lists are the only way to reach `yed_*` / `set_*` /
  `vault_*`.
- `lightwalletd.rpc.dynamic-method-outside-gate`: `RawRequest` with a method name that is not a
  literal, anywhere outside that gate.
- `lightwalletd.grpc.yellowback-handler-without-limiter`: every exported `YellowbackStreamer` handler
  must charge the per-peer rate limiter (`y.limited`).
- `lightwalletd.grpc.request-range-unbounded`: a block range taken from a request with no length check.
- `lightwalletd.log.secret-or-raw-tx`: a log call that carries the RPC password, a raw transaction or
  the whole `Options` struct.

Test cases: `contrib/semgrep/testdata/lightwalletd.go`. Run them with
`semgrep --test --config contrib/semgrep/rules contrib/semgrep/testdata`.

## Accepting a finding (always with a reason a reviewer can check)

- **Semgrep**: an entry in `contrib/semgrep/accepted.json`, keyed by fingerprint or by rule + paths
  (`contrib/semgrep/README.md`). Policy is set in `contrib/semgrep/config.json`, through
  `exclude_rules`, `audit_rules` and `bucket_overrides`. `.semgrepignore` covers only vendored and
  generated code (`vendor/`, `walletrpc/*.pb.go`) and fixtures. Code that is unchanged from upstream
  is scanned and triaged like any other code.
- **gitleaks**: add the finding's fingerprint to `.gitleaksignore` under a comment that gives the
  reason. This is only for test vectors and public constants. A real secret is never accepted: it is
  rotated.
- **OSV-Scanner**: an `[[IgnoredVulns]]` entry in `osv-scanner.toml` with `reason` and an
  `ignoreUntil` no more than 90 days away.
- **govulncheck**: there is no ignore list. Fix the vulnerability by upgrading the module or the Go
  toolchain, or stop calling the affected symbol. On a PR, only vulnerabilities the PR adds fail the
  check (`contrib/secops/new-findings.py`).
- **gosec**: `// #nosec G<nnn> -- <reason>` on the line.
- **zizmor**: fix the workflow. Use an inline `# zizmor: ignore[<audit>]` with a reason only when the
  pattern is intended.

## Backlog (2026-10-23)

The first full scans found 15 known vulnerabilities in go.mod's dependencies, 13 of them reachable.
They also found 54 reachable vulnerabilities in all, most of them in the standard library of the
end-of-life Go 1.24.7. These were fixed by #3 (Go 1.26.9; grpc 1.83.2, x/net 0.60.0, btcd 0.26.2, prometheus
client_golang 1.24.1). `osv-scanner.toml` accepts nothing.

After the upgrade one reachable vulnerability is left, GO-2026-6809 (crypto/x509). It is fixed in Go
1.26.10, which came out on 2026-10-19 and is still younger than the two-week version rule. Move the
toolchain to 1.26.10 on or after 2026-11-02. Until then govulncheck reports it on every full scan;
it does not fail PRs because the base branch has it too.

The security jobs run on Go 1.26.9 whatever go.mod says, because govulncheck v1.8.0 needs Go 1.26.
govulncheck therefore judges the standard library of the toolchain the repository moves to.

## Repository settings (owner)

- *Settings → Code security*: turn on Dependabot alerts and Dependabot security updates. Code
  scanning receives the SARIF uploads, and that is free for public repositories.
- For the Semgrep bucket PRs, see `contrib/semgrep/README.md`, "Repository settings": *Allow GitHub
  Actions to create and approve pull requests*, and optionally a `SEMGREP_PR_TOKEN` secret.
