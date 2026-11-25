# OWASP review: byname v1 (2026-11-24)

Scope: the Go service (`cmd/`, `internal/`), the Pi deployment
(`deploy/`), and the threat model in the plan: a household tailnet, no logins,
plain HTTP inside WireGuard. Method: OWASP Top 10:2025 checklist, a pattern
sweep of the code, and attack replays in real headless Chrome against a
non-localhost origin, which is the condition `http://nym` runs in.

## Fixed during the review

| # | Category | Finding | Fix |
|---|---|---|---|
| 1 | A01 / A06 | **Every form in the UI would have been rejected in production.** `Referrer-Policy: no-referrer` makes browsers send `Origin: null` on form POSTs. Over plain HTTP there is no `Sec-Fetch-Site`, so `CrossOriginProtection` blocked byname's own forms. Unit tests passed because they set `Origin` by hand, and a `localhost` browser test passed because localhost counts as a secure context. Reproduced in Chrome on a non-localhost origin. | Policy changed to `same-origin` (header and meta tag). Other sites, including redirect destinations, still receive no Referer. Re-verified in Chrome. |
| 2 | A06 | **Audit-log growth by a compromised device.** 30 writes/min per device allowed about 43k audited changes a day, up to ~200 MB a day, enough to fill the SD card within days. | Per-device daily budget (`--write-daily`, default 300), checked atomically with the short-term limit (refunded if the short limit refuses). `/_/healthz` reports `disk_free_mb` and `low_disk`; `byname-verify` fails on low disk. |
| 3 | A01 | **Link cap bypass.** Restore, revert-to-deleted and import skipped the live-link cap. Found by automated commit review. | The store enforces the cap inside the transaction on every path that makes a link live; regression test covers all four. |

## Verified controls

| Category | Control | Evidence |
|---|---|---|
| A01 Access control | Tailnet-only peers (nftables, systemd `IPAddressAllow`, app check); Host allowlist; deny by default | `TestOnlyTailnetPeers`, `TestHostAllowlist`, `byname-verify` |
| A01 CSRF | `CrossOriginProtection` on every non-GET; JSON API requires `application/json`; no CORS; GET never mutates | Chrome replay: auto-submitted form 403, `no-cors` `text/plain` fetch 403, JSON fetch stopped at preflight (405, no CORS); `TestCrossOriginWritesBlocked`, `TestGETNeverMutates` |
| A01 DNS rebinding | Host allowlist → 421 | `TestHostAllowlist`, `byname-verify` |
| A02 Misconfiguration | CSP (`default-src 'none'`, no inline script), nosniff, frame-ancestors none, no directory listing, server timeouts, size limits, systemd sandbox (target exposure ≤ 2.0) | `TestSecurityHeadersEverywhere`, `TestLimits`, `byname-verify` |
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

You chose to let home-network devices without Tailscale use byname, including
creating and editing links. It's opt-in (`bootstrap.sh --lan-access`) and
opens three layers together: an nftables rule for tcp/80 from the LAN
subnet, a systemd `IPAddressAllow` drop-in, and `BYNAME_ALLOW_CIDRS`. `byname-verify`
fails if they disagree. Everything else still applies to LAN requests: the
Host allowlist, cross-origin protection, limits, audit log and revert
(`TestLANAccessWhenConfigured`).

What it costs:

- **Cleartext on the LAN.** Another device on the home network (a compromised
  IoT gadget, say) could sniff LAN-side byname traffic or, with ARP spoofing,
  alter redirects. Tailscale traffic stays encrypted.
- **Any LAN device can edit links**, including the devices most likely to be
  compromised.
- **IP-only attribution** for LAN edits. DHCP reservations make those
  addresses meaningful in the history.
- **One shared change budget** for every device without Tailscale (30 a
  minute, 300 a day; since 2026-11-25). A LAN device can take any number of
  addresses, so per-address budgets bounded nothing. One misbehaving LAN
  device can use up the budget for the others; tailnet devices and
  signed-in accounts keep their own.

Mitigations worth doing: put IoT devices and guests on an isolated guest
Wi-Fi/VLAN so they are outside `--lan-subnet`, and check `nym/_/activity` now
and then.

## Cloud hosting (2026-11-24)

