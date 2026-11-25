# Running ot in the cloud

The same service and the same scripts, on a small server at AWS (Lightsail)
or Hetzner instead of a Raspberry Pi at home. There are two ways to run it,
and you can combine them:

| | **Your own domain, no Tailscale** | **Tailscale** |
|---|---|---|
| People use | `https://hg.example.com/wiki`, after signing in with Slack or Google | `ot/wiki` on devices running Tailscale, no sign-in |
| You administer it with | OpenSSH, open only to your own addresses (`ssh_cidrs`) | Tailscale SSH over the tailnet |
| Open to the internet | TCP 80 and 443; TCP 22 from your addresses | nothing (only Tailscale's WireGuard port) |
| Server | needs IPv4 ($5.00 on Lightsail) | IPv6-only is enough ($3.50) |

**Combining them.** Set `tailscale = true` *and* `public_domain`. Tailnet
devices then use `ot/` with no sign-in, and everyone else uses your domain
and signs in. A Tailscale-only server can also get a public address without
open ports through Tailscale Funnel ([public-access.md](public-access.md)).

Compared with the Pi:

- **No home network**, so `--lan-access` doesn't exist here.
- **No hardware, no home outages.** A power cut or ISP outage at home no
  longer takes the links down for family members who are away.
- **A monthly bill**, a few dollars (below).

`ot` is the default service name everywhere below. If you set another `name`
(the walkthrough uses `hg`), use it wherever these docs say `ot` in a
hostname, URL or tag.

## What it costs

Prices checked on 2026-11-24 (AWS price list API and `aws lightsail
get-bundles`; Hetzner's own price pages). Hetzner prices exclude VAT.

| Option | Per month | Notes |
|---|---|---|
| AWS Lightsail nano, IPv6-only | $3.50 | 512 MB, 2 vCPU burst, 20 GB SSD, 1 TB transfer, x86. Enough with Tailscale only |
| **AWS Lightsail nano with IPv4** (`ipv4 = true`) | **$5.00** | Needed for your own domain and for Sign in with Slack. The static IP is free while attached |
| AWS EC2 t4g.nano + 8 GB gp3 (not used) | $3.71 IPv6-only, $7.36 with IPv4 | More expensive, and IPv6-only Debian on EC2 has had cloud-init problems |
| Hetzner CX23 (EU) | €5.99 + IPv4 | Hetzner listed it as unavailable on 2026-11-24 |
| Hetzner CAX11 (EU, arm64) | €6.49 + IPv4 | Also listed as unavailable |
| Hetzner CPX12 (EU) | €9.99 in hel1, €11.99 in fsn1 and nbg1, + IPv4 | |
| Hetzner CPX11 (US: ash, hil) | €17.99 + IPv4 | The only US option |
| S3 backups | under $0.01 | Either provider; see [Backups](#backups) |
| For comparison: the Pi at home | about $0.30 of electricity | Hardware you already own |

**AWS Lightsail is the cheaper choice** by a wide margin, with or without
IPv4. Hetzner's cheapest type, when it is in stock, is €5.99 before its IPv4
fee, against $5.00 for Lightsail with IPv4. Its US locations cost several
times as much. Hetzner gives you more RAM and traffic, which ot doesn't use.
Choose Hetzner if you'd rather keep this off AWS. Its EU locations add about
100–150 ms to every redirect from North America.

The Hetzner stack picks the cheapest server type Hetzner can actually deliver
in your location when you create the server (`server_type_preference`,
cheapest first), because the cheapest types are often out of stock.

## Your own domain, no Tailscale (walkthrough)

This example runs the service as `hg` at `https://hg.example.com` with Slack
sign-in, on Lightsail. Hetzner works the same way with
`deploy/cloud/hetzner`.

**1. Before you start.**

- `brew install opentofu`, and your usual AWS CLI credentials (`aws
  configure` or `AWS_PROFILE`).
- An SSH key (`~/.ssh/id_ed25519.pub`).
- Your home IP address: `curl -s https://checkip.amazonaws.com`. AWS's
  firewall lets only this address reach SSH. If it changes, update
  `ssh_cidrs` and run `tofu apply` again. With IPv4 on, `ssh_cidrs` needs at
  least one IPv4 block. You can add your home IPv6 prefix as well.
- Access to your domain's DNS.
- A Slack app. Its redirect URL is `https://hg.example.com/_/auth/slack/callback`
  ([public-access.md → Slack](public-access.md#slack)).

**2. Create the server.**

```sh
cd deploy/cloud/aws
cat > terraform.tfvars <<'EOF'
name           = "hg"
region         = "us-west-2"
ssh_public_key = "ssh-ed25519 AAAA... you@laptop"
public_domain  = "hg.example.com"
ipv4           = true
ssh_cidrs      = ["203.0.113.7/32"]
EOF
tofu init
tofu apply
tofu output dns_records
```

OpenTofu refuses a server you couldn't reach or nobody could use. For
example, it refuses no Tailscale without `ssh_cidrs`, and `public_domain`
without `ipv4`. AWS's firewall allows TCP 80 and 443 from anywhere, and
TCP 22 from `ssh_cidrs` only. After each apply, OpenTofu fails if AWS's
stored rules let SSH in from the whole internet. To see the rules:
`aws lightsail get-instance-port-states --instance-name hg`.

**3. DNS.** At your DNS host, create the two records `dns_records` printed:

```
hg.example.com  A     <static IPv4>
hg.example.com  AAAA  <IPv6>
```

Wait until `dig +short hg.example.com` shows them.

**4. Bootstrap, then configure sign-in, then turn on the domain.** Run these
from the repo root. `tofu output -raw next_steps` prints them with your
values filled in.

```sh
scp -r deploy admin@hg.example.com:
ssh admin@hg.example.com 'sudo ./deploy/host/bootstrap.sh --admin-user admin --name hg \
  --no-tailscale --ssh-from any'

ssh admin@hg.example.com 'sudo tee -a /etc/ot/ot.local.env' <<'EOF'
OT_SLACK_CLIENT_ID=1234567890.1234567890
OT_SLACK_TEAMS=T0123ABCD
EOF
pbpaste | ssh admin@hg.example.com 'sudo sh -c "umask 077; cat > /etc/credstore/ot.slack-client-secret"'

ssh admin@hg.example.com 'sudo ./deploy/host/bootstrap.sh --admin-user admin --name hg \
  --no-tailscale --ssh-from any --public-domain hg.example.com'
make deploy TARGET=admin@hg.example.com
ssh admin@hg.example.com sudo ot-verify
```

The `pbpaste` line sends the Slack client secret from your clipboard. The
first visit to `https://hg.example.com` gets ot its certificate from Let's
Encrypt; `ot-verify` makes that visit for you.

**5. Check from a phone**, as in [public-access.md](public-access.md#4-turn-it-on).

Without Tailscale, the history records each change with the Slack account
that made it, for example `alice@example.com (via Slack)`.

## With Tailscale

**Before you start**

1. **OpenTofu**: `brew install opentofu` (the commands below use `tofu`).
2. **Tailscale policy**: merge
   [`deploy/tailscale/policy.hujson`](../deploy/tailscale/policy.hujson) into
   your tailnet policy. Use `tag:<name>`, and `"admin"` as the SSH user (that
   is the admin account on cloud servers).
3. **No other device named `ot`** on your tailnet, or the new server becomes
   `ot-1`. If you're moving from the Pi, follow
   [Moving between hosts](#moving-between-hosts) first.
4. **A one-off auth key**: admin console → Settings → Keys → *Generate auth
   key*. Set Reusable off, Ephemeral off, Pre-approved on, Tags `tag:ot`, and
   a 1-day expiry. The server uses it once on first boot.
5. **Provider credentials.** For AWS, your usual CLI credentials
   (`aws configure` or `AWS_PROFILE`). For Hetzner, a project API token with
   Read & Write.

**Create the server**

AWS Lightsail:

```sh
cd deploy/cloud/aws
cp terraform.tfvars.example terraform.tfvars    # set name, region, ssh_public_key, tailscale = true
export TF_VAR_tailscale_auth_key=tskey-auth-...
tofu init
tofu apply
```

Hetzner:

```sh
cd deploy/cloud/hetzner
cp terraform.tfvars.example terraform.tfvars    # set name, location, ssh_public_key, tailscale = true
export TF_VAR_hcloud_token=... TF_VAR_tailscale_auth_key=tskey-auth-...
tofu init
tofu apply                                      # shows which server type was in stock
```

First boot takes a minute or two. It sets the hostname, creates an `admin`
account with your SSH key, installs Tailscale and joins your tailnet as `ot`
with Tailscale SSH on. Its log is `/var/log/cloud-init-output.log`. Once
`tofu apply` finishes, only Tailscale's WireGuard port (UDP 41641) is open
from the internet, and WireGuard never answers anyone it doesn't know.

On later `tofu apply` runs the auth key is no longer read, but the variable
still needs some well-formed value: `TF_VAR_tailscale_auth_key=tskey-auth-unused`.

**Bootstrap, deploy, verify**

`tofu output -raw next_steps` prints these with your name filled in. Run
them from the repo root:

```sh
tailscale ping ot
scp -r deploy admin@ot:
ssh admin@ot 'sudo ./deploy/host/bootstrap.sh --admin-user admin --name ot --tag tag:ot'
make deploy TARGET=admin@ot
ssh admin@ot sudo ot-verify
```

Then check a household device as in
[operations.md, step 7](operations.md#7-check-every-household-device), using
the Tailscale steps only.

## Either way

**First boot.** A new Lightsail instance starts with AWS's default firewall
(SSH and HTTP open) for the minute or so until OpenTofu applies the port
rules. SSH accepts only your key during that window, and nothing answers on
HTTP yet. Hetzner attaches its firewall at creation.

**The server's `name` is fixed** once it exists. To rename, create a new
server and move the data over.

**What `bootstrap.sh` does differently on a cloud server:**

- no LAN options;
- OpenSSH reachable only over the tailnet; without Tailscale, as
  `--ssh-from` says. `--ssh-from any` leaves the address filter to the
  provider's firewall (`ssh_cidrs`), so a new home address is fixed with
  `tofu apply`. A specific address filters on the server too, but locks you
  out if your address changes;
- the journal kept on disk, capped at 200 MB;
- a 512 MB swap file on small servers.

**Deploys.** `make deploy` builds arm64 and amd64 binaries, and ships the one
that matches the server (Lightsail and the CX/CPX lines are amd64, CAX is
arm64). `ot-verify` checks the same baseline as on the Pi, minus the Pi
hardware checks. At the end, it lists what should and shouldn't answer from
the internet.

## Backups

These work exactly as on the Pi
([operations.md → Backups](operations.md#backups-s3)): a versioned S3
bucket, and an access key that can only add objects. The backup unit uses
S3's dual-stack endpoints, so IPv6-only servers reach S3 too.

## IPv6-only

Without a public domain, both stacks default to no public IPv4 address. That
saves $1.50/month on Lightsail and Hetzner's IPv4 fee, and leaves no IPv4
address to scan. Everything a tailnet-only server talks to works over IPv6:
Tailscale's coordination and relay servers, the Debian and Tailscale package
mirrors, S3, and Google sign-in.

Two things need IPv4, which is why `public_domain` requires `ipv4 = true`:

- **A public website**: many visitors' networks are still IPv4-only.
- **Sign in with Slack**: `slack.com` has no IPv6 address (checked
  2026-11-25).

Tailscale devices on IPv4-only networks still reach an IPv6-only server,
through a Tailscale relay (DERP) instead of directly. `tailscale ping ot`
shows `via DERP(...)` for a relayed path and an address for a direct one.
Changing `ipv4` on Lightsail later replaces the server.

## Moving between hosts

Moving between the Pi and the cloud (in either direction) keeps every link
and its full history:

1. **Take a snapshot on the old host** and copy it to your Mac:
   ```sh
   ssh you@ot 'sudo -u ot /usr/local/lib/ot/current/ot snapshot'     # prints the snapshot's path
   ssh you@ot 'sudo cat <that path>' > ot-move.db
   ```
2. **Stop the old host**: `ssh you@ot 'sudo systemctl disable --now ot'`.
   With Tailscale, also free the name: `ssh you@ot 'sudo tailscale logout'`,
   then delete the machine in the Tailscale admin console.
3. **Create the new host** (above, or the Pi runbook), then bootstrap and
   deploy it. For your own domain, point its DNS at the new server.
4. **Restore on the new host**:
   ```sh
   scp ot-move.db admin@SERVER:
   ssh admin@SERVER 'sudo ot-restore ot-move.db'
   ```

Tailscale devices keep working with no changes, because they only know the
name. Home devices without Tailscale lose access when you move off the Pi.

## Tear down

- **AWS**: `tofu destroy`. This releases the static IP too.
- **Hetzner**: set `protect = false`, run `tofu apply`, then `tofu destroy`.

Then remove the machine from the Tailscale admin console (if it was on the
tailnet) and the DNS records (if it had a domain). The S3 backups stay.

## Security differences from the Pi

[security-review.md](security-review.md) covers them:

- the provider can read the disk;
- with Tailscale, the auth key sits in user data;
- break-glass access means rebuilding and restoring instead of plugging in
  a keyboard;
- without Tailscale, SSH and the web ports face the internet: SSH limited to
  your addresses, the web ports to anyone.
