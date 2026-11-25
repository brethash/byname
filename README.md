# ot/

Household short links (`ot/wiki`, `ot/gh/otterly`) for every device on your
tailnet, at home or away. They're served from a Raspberry Pi 3B at home (which
can also serve home-network devices that don't run Tailscale) or from a
$3.50/month cloud server.

`ot` is the default name. Choose your own (`go/wiki`, `links/wiki`) with one
setting: `bootstrap.sh --name go`, or `name = "go"` in `deploy/cloud`.

- **Tailnet first, LAN optional.** Tailscale MagicDNS resolves `ot` on every
  device running Tailscale, and WireGuard encrypts that traffic. With
  `--lan-access`, home-network devices without Tailscale can use it too, over
  plain HTTP and identified by IP address; router DNS resolves `ot` for them.
- **No logins, full accountability.** Anyone on the tailnet can create or edit
  a link. Every change records who made it (Tailscale WhoIs), and any earlier
  version can be restored with one click.
- **Public access, if you want it.** Optionally publish it on the internet at
  `https://ot.<tailnet>.ts.net` through Tailscale Funnel. People outside the
  tailnet sign in with Google or Slack, and only accounts on your allow list
  get in ([docs/public-access.md](docs/public-access.md)).
- **Small and boring to run.** One static Go binary, SQLite, about 30 MB of RAM,
  near-zero disk writes when idle, automatic OS updates, and hourly
  snapshots to S3.

## Using it

| You type | You get |
|---|---|
| `ot/` | home: search, create, popular, recent changes |
| `ot/wiki` | redirect to the link's destination |
| `ot/wiki/some/page?x=1` | the destination with `/some/page?x=1` appended |
| `ot/gh/otterly` | template `https://github.com/{1}` → `https://github.com/otterly` |
| `ot/wiki+` | the link's details and history, without redirecting |
| `ot/nope` | "doesn't exist yet", with a create form |

Browser setup notes: [docs/client-setup.md](docs/client-setup.md).

## Developing

Needs Go 1.25+ (for `net/http.CrossOriginProtection`) and shellcheck.

```sh
make run      # http://localhost:8080, dev mode (localhost allowed, no WhoIs)
make gate     # vet, staticcheck, race tests, govulncheck, shellcheck
make fuzz     # fuzz URL validation and template expansion
```

```
cmd/ot/            serve | snapshot | backup | export | import | sessions | version
internal/link/     names, destination validation, template expansion (+ fuzz tests)
internal/store/    SQLite: links, append-only audit log, snapshots
internal/app/      validation, click batching, snapshot scheduling
internal/web/      middleware, handlers, templates, JSON API, public (Funnel) address
internal/auth/     Google and Slack sign-in (OpenID Connect)
internal/whois/    tailscaled LocalAPI WhoIs client
internal/backup/   S3 upload of the newest snapshot
deploy/host/       bootstrap (Pi or cloud), firewall, install/verify/restore scripts
deploy/systemd/    units
deploy/cloud/      OpenTofu for AWS Lightsail and Hetzner, plus the first-boot script
```

## Where to run it

| Host | Cost per month | Who can use it |
|---|---|---|
| Raspberry Pi at home | about $0.30 of electricity | tailnet devices, and optionally home-network devices without Tailscale |
| AWS Lightsail, IPv6-only ([docs/cloud.md](docs/cloud.md)) | $3.50 | tailnet devices |
| Hetzner Cloud, cheapest type in stock ([docs/cloud.md](docs/cloud.md)) | €5.99–€17.99 | tailnet devices |

Prices as of 2026-11-24. [docs/operations.md](docs/operations.md) covers Pi
setup, deploys with automatic rollback, S3 backups and restores, and
troubleshooting. [docs/cloud.md](docs/cloud.md) covers the cloud servers and
moving between hosts.

```sh
make deploy TARGET=<you>@ot      # builds arm64 + amd64, ships the one the host needs
```

## Security

Every request on the tailnet address passes these checks before any handler
runs:

1. the peer must be a tailnet address;
2. `Host` must be one of `OT_HOSTS` (blocks DNS rebinding);
3. request size limits;
4. `Origin` must match on every write (Go's `CrossOriginProtection`);
5. strict CSP and related headers on every response;
6. write limits per device or signed-in account, a short-term rate and a
   daily budget; devices without Tailscale share one budget.

Destinations must be plain `http(s)` URLs with no embedded credentials,
backslashes or look-alike Unicode. Template placeholders can never change the
host. Redirects are always `302` with `no-store`, so edits take effect
immediately. The JSON API requires `application/json`, so browsers must send
a CORS preflight, which is never granted.

The optional public address reaches ot over a socket that only Tailscale can
open. There, everything except the sign-in pages needs a session from an
allow-listed Google or Slack account. Tailnet devices are recognised and
skip sign-in. Sessions are server-side, in `__Host-` cookies that are
Secure, HttpOnly and SameSite=Lax.

On the host, the service runs under a tight systemd sandbox: read-only
system, syscall filter, only `CAP_NET_BIND_SERVICE`, and only tailnet peers
allowed. It sits behind a default-drop firewall; cloud servers also sit
behind the provider's firewall, with no public port except Tailscale's. The
AWS key exists only in the backup unit and can only add objects.

The OWASP review, including what it fixed and the risks accepted, is in
[docs/security-review.md](docs/security-review.md).
