# byname

Short links by name, for a household or a small team: `nym/wiki`,
`nym/gh/octocat`. They're
served from a Raspberry Pi 3B at home or from a $3.50–$5.00/month cloud
server. People reach them on any of three paths:

- your tailnet;
- your home network;
- your own domain on the internet (`https://hg.example.com/wiki`), with
  Slack or Google sign-in.

Tailscale is optional.

`nym` is the default prefix people type. Choose your own (`go/wiki`,
`hg/wiki`) with one setting: `bootstrap.sh --name hg`, or `name = "hg"` in
`deploy/cloud`.

- **On the tailnet, no logins.** Tailscale MagicDNS resolves `nym` on every
  device running Tailscale, and WireGuard encrypts that traffic. Anyone on
  the tailnet can create or edit a link. Every change records who made it
  (Tailscale WhoIs), and any earlier version can be restored with one click.
- **On the home network, optionally.** With `--lan-access` on a Pi,
  home-network devices without Tailscale can use it too, over plain HTTP and
  identified by IP address; router DNS resolves `nym` for them.
- **On the internet, with sign-in.** Serve your own domain
  (`https://hg.example.com`, Let's Encrypt certificate; no Tailscale needed),
  or publish the tailnet name through Tailscale Funnel. People there sign in
  with Google or Slack, only accounts on your allow list get in, and the
  history records the account ([docs/public-access.md](docs/public-access.md)).
- **Small and boring to run.** One static Go binary, SQLite, about 30 MB of RAM,
  near-zero disk writes when idle, automatic OS updates, and hourly
  snapshots to S3.

## Using it

| You type | You get |
|---|---|
| `nym/` | home: search, create, popular, recent changes |
| `nym/wiki` | redirect to the link's destination |
| `nym/wiki/some/page?x=1` | the destination with `/some/page?x=1` appended |
| `nym/gh/octocat` | template `https://github.com/{1}` → `https://github.com/octocat` |
| `nym/wiki+` | the link's details and history, without redirecting |
| `nym/nope` | "doesn't exist yet", with a create form |

Browser setup notes: [docs/client-setup.md](docs/client-setup.md).

## Developing

Needs Go 1.25+ (for `net/http.CrossOriginProtection`) and shellcheck.

```sh
make run      # http://localhost:8080, dev mode (localhost allowed, no WhoIs)
make gate     # vet, staticcheck, race tests, govulncheck, shellcheck
make fuzz     # fuzz URL validation and template expansion
```

```
cmd/byname/        serve | snapshot | backup | export | import | sessions | version
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
| AWS Lightsail with your own domain ([docs/cloud.md](docs/cloud.md)) | $5.00 | anyone who signs in with Slack or Google; tailnet devices too if you add Tailscale |
| AWS Lightsail, IPv6-only, Tailscale ([docs/cloud.md](docs/cloud.md)) | $3.50 | tailnet devices |
| Hetzner Cloud, cheapest type in stock ([docs/cloud.md](docs/cloud.md)) | €5.99–€17.99, plus IPv4 for a domain | either of the above |

Prices as of 2026-11-24. [docs/operations.md](docs/operations.md) covers Pi
setup, deploys with automatic rollback, S3 backups and restores, and
troubleshooting. [docs/cloud.md](docs/cloud.md) covers the cloud servers and
moving between hosts.

```sh
make deploy TARGET=<you>@nym      # builds arm64 + amd64, ships the one the host needs
```

## Security

Every request on the plain-HTTP address passes these checks before any
handler runs:

1. the peer must be a tailnet address (or the home LAN with `--lan-access`);
2. `Host` must be one of `BYNAME_HOSTS` (blocks DNS rebinding);
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

On the optional public address, everything except the sign-in pages needs a
session from an allow-listed Google or Slack account. Sessions are
server-side, in `__Host-` cookies that are Secure, HttpOnly and
SameSite=Lax.

- **Your own domain**: byname terminates TLS itself, with a Let's Encrypt
  certificate for that one name. It trusts no proxy headers, and treats
  every request there as coming from the internet.
- **Funnel**: requests arrive over a socket that only Tailscale can open, and
  tailnet devices are recognised and skip sign-in.

On the host, the service runs under a tight systemd sandbox: read-only
system, syscall filter, only `CAP_NET_BIND_SERVICE`, and only tailnet peers
allowed. It sits behind a default-drop firewall; cloud servers also sit
behind the provider's firewall, with no public port except Tailscale's. The
AWS key exists only in the backup unit and can only add objects.

The OWASP review, including what it fixed and the risks accepted, is in
[docs/security-review.md](docs/security-review.md).