byname can now run on an AWS Lightsail or Hetzner Cloud server
([cloud.md](cloud.md)). The app and its request checks are unchanged. What
changes is where it runs and who else can reach the machine.

**Exposure.** Once a server is created, no TCP port is public. (A new
Lightsail instance has AWS's default SSH/HTTP rules for the minute before
OpenTofu replaces them; sshd is key-only.) The provider's firewall (Lightsail
public ports, a Hetzner firewall) and nftables each allow only UDP 41641 in
from the internet. That is Tailscale's WireGuard port, which never answers a
packet from an unknown peer. Port 80 (byname) and port 22 (OpenSSH, as a fallback
to Tailscale SSH) are accepted only on `tailscale0`. The app's source check
and systemd's `IPAddressAllow` are the same as on the Pi. LAN access can't be
turned on: `bootstrap.sh` refuses it on a cloud server and `byname-verify` fails
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
`byname-verify` checks the rest.

**Custom service name.** `bootstrap.sh` and OpenTofu both require the name
to be a DNS label starting with a letter. It becomes the first `BYNAME_HOSTS`
entry, so the Host allowlist works as before. The generated favicon escapes
it. No new risk.

## Public address with sign-in (2026-11-25)

Optional ([public-access.md](public-access.md)). Tailscale Funnel publishes
`https://<name>.<tailnet>.ts.net` on the internet. Devices on the tailnet or
the home LAN still skip sign-in. Everyone else must sign in with Google or
Slack, and only accounts on the allow list get in.

**Exposure.** Still no public port on the host. Funnel traffic comes in over
the tailnet: Tailscale's ingress relays, then WireGuard to the host. The
host's tailscaled terminates TLS and proxies to byname over
`/run/byname/public.sock`. That directory is 0700 and the socket 0600, both owned by byname, so only
root (tailscaled) and byname can connect. Anyone on the internet can now reach
the sign-in page, the static assets and the favicon. Everything else needs
a session.

**Trust boundary.** On that socket only, byname reads three headers that
tailscaled sets: `X-Forwarded-For` (the real client address; tailscaled
overwrites it rather than appending), `X-Forwarded-Host`, and
`Tailscale-Funnel-Request`, which marks internet requests (tailscaled deletes
any copy a client sends, including underscore variants). This was checked in
Tailscale's source, v1.104 `ipn/ipnlocal/serve.go`. A request counts as
coming from the tailnet only when it has no Funnel marker **and** its
forwarded address is a tailnet address. A missing or unparsable address
counts as the internet. On the plain-HTTP listener these headers are
ignored.

| Control | How |
|---|---|
| Sign-in | OpenID Connect authorization-code flow as a confidential client. Google uses PKCE (S256). Slack doesn't advertise PKCE, so state, nonce and the client secret protect its flow. |
| ID tokens | Taken straight from the token endpoint over TLS, in a request authenticated with the client secret. The signature isn't checked, which OIDC Core §3.1.3.7(6) allows for tokens received this way. byname checks issuer, audience (and `azp` when there are several audiences), expiry and issue time with 2 minutes of skew, and the nonce (constant-time). It accepts only a verified email address. |
| Who gets in | Google: listed addresses, or Workspace domains matched on the `hd` claim, never on the address suffix. A listed address counts only where Google vouches for it: Gmail, or an account in that address's Workspace domain. Slack: workspace IDs. Checked at sign-in and again on every request, so removing someone signs them out. |
| Login CSRF | `state`, nonce, PKCE verifier, destination and expiry (10 minutes) are sealed with AES-256-GCM, using a key made at startup, into a `__Host-byname_signin` cookie. The server keeps nothing per sign-in. The callback's `state` must match the browser's own cookie (constant time). A mismatched callback is refused and leaves that cookie alone, so it can't cancel a real sign-in. The cookie is cleared once used, and the provider refuses a replayed code. |
| Sessions | A 256-bit random token in a `__Host-byname_session` cookie: Secure, HttpOnly, SameSite=Lax, 30 days. The database stores only its SHA-256, so snapshots and backups can't be used to sign in. Sign-out deletes the row. A new sign-in replaces any old session. Each account keeps at most 10 sessions; older ones are dropped. `byname sessions revoke` ends sessions from the host. |
| Open redirects | After sign-in byname returns only to a local path. `//host`, `/\host`, control characters and the sign-in pages are refused. |
| CSRF on writes | Unchanged `CrossOriginProtection`. Over HTTPS browsers send `Sec-Fetch-Site`. `Host` is reset to the public name before the check. |
| Abuse | Requests from the internet without a session are limited per address, or per /64 for IPv6 (60 a minute, bursts of 30). The limiter's memory is bounded: entries are kept in least-recently-used order, dropped after 31 idle seconds, and past 10,000 the least recently used one makes room, so every address still gets a bucket of its own. Sign-ins in progress cost the server nothing. Signed-in users get the per-account write limits. |
| Mistaken Funnel | The plain-HTTP listener refuses requests carrying Tailscale's Funnel marker, so `tailscale funnel 80` can't publish byname without sign-in. `byname-verify` fails on any Serve/Funnel target other than byname's socket, and on any Funnel while the public address is off. |
| Secrets | Client secrets come from systemd credentials (`/etc/credstore`, root 0600, `ImportCredential=byname.*`), never from flags or the environment. They are sent only to the provider's token endpoint, never put in a browser URL. |
| Logging | Every sign-in, refusal, failure and sign-out is logged with provider, account and address. Tokens and codes are never logged. |
| Transport | HSTS on the public name. TLS is Tailscale's, with a Let's Encrypt certificate. |

