# ot/

Household short links (`ot/wiki`, `ot/gh/otterly`) served from a Raspberry
Pi 3B to every device on your tailnet, at home or away, and optionally to
devices on the home network that don't run Tailscale.

- **Tailnet first, LAN optional.** Tailscale MagicDNS resolves `ot` on every
  device running Tailscale, and WireGuard encrypts that traffic. With
  `--lan-access`, home-network devices without Tailscale can use it too, over
  plain HTTP and identified by IP address; router DNS resolves `ot` for them.
- **No logins, full accountability.** Anyone on the tailnet can create or edit
  a link. Every change records who made it (Tailscale WhoIs), and any earlier
  version can be restored with one click.
- **Small and boring to run.** One static Go binary, SQLite, about 30 MB of RAM,
  near-zero SD card writes when idle, automatic OS updates, and hourly
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
cmd/ot/            serve | snapshot | backup | export | import | version
internal/link/     names, destination validation, template expansion (+ fuzz tests)
internal/store/    SQLite: links, append-only audit log, snapshots
internal/app/      validation, click batching, snapshot scheduling
internal/web/      middleware, handlers, templates, JSON API
internal/whois/    tailscaled LocalAPI WhoIs client
internal/backup/   S3 upload of the newest snapshot
deploy/            Pi bootstrap, systemd units, firewall, deploy/verify/restore scripts
```

## Running it on the Pi

[docs/operations.md](docs/operations.md) covers first-time setup, deploys with
automatic rollback, S3 backups and restores, and troubleshooting.

```sh
make deploy PI=<you>@ot
```

## Security

Every request passes these checks before any handler runs:

1. the peer must be a tailnet address;
2. `Host` must be one of `OT_HOSTS` (blocks DNS rebinding);
3. request size limits;
4. `Origin` must match on every write (Go's `CrossOriginProtection`);
5. strict CSP and related headers on every response;
6. per-device write limits (a short-term rate and a daily budget).

Destinations must be plain `http(s)` URLs with no embedded credentials,
backslashes or look-alike Unicode. Template placeholders can never change the
host. Redirects are always `302` with `no-store`, so edits take effect
immediately. The JSON API requires `application/json`, so browsers must send
a CORS preflight, which is never granted.

On the Pi, the service runs under a tight systemd sandbox: read-only system,
syscall filter, only `CAP_NET_BIND_SERVICE`, and only tailnet peers allowed.
It sits behind a default-drop firewall. The AWS key exists only in the backup
unit and can only add objects.

The OWASP review, including what it fixed and the risks accepted, is in
[docs/security-review.md](docs/security-review.md).
