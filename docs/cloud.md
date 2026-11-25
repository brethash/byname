# Running ot in the cloud

The same service and the same scripts, on a small server at AWS (Lightsail)
or Hetzner instead of a Raspberry Pi at home. The server joins your tailnet
on first boot and is reachable only through it.

Compared with the Pi:

- **Tailnet only.** There is no home network, so `--lan-access` doesn't
  exist here: devices without Tailscale can't use a cloud-hosted ot.
- **No hardware, no home outages.** A power cut or ISP outage at home no
  longer takes the links down for family members who are away.
- **A monthly bill**, a few dollars (below).

Either host can also be published on the internet with Google or Slack
sign-in ([public-access.md](public-access.md)). Sign in with Slack needs IPv4
(`slack.com` has no IPv6 address), so set `ipv4 = true` for it.

`ot` is the default service name everywhere below. If you set another `name`,
use it wherever these docs say `ot` in a hostname, URL or tag.

## What it costs

Prices checked on 2026-11-24 (AWS price list API and `aws lightsail
get-bundles`; Hetzner's own price pages). Hetzner prices exclude VAT.

| Option | Per month | Notes |
|---|---|---|
| **AWS Lightsail nano, IPv6-only** (default) | **$3.50** | 512 MB, 2 vCPU burst, 20 GB SSD, 1 TB transfer, x86 |
| AWS Lightsail nano with IPv4 (`ipv4 = true`) | $5.00 | Not needed (see [IPv6-only](#ipv6-only)) |
| AWS EC2 t4g.nano + 8 GB gp3 (not used) | $3.71 IPv6-only, $7.36 with IPv4 | More expensive, and IPv6-only Debian on EC2 has had cloud-init problems |
| Hetzner CX23 (EU) | €5.99 | Hetzner listed it as unavailable on 2026-11-24 |
| Hetzner CAX11 (EU, arm64) | €6.49 | Also listed as unavailable |
| Hetzner CPX12 (EU) | €9.99 in hel1, €11.99 in fsn1 and nbg1 | |
| Hetzner CPX11 (US: ash, hil) | €17.99 | The only US option |
| S3 backups | under $0.01 | Either provider; see [Backups](#backups) |
| For comparison: the Pi at home | about $0.30 of electricity | Hardware you already own |

**AWS Lightsail is the cheaper choice** by a wide margin. Hetzner's cheapest
type, when it is in stock, is €5.99 against $3.50: about 70% more even if a
euro were worth only a dollar. Its US locations cost five times as much. Hetzner gives you
more RAM and traffic, which ot doesn't use. Choose Hetzner if you'd rather
keep this off AWS. Its EU locations add about 100–150 ms to every redirect
from North America.

The Hetzner stack picks the cheapest server type Hetzner can actually deliver
in your location when you create the server (`server_type_preference`,
cheapest first), because the cheapest types are often out of stock.

## Before you start

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

## Create the server

AWS Lightsail:

```sh
cd deploy/cloud/aws
cp terraform.tfvars.example terraform.tfvars    # set name, region, ssh_public_key
export TF_VAR_tailscale_auth_key=tskey-auth-...
tofu init
tofu apply
```

Hetzner:

```sh
cd deploy/cloud/hetzner
cp terraform.tfvars.example terraform.tfvars    # set name, location, ssh_public_key
export TF_VAR_hcloud_token=... TF_VAR_tailscale_auth_key=tskey-auth-...
tofu init
tofu apply                                      # shows which server type was in stock
```

First boot takes a minute or two. It sets the hostname, creates an `admin`
account with your SSH key, installs Tailscale and joins your tailnet as `ot`
with Tailscale SSH on. Its log is `/var/log/cloud-init-output.log`. Once
`tofu apply` finishes, only Tailscale's WireGuard port (UDP 41641) is open
from the internet, and WireGuard never answers anyone it doesn't know. On
Lightsail, the new instance starts with AWS's default firewall (SSH and HTTP
open) for the minute or so until OpenTofu applies the port rules; SSH accepts
only your key during that window. Hetzner attaches its firewall at creation.

The server's `name` is fixed once it exists. To rename, create a new server
and move the data over.

On later `tofu apply` runs the auth key is no longer read, but the variable
still needs some well-formed value: `TF_VAR_tailscale_auth_key=tskey-auth-unused`.

## Bootstrap, deploy, verify

`tofu output -raw next_steps` prints these with your name filled in. Run
them from the repo root:

```sh
tailscale ping ot
scp -r deploy admin@ot:
ssh admin@ot 'sudo ./deploy/host/bootstrap.sh --admin-user admin --name ot --tag tag:ot'
make deploy TARGET=admin@ot
ssh admin@ot sudo ot-verify
```

`bootstrap.sh` sees that this isn't a Pi and adjusts:

- no LAN options;
- OpenSSH reachable only over the tailnet, as a fallback to Tailscale SSH;
- the journal kept on disk, capped at 200 MB;
- a 512 MB swap file on small servers.

`make deploy` builds arm64 and amd64 binaries, and ships the one that matches
the server (Lightsail and the CX/CPX lines are amd64, CAX is arm64).
`ot-verify` checks the same baseline as on the Pi, minus the Pi hardware
checks. At the end it lists the public addresses that must time out from
outside the tailnet.

Then check a household device as in
[operations.md, step 7](operations.md#7-check-every-household-device), using
the Tailscale steps only.

## Backups

These work exactly as on the Pi
([operations.md → Backups](operations.md#backups-s3)): a versioned S3
bucket, and an access key that can only add objects. The backup unit uses
S3's dual-stack endpoints, so IPv6-only servers reach S3 too.

## IPv6-only

Both stacks default to no public IPv4 address. That saves $1.50/month on
Lightsail and Hetzner's IPv4 fee, and leaves no IPv4 address to scan.
Everything the server talks to works over IPv6: Tailscale's coordination and
relay servers, the Debian and Tailscale package mirrors, and S3.

One exception: Sign in with Slack ([public-access.md](public-access.md))
needs IPv4, because `slack.com` has no IPv6 address (checked 2026-11-25).
Google sign-in works over IPv6.

Devices on IPv4-only networks still connect, through a Tailscale relay
(DERP) instead of directly. `tailscale ping ot` shows `via DERP(...)` for a
relayed path and an address for a direct one. A nearby relay adds a few
milliseconds. Most US home ISPs and mobile carriers provide IPv6. If you'd
rather have direct paths everywhere, set `ipv4 = true` before creating the
server (on Lightsail, changing it later replaces the server).

## Moving between hosts

Moving between the Pi and the cloud (in either direction) keeps every link
and its full history:

1. **Take a snapshot on the old host** and copy it to your Mac:
   ```sh
   ssh you@ot 'sudo -u ot /usr/local/lib/ot/current/ot snapshot'     # prints the snapshot's path
   ssh you@ot 'sudo cat <that path>' > ot-move.db
   ```
2. **Stop the old host and free the name**:
   `ssh you@ot 'sudo systemctl disable --now ot; sudo tailscale logout'`.
   Then delete the machine in the Tailscale admin console.
3. **Create the new host** (above, or the Pi runbook), then bootstrap and
   deploy it.
4. **Restore on the new host**:
   ```sh
   scp ot-move.db admin@ot:
   ssh admin@ot 'sudo ot-restore ot-move.db'
   ```

Devices keep working with no changes, because they only know the name. Home
devices without Tailscale lose access when you move off the Pi.

## Tear down

- **AWS**: `tofu destroy`.
- **Hetzner**: set `protect = false`, run `tofu apply`, then `tofu destroy`.

Then remove the machine from the Tailscale admin console. The S3 backups
stay.

## Security differences from the Pi

[security-review.md](security-review.md#cloud-hosting-2026-11-24) covers them:
the provider can read the disk, the auth key sits in user data, and
break-glass access means rebuilding and restoring instead of plugging in a
keyboard.