| New risk | Handling |
|---|---|
| byname needs outbound internet for the token exchange, so the drop-in sets `IPAddressAllow=any`, which also lifts systemd's inbound filter on port 80 | Two of the three layers remain: nftables and byname's own peer check. The drop-in makes byname require `byname-firewall.service`, so it doesn't run without the firewall. Funnel traffic arrives on the socket, outside IP filtering. Possible later: an nftables output rule that limits byname's own outbound connections to tcp/443. |
| Suspending someone at Google or Slack doesn't end their byname session (up to 30 days) | Documented. Remove them from the allow list or run `byname sessions revoke`. |
| Allowing a Slack workspace admits its guests | Documented. |
| A flood of made-up session cookies costs one SQLite primary-key lookup each before the rate limiter answers | Accepted: microseconds per request, and Funnel's own limits apply. |
| A stolen session cookie or a taken-over Google/Slack account gives the same access as a household member | It's attributed in the history, and `byname sessions revoke` and the allow list cut it off. Use MFA on those accounts. |
| The tailnet name appears in Certificate Transparency logs and public DNS | Accepted: Funnel needs a public name. |
| Slack can't be used on IPv6-only servers (`slack.com` has no AAAA record) | Documented. `byname-verify` fails with the reason. |
| Sessions live in the same SQLite file as the links | Only hashes are stored. They are restored along with the links. |

**Independent review (same day).** A second reviewer, working only from the
code and the owasp-security checklist, found no way to bypass authentication
and confirmed the unsigned-ID-token reasoning for both providers. It found
these, all fixed before commit:

| # | Severity | Finding | Fix |
|---|---|---|---|
| 1 | Medium | Two addresses could keep the in-memory list of pending sign-ins (1,000 max, 10 minutes each) full and block every internet sign-in. A hostile page could do the same through its visitors, because `/start` is a GET. | Pending state moved into the AES-GCM-sealed cookie; the server keeps none. |
| 2 | Medium | The anonymous rate limiter kept an entry per address for 24 hours, with no cap and a full scan on every call. Rotating IPv6 addresses could run byname out of memory, for tailnet users too. | Idle entries dropped after 31 s, IPv6 keyed by /64, and a 10,000-entry cap that evicts the least recently used entry. A first version sent newcomers to a shared overflow bucket once full; an automated review after the commit pointed out that an attacker with a /48 could drain it, so eviction replaced it. The write limiter doesn't evict: with its table full it refuses new keys, so no one can reset a daily budget by pushing it out A third automated review then noted that LAN devices were keyed per address, so one LAN device taking many addresses could fill that table and lock out newcomers. Devices without Tailscale now share one change budget (30 a minute, 300 a day), as the first review had recommended. |
| 3 | Low | A Funnel pointed at port 80 (`tailscale funnel 80`) arrives from localhost, which the private listener trusts, so it would have skipped sign-in. `byname-verify` didn't notice. | The private listener refuses Funnel-marked requests. `byname-verify` audits every Serve/Funnel target. |
| 4 | Low | A listed non-Gmail address matched any Google account with that address. Google doesn't vouch for those. | Such entries match only accounts in that domain's Workspace (`hd`). |
| 5 | Low | `IPAddressAllow=any` also lifts systemd's inbound filter, but the docs said the defences were unchanged. | The drop-in requires the firewall; docs corrected. |
| 6 | Low | One allowed account could fill the 10,000-row session table. | At most 10 sessions per account. |

