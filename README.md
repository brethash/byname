# byname

Tired of forgetting that link to your critical google doc, confluence page, favorite [Homestar Runner](https://homestarrunner.com/sbemails/58-dragon) video? If you can remember words, then you can use Byname. 

Rather than relying on a million open tabs or remembering complex URLs, simply create semantically worded links you can actually remember.

byname is meant to scale from a simple household organization tool to serving full teams and projects. Simply type `me/wiki` in any
browser and land on your wiki.

Hosting is simple and flexible: one small Go program with a SQLite database, running on
a Raspberry Pi at home or a $5/month cloud server.

## Who can use it

People reach byname in up to three ways, and you choose which ones are on.

| Path | Address people use | Sign-in |
|---|---|---|
| **Your own domain** (cloud server or Pi) | `https://me.example.com/wiki`, from any browser anywhere | Slack or Google; only accounts on your allow list |
| **Your tailnet** (Tailscale) | `me/wiki` on devices running Tailscale | none: Tailscale identifies each person and device |
| **Your home network** (Pi only) | `me/wiki` on home Wi-Fi | none; changes are recorded by IP address |

Tailscale is optional. With your own domain and Slack sign-in, a server needs
nothing else. A Pi can use the same sign-in as a cloud server: forward ports
80 and 443 from your router (`--public-domain`), or use Tailscale Funnel
(`--funnel`) and open no ports at all.

`me` is the default prefix people type. Choose your own with one setting:
`--name hg` makes it `hg/wiki`.

## Features

**Links**

- `me/wiki` redirects to the link's destination. Anything after the name is
  passed on: `me/wiki/some/page?x=1` goes to the destination with
  `/some/page?x=1` appended.
- Templates: a destination like `https://github.com/{1}` makes `me/gh/octocat`
  go to `https://github.com/octocat`. `{1}`…`{9}` and `{*}` are available, and
  a placeholder can never change the host.
- Names ignore case, dashes and underscores, so `me/Wiki-Home` and
  `me/wikihome` are the same link.
- `me/wiki+` shows a link's details and history instead of redirecting.
- A name that doesn't exist yet opens a "create it" form with suggestions.
- The home page has search, popular links, the newest links and recent
  changes. Browsers can add byname as a search engine.

**Accountability without admins**

- Every change is recorded with who made it: the Tailscale user and device,
  the Slack or Google account, or the IP address on a home network.
- The history can't be rewritten: the database itself refuses edits and
  deletions of it.
- Any earlier version of a link can be restored with one click. Deleted
  links can be restored too.
- Editing limits per device and per account stop runaway scripts.

**Sign-in for the internet**

- Google and Slack (OpenID Connect). Allow specific Google accounts or whole
  Google Workspace domains, or members of specific Slack workspaces.
- With one provider configured, people go straight to it.
- `byname sessions` lists who is signed in; `byname sessions revoke` signs
  someone out everywhere.

**Small and low-maintenance**

- One static binary, about 30 MB of memory, and almost no disk writes when
  idle.
- Automatic OS and security updates on the server.
- A daily database snapshot (and one before every deploy), uploaded to a
  versioned S3 bucket with a key that can only add files.
- Deploys check the new version's health and roll back on their own if it
  fails.
- JSON export and import, and a small JSON API.

## Deploying

### Choose a host

| Host | Per month | Good for |
|---|---|---|
| AWS Lightsail with your own domain | $5.00 | a team or family anywhere, with Slack or Google sign-in; no Tailscale needed |
| AWS Lightsail, Tailscale only | $3.50 | a household that already uses Tailscale |
| Raspberry Pi 3B at home | about $0.30 of electricity | Tailscale devices plus, optionally, everything on the home network. Add Slack or Google sign-in for the internet with `--public-domain` (router port-forwarding) or `--funnel` (no open ports) |
| Hetzner Cloud | €5.99 and up | either cloud setup, if you'd rather not use AWS |

Prices as of 2026-11-24. The cloud servers are created with OpenTofu from
[`deploy/cloud`](deploy/cloud). Every host is a Debian 13 machine set up by the
same scripts.

### The steps

Every deployment follows the same five steps.

1. **Create the server.** For the cloud, run `tofu apply` in
   `deploy/cloud/aws` (or `hetzner`). For a Pi, flash Raspberry Pi OS Lite.
2. **Point a name at it.** For your own domain, add an A and an AAAA record
   at your DNS host (`tofu output dns_records` prints them). For a Pi, point
   an A record at your home's public IP and forward ports 80 and 443. With Tailscale,
   the server joins your tailnet as `me`.
3. **Harden it.** Run `deploy/host/bootstrap.sh` on the server. It sets up a
   default-deny firewall, key-only SSH, automatic updates and the systemd
   sandbox. Flags choose the paths above: `--no-tailscale`,
   `--public-domain me.example.com`, `--lan-access`, `--funnel`.
4. **Deploy.** Run `make deploy TARGET=admin@me.example.com` from your Mac. It
   runs the tests, builds the binary for the server's CPU, uploads it,
   switches over and checks its health.
5. **Verify.** Run `byname-verify` on the server. It checks the firewall,
   sandbox, sign-in, certificate and backups, and lists the checks to run
   from other devices.

For example, the whole setup for your own domain with Slack sign-in and no
Tailscale looks like this, run from the repo root:

```sh
# deploy/cloud/aws/terraform.tfvars from domain.tfvars.example: name, public_domain, ipv4 = true, ssh_cidrs (see docs/cloud.md)
cd deploy/cloud/aws && tofu init && tofu apply && tofu output dns_records && cd -   # then add the DNS records
git archive HEAD deploy/host deploy/systemd | ssh admin@me.example.com tar -x       # only what bootstrap needs
ssh admin@me.example.com 'sudo ./deploy/host/bootstrap.sh --admin-user admin --name hg \
  --no-tailscale --ssh-from any'
# add BYNAME_SLACK_CLIENT_ID / BYNAME_SLACK_TEAMS and the Slack secret (docs/public-access.md)
ssh admin@me.example.com 'sudo ./deploy/host/bootstrap.sh --admin-user admin --name hg \
  --no-tailscale --ssh-from any --public-domain me.example.com'
make deploy TARGET=admin@me.example.com
ssh admin@me.example.com sudo byname-verify
```

Only `deploy/host` and `deploy/systemd` go to the server. OpenTofu's state,
providers and `terraform.tfvars` stay on your Mac.

Updates are one command: `make deploy TARGET=…` from `main`.

On a Pi, SSH answers only the home network (or the tailnet), so deploy from
home or over Tailscale: `make deploy TARGET=you@me`, not the public domain.

### Guides

| Guide | Covers |
|---|---|
| [docs/cloud.md](docs/cloud.md) | AWS Lightsail and Hetzner: costs, a step-by-step walkthrough with your own domain, the Tailscale variant, moving between hosts |
| [docs/public-access.md](docs/public-access.md) | your own domain or Tailscale Funnel, on a cloud server or a Pi; setting up Slack and Google sign-in; managing who can sign in |
| [docs/operations.md](docs/operations.md) | the Raspberry Pi from scratch, including [sign-in from the internet](docs/operations.md#8-optional-sign-in-from-the-internet); deploys and rollback, S3 backups and restores, troubleshooting |
| [docs/client-setup.md](docs/client-setup.md) | making `me/wiki` work in each browser and device |
| [docs/security-review.md](docs/security-review.md) | the security reviews: what was found, what was fixed, which risks are accepted |

## Security in brief

- **Plain-HTTP address (tailnet and LAN):**
  - Only peers on the tailnet (or the home LAN, if enabled) are accepted.
  - The `Host` header must be one of byname's names, which blocks DNS
    rebinding.
  - Every write must come from byname's own pages, enforced with Go's
    cross-origin protection.
- **Public address:**
  - Everything but the sign-in pages needs a session from an allow-listed
    account.
  - Sessions are kept on the server, in `__Host-` cookies that are Secure,
    HttpOnly and SameSite=Lax.
  - On your own domain, byname terminates TLS itself. It trusts no proxy
    headers, and limits connections per address.
- **Destinations** must be plain `http(s)` URLs with no embedded credentials.
  Redirects are never cached, so edits take effect at once.
- **On the host:**
  - The service runs as its own user under a strict systemd sandbox, behind a
    default-deny firewall and, in the cloud, the provider's firewall.
  - The cloud firewall opens only the ports the chosen setup needs.
  - The backup key can only add objects to S3.

Details, findings and accepted risks: [docs/security-review.md](docs/security-review.md).

## Development

Needs Go 1.25 or later and shellcheck; OpenTofu and the AWS CLI for cloud
deployments.

```sh
make run          # http://localhost:8080 in dev mode (serve --dev: localhost fully trusted)
make gate         # vet, staticcheck, race tests, govulncheck, shellcheck
make fuzz         # fuzz URL validation and template expansion
make cloud-check  # tofu fmt, validate and offline tests for both cloud stacks
make host-test    # bootstrap, firewall, install and restore in a Debian 13 container (needs Docker)
```

```
cmd/byname/        serve | snapshot | backup | export | import | sessions | version
internal/link/     names, destination validation, template expansion
internal/store/    SQLite: links, append-only history, sessions, snapshots
internal/app/      validation, click counting, snapshot scheduling
internal/web/      HTTP: middleware, pages, JSON API, the public address
internal/auth/     Google and Slack sign-in (OpenID Connect)
internal/whois/    Tailscale identity lookups
internal/backup/   S3 upload of the newest snapshot
deploy/host/       bootstrap, firewall, install, verify and restore scripts
deploy/systemd/    service units
deploy/cloud/      OpenTofu for AWS Lightsail and Hetzner, plus first boot
```
