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

## Change after the review: LAN access (2026-11-24)

You chose to let home-network devices without Tailscale use ot, including
creating and editing links. It's opt-in (`bootstrap.sh --lan-access`) and
opens three layers together: an nftables rule for tcp/80 from the LAN
subnet, a systemd `IPAddressAllow` drop-in, and `OT_ALLOW_CIDRS`. `ot-verify`
fails if they disagree. Everything else still applies to LAN requests: the
Host allowlist, cross-origin protection, limits, audit log and revert
(`TestLANAccessWhenConfigured`).

What it costs:

- **Cleartext on the LAN.** Another device on the home network (a compromised
  IoT gadget, say) could sniff LAN-side ot traffic or, with ARP spoofing,
  alter redirects. Tailscale traffic stays encrypted.
- **Any LAN device can edit links**, including the devices most likely to be
  compromised.
- **IP-only attribution** for LAN edits. DHCP reservations make those
  addresses meaningful in the history.

Mitigations worth doing: put IoT devices and guests on an isolated guest
Wi-Fi/VLAN so they are outside `--lan-subnet`, and check `ot/_/activity` now
and then.

## Cloud hosting (2026-11-24)

ot can now run on an AWS Lightsail or Hetzner Cloud server
([cloud.md](cloud.md)). The app and its request checks are unchanged. What
changes is where it runs and who else can reach the machine.

**Exposure.** Once a server is created, no TCP port is public. (A new
Lightsail instance has AWS's default SSH/HTTP rules for the minute before
OpenTofu replaces them; sshd is key-only.) The provider's firewall (Lightsail
public ports, a Hetzner firewall) and nftables each allow only UDP 41641 in
from the internet. That is Tailscale's WireGuard port, which never answers a
packet from an unknown peer. Port 80 (ot) and port 22 (OpenSSH, as a fallback
to Tailscale SSH) are accepted only on `tailscale0`. The app's source check
and systemd's `IPAddressAllow` are the same as on the Pi. LAN access can't be
turned on: `bootstrap.sh` refuses it on a cloud server and `ot-verify` fails
if it finds it. Servers are IPv6-only by default, so there is no IPv4
address to scan.

| New risk | Handling |
|---|---|
| The provider runs the hypervisor and the disk, so it can read the links | Accepted: household links, and the same is true of S3 backups. |
| Someone who takes over your AWS or Hetzner account takes over the server | Use MFA on both accounts. Pass the Hetzner token as `TF_VAR_hcloud_token`, never in a file, and revoke it when you're done. |
| The Tailscale auth key sits in the server's user data and in local OpenTofu state | The key is one-off, pre-approved and tagged, so first boot spends it. It is still readable afterwards through the provider's metadata service and in the state file, but it's useless by then. `first-boot.sh` hands it to `tailscale up` from a 0600 file on tmpfs, never on a command line. The key must not be reusable. |
| No console login for break-glass access | Accepted. The admin account is key-only with passwordless sudo and is reachable only over the tailnet. If the server becomes unreachable, build a new one and restore the newest S3 backup, which loses at most the last hour of changes. |
| The backup key now lives on a provider's machine | Unchanged: it can only `PutObject`, and bucket versioning keeps every earlier object. The unit uses S3's dual-stack endpoints. |
| Clients on IPv4-only networks are relayed through Tailscale's DERP servers | WireGuard encrypts end to end, so a relay sees only ciphertext and timing. |
| OpenTofu providers and cloud images | Provider versions and hashes are pinned in the committed `.terraform.lock.hcl`. Images are the providers' official Debian 13 images. Tailscale comes from its signed apt repository, as on the Pi. |

**Verified so far:**

- `tofu validate` passes for both stacks.
- A read-only `tofu plan` against AWS shows three resources: the key pair, a
  `nano_ipv6_3_0` IPv6-only instance, and public ports with UDP 41641 only.
- The Hetzner server-type selection was tested on sample data.
- The user data renders to valid POSIX sh and carries `first-boot.sh`
  unchanged.
- shellcheck passes on every script.

**Not verified yet:** a real first boot on either provider, which would
exercise cloud-init and the Tailscale join with IPv6 only. On the server,
`ot-verify` checks the rest.

**Custom service name.** `bootstrap.sh` and OpenTofu both require the name
to be a DNS label starting with a letter. It becomes the first `OT_HOSTS`
entry, so the Host allowlist works as before. The generated favicon escapes
it. No new risk.

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