Also taken from its hardening notes:

- An unclassified request now counts as the internet (the zero value), not
  as trusted.
- The tailnet path through Serve honours `--allow-cidrs`.
- The test hooks require an https token endpoint.
- The sample policy ships with Funnel commented out.
- The docs cover Slack guests, and note that suspending someone at the
  provider doesn't end their session.

**Verified:**

- Unit tests cover the provider exchange, every ID-token rejection, the allow
  lists (including the Gmail/Workspace rule) and the sealed sign-in cookie
  (wrong state, tampered, expired, another process's key).
- HTTP tests cover the full sign-in flow against a fake token endpoint:
  tailnet passthrough, internet gating of every route class, a forged or
  missing forwarding header, a foreign forwarded host, state/cookie mismatch
  and replay, refusal of someone not on the list, cancellation, sign-out,
  removal from the allow list, Workspace-domain matching, rate limiting,
  limiter memory bounds, IPv6 /64 keys, the per-account session cap,
  `safeNext`, that the private listener ignores forwarding headers, and that
  it refuses Funnel traffic.
- Smoke test of the real binary on a unix socket (0600).
- `make gate` passes.

**Not verified yet:** a real Funnel, real Google and Slack apps, and a phone
on cellular. `byname-verify` and the phone check in public-access.md cover these
on the host.

## Without Tailscale: your own domain (2026-11-25)

Tailscale is now optional. A server can serve its own domain directly
(`bootstrap.sh --no-tailscale --public-domain hg.example.com`): byname terminates
TLS with a Let's Encrypt certificate, and everyone signs in with Google or
Slack. Sign-in, sessions, allow lists and the write limits are the ones
reviewed above. What changes is the exposure.

**Exposure.**

| Port | Who can reach it |
|---|---|
| TCP 443 | The internet. byname's TLS listener: every request counts as coming from the internet and needs a session, except the sign-in pages and static assets. |
| TCP 80 | The internet. It answers Let's Encrypt's HTTP challenge and redirects everyone else to https. Only peers in `BYNAME_ALLOW_CIDRS` (localhost, plus tailnet/LAN when those are on) reach the private, no-login handler. |
| TCP 22 | The provider firewall limits it to `ssh_cidrs`. On the host, `--ssh-from any` leaves address filtering to the provider; a specific `--ssh-from` filters there too. sshd is key-only, with no root login and `AllowUsers admin`. |

Nothing else is open: no Tailscale port without Tailscale.

| Control | How |
|---|---|
| No trusted headers | On the direct TLS listener, byname reads nothing from the request to decide trust. The peer is the TCP peer, and `Host` must be the public name (421 otherwise). Forwarding and `Tailscale-*` headers are ignored. |
| Certificates | `autocert` with `HostWhitelist(public name)`: a TLS handshake for any other name fails before Let's Encrypt is contacted, so scanners can't make byname request certificates or burn its rate limits. Keys and certificates are in `/var/lib/byname/acme` (0700, byname). TLS 1.2 minimum. HSTS is on. |
| Port 80 redirect | byname builds the target from its own public URL plus the request's path and query only. An opaque request target (`GET http:evil.example`) can't become part of the host. |
| Connection floods | Each internet-facing listener holds at most 256 internet connections, and 32 per IPv4 address or IPv6 /64. Over the limit, a connection is closed at once instead of waiting, so nobody can hold the accept queue. Tailnet, LAN and localhost peers are never counted or refused. Port 80 redirects close their connection. Timeouts: 5 s for headers (TLS handshakes included), 10 s for the body, 60 s idle. |
| Port 80 and localhost | Port 80 gives the private handler only to trusted peers using the short name. Localhost gets only `/_/healthz`, so a reverse proxy or tunnel that forwarded internet traffic to localhost:80 can't expose the no-login handler. |
| Firewall | nftables opens 80 and 443 only with `--public-domain`, and the Tailscale rules only with Tailscale. With a public address, byname won't start without the firewall (`Requires=byname-firewall.service`). |
| Fail-closed setup | OpenTofu refuses a server without Tailscale that has no `ssh_cidrs` or no `public_domain`, and a public domain without IPv4. It parses `ssh_cidrs` (canonical, no wider than /16 or /48, an IPv4 block when the server has IPv4). After every apply, it fails if AWS's stored firewall lets SSH in from the whole internet. `bootstrap.sh` won't open the public address until a sign-in provider is configured, won't run `--no-tailscale` while tailscaled is up (Tailscale SSH bypasses the firewall), and refuses `--ssh-from any` on a Pi. `byname-verify` reads the effective `BYNAME_ALLOW_CIDRS`, fails if tailscaled runs without Tailscale configured, makes a real HTTPS request (DNS, certificate, sign-in redirect), and checks that port 80 only redirects an untrusted peer. |

| New risk | Handling |
|---|---|
| The server is directly on the internet: scanners, TLS and HTTP parser bugs, floods | Go's TLS and HTTP stacks, kept current by `make gate`'s govulncheck and redeploys. Connection cap and timeouts. Everything but sign-in needs a session. |
| SSH reachable from the internet when the provider firewall is the only address filter (`--ssh-from any`) | Chosen so a changed home address doesn't lock you out: `tofu apply` with new `ssh_cidrs` fixes it. sshd is key-only. Use a specific `--ssh-from` if your address is static. |
| A new Lightsail instance has AWS's default firewall (22 and 80 open) for about a minute | Unchanged from before; nothing listens on 80 yet, and sshd accepts only your key. |
| No WhoIs and no tailnet: every user is identified by their Google or Slack account | That's the point of this mode. |
| Certificates depend on DNS pointing at the server | If DNS is wrong, HTTPS fails closed (no certificate); nothing falls back to plain HTTP. While DNS is wrong, anyone probing the name can make byname retry and use up Let's Encrypt's 5 failed validations an hour, which only delays your own certificate. Set DNS first. |
| On a Pi, a router that rewrites the source address of forwarded connections would make internet clients look like LAN peers on port 80 | Documented: don't combine `--lan-access` with `--public-domain` behind such a router. |
| byname behind someone else's proxy | Not supported: in direct mode byname must be the one terminating TLS, and it trusts no forwarding headers. |

**Independent review (same day).** A reviewer working from the code found no
path from the internet to the no-login handler and no open redirect. It
found:

| # | Severity | Finding | Fix |
|---|---|---|---|
| 1 | Medium | One host could hold all 512 connection slots on port 80 or 443. Accept then waited, and the household's tailnet/LAN users queued behind it. File descriptors could also run out before the cap, and accept errors were logged only at debug. | The non-blocking guard above, with per-address limits; trusted peers exempt; caps lowered to 256; port 80 redirects close; accept errors at warn. |
| 2 | Low–medium | `--ssh-from any` was accepted on a Pi, which has no provider firewall. | Refused on a Pi. |
| 3 | Low–medium | `byname-verify` read `BYNAME_ALLOW_CIDRS` only from `byname.env`, but `byname.local.env` overrides it. A broad value there would have opened the no-login handler on port 80 to the internet, unnoticed. | Effective value read, plus a live check that port 80 only redirects an untrusted peer. |
| 4 | Low | `ssh_cidrs` was checked as a string: `1.2.3.4/0`, `0::/0` and host bits got through. | Parsed: canonical form, size limits, and an IPv4 block required with IPv4. A post-apply check covers AWS's undocumented handling of an address family left out. |
| 5 | Low | `--no-tailscale` on a host that had Tailscale left tailscaled, and so Tailscale SSH, running. | The bootstrap refuses until it's stopped, and `byname-verify` fails while it runs. |

It also found three functional bugs, all fixed:

- the first no-Tailscale bootstrap run (before sign-in exists) failed its
  own preflight;
- a LAN or tailnet peer using the public name on port 80 got a 421 instead
  of a redirect;
- a trailing dot in the public URL broke certificates.

Taken from its hardening notes:

- localhost now reaches only the health check on port 80;
- Lightsail's firewall rules no longer wait for the static IP, which
  shortens AWS's default-firewall window at creation.


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
