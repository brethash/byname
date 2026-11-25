# OWASP review: ot v1 (2026-11-24)

Scope: the Go service (`cmd/`, `internal/`), the Pi deployment
(`deploy/`), and the threat model in the plan: a household tailnet, no logins,
plain HTTP inside WireGuard. Method: OWASP Top 10:2025 checklist, a pattern
sweep of the code, and attack replays in real headless Chrome against a
non-localhost origin, which is the condition `http://ot` runs in.

## Fixed during the review

| # | Category | Finding | Fix |
|---|---|---|---|
| 1 | A01 / A06 | **Every form in the UI would have been rejected in production.** `Referrer-Policy: no-referrer` makes browsers send `Origin: null` on form POSTs. Over plain HTTP there is no `Sec-Fetch-Site`, so `CrossOriginProtection` blocked ot's own forms. Unit tests passed because they set `Origin` by hand, and a `localhost` browser test passed because localhost counts as a secure context. Reproduced in Chrome on a non-localhost origin. | Policy changed to `same-origin` (header and meta tag). Other sites, including redirect destinations, still receive no Referer. Re-verified in Chrome. |
| 2 | A06 | **Audit-log growth by a compromised device.** 30 writes/min per device allowed about 43k audited changes a day, up to ~200 MB a day, enough to fill the SD card within days. | Per-device daily budget (`--write-daily`, default 300), checked atomically with the short-term limit (refunded if the short limit refuses). `/_/healthz` reports `disk_free_mb` and `low_disk`; `ot-verify` fails on low disk. |
| 3 | A01 | **Link cap bypass.** Restore, revert-to-deleted and import skipped the live-link cap. Found by automated commit review. | The store enforces the cap inside the transaction on every path that makes a link live; regression test covers all four. |

## Verified controls

| Category | Control | Evidence |
|---|---|---|
| A01 Access control | Tailnet-only peers (nftables, systemd `IPAddressAllow`, app check); Host allowlist; deny by default | `TestOnlyTailnetPeers`, `TestHostAllowlist`, `ot-verify` |
| A01 CSRF | `CrossOriginProtection` on every non-GET; JSON API requires `application/json`; no CORS; GET never mutates | Chrome replay: auto-submitted form 403, `no-cors` `text/plain` fetch 403, JSON fetch stopped at preflight (405, no CORS); `TestCrossOriginWritesBlocked`, `TestGETNeverMutates` |
| A01 DNS rebinding | Host allowlist → 421 | `TestHostAllowlist`, `ot-verify` |
| A02 Misconfiguration | CSP (`default-src 'none'`, no inline script), nosniff, frame-ancestors none, no directory listing, server timeouts, size limits, systemd sandbox (target exposure ≤ 2.0) | `TestSecurityHeadersEverywhere`, `TestLimits`, `ot-verify` |
| A03 Supply chain | 3 runtime modules, pinned `go.sum`, `govulncheck` and `staticcheck` pinned as Go tools in the gate, static `-trimpath` build, sha256 checked on deploy, signed Tailscale apt repo | `make gate` → "No vulnerabilities found" |
| A04 Crypto | Transport is WireGuard (Tailscale); S3 over TLS with SSE-S3 enforced by IAM condition; no passwords stored | `deploy/aws/iam-policy.json` |
| A05 Injection | All SQL values bound; the only concatenated SQL pieces are constants, a switch-selected `ORDER BY`, and integer `LIMIT`s; LIKE wildcards escaped; `html/template` everywhere (no `template.HTML`); no `os/exec`; shell scripts quote variables and validate inputs before `sed` | pattern sweep; `TestStoredXSSIsEscaped`; shellcheck clean |
| A05 Open redirect | Redirects only to stored, validated `http(s)` targets; template placeholders cannot reach the host; post-form redirects are local paths that cannot start with `//` | `FuzzValidateTarget`, `FuzzExpand` (27M+ execs) |
| A06 Design | Threat model; optimistic concurrency; soft deletes; append-only audit log enforced by triggers; one-click revert; rate and daily limits; link cap | store and web tests |
| A07 Auth | By design no logins; identity is attribution only (Tailscale WhoIs) and never grants anything | — |
| A08 Integrity | Strict JSON decoding (unknown fields rejected); imports re-validated; backups put-only, versioned, off-box | `TestAPI`, import smoke test |
| A09 Logging | Every change logged with actor; blocked cross-origin attempts at WARN with Origin; 403/421/429 logged; durable audit in the DB | server log during Chrome replay |
| A10 Exceptions | Panics recovered → generic 500; internal errors logged, not shown; source and Host checks fail closed; WhoIs failure degrades attribution only | `fail()`, `recoverer` |

## Accepted risks

1. **A household member can change any link on purpose.** That is the
   chosen model. It is attributed and revertible. "Locked links" (edits
   restricted to named tailnet users) is the upgrade path.
2. **A browser that sends no `Origin` on a cross-site POST** would get past
   `CrossOriginProtection`. Every current browser sends it.
3. **Root on the Pi can rewrite history.** The triggers stop the app, not
   root. The S3 copies are append-only from the Pi's side, so they are the
   tamper-evident record.
4. **SD card theft exposes the links.** The database is not encrypted at
   rest. Some saved URLs may carry share tokens. Optional `age` encryption
   of backups is in "Later".
5. **The admin account may have passwordless sudo** (a Raspberry Pi OS
   default). SSH is key-only and Tailscale SSH uses check mode. To require a
   password, remove `/etc/sudoers.d/010_pi-nopasswd` if present; `make deploy`
   uses `ssh -t`, so sudo can prompt.
6. **`/_/whoami` shows WhoIs error text** (for example the socket path) to
   tailnet users. Kept on purpose: it is the M0 diagnostic.
