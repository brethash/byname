# Running byname in the cloud

The same service and the same scripts, on a small server at AWS (Lightsail)
or Hetzner instead of a Raspberry Pi at home. There are two ways to run it,
and you can combine them:

| | **Your own domain, no Tailscale** | **Tailscale** |
|---|---|---|
| People use | `https://me.example.com/wiki`, after signing in with Slack or Google | `me/wiki` on devices running Tailscale, no sign-in |
| You administer it with | OpenSSH, open only to your own addresses (`ssh_cidrs`) | Tailscale SSH over the tailnet |
| Open to the internet | TCP 80 and 443; TCP 22 from your addresses | nothing (only Tailscale's WireGuard port), plus TCP 22 from your addresses if you set `ssh_cidrs` |
| Server | needs IPv4 ($5.00 on Lightsail) | IPv6-only is enough ($3.50) |

**Combining them.** Set `tailscale = true` *and* `public_domain` (start from
`domain.tfvars.example` and add `tailscale = true`). Tailnet
devices then use `me/` with no sign-in, and everyone else uses your domain
and signs in. A Tailscale-only server can also get a public address without
open ports through Tailscale Funnel ([public-access.md](public-access.md)).

Compared with the Pi:

- **No home network**, so `--lan-access` doesn't exist here.
- **No hardware, no home outages.** A power cut or ISP outage at home no
  longer takes the links down for family members who are away.
- **A monthly bill**, a few dollars (below).

`me` is the default service name everywhere below. If you set another `name`
(the walkthrough uses `hg`), use it wherever these docs say `me` in a
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
times as much. Hetzner gives you more RAM and traffic, which byname doesn't use.
Choose Hetzner if you'd rather keep this off AWS. Its EU locations add about
100–150 ms to every redirect from North America.

The Hetzner stack picks the cheapest server type Hetzner can actually deliver
in your location when you create the server (`server_type_preference`,
cheapest first), because the cheapest types are often out of stock.

## Your own domain, no Tailscale (walkthrough)

This example runs the service as `hg` at `https://me.example.com` with Slack
sign-in, on Lightsail. Hetzner works the same way with
`deploy/cloud/hetzner`.

**1. Before you start.**

- `brew install opentofu`, and your usual AWS CLI credentials (`aws
  configure` or `AWS_PROFILE`).
- An SSH key (`~/.ssh/id_ed25519.pub`).
- Your home IP address: `curl -s https://checkip.amazonaws.com`. AWS's
  firewall lets only this address reach SSH. If it changes, update
  `ssh_cidrs` and run `tofu apply` again. Add your home IPv6 prefix too if
  your network has IPv6 (for example the /56 your router hands out).
  Without one, SSH over IPv6 stays closed, and `ssh admin@me.example.com`
  from a network with IPv6 tries the closed IPv6 address first and waits for
  it to time out. Tell `ssh` to use IPv4 for the domain in `~/.ssh/config`:
  ```
  Host me.example.com
    AddressFamily inet
  ```
- Access to your domain's DNS.
- A Slack app. Its redirect URL is `https://me.example.com/_/auth/slack/callback`
  ([public-access.md → Slack](public-access.md#slack)).

**2. Create the server.**

The same settings as `domain.tfvars.example`, with this example's values:

```sh
cd deploy/cloud/aws
cat > terraform.tfvars <<'EOF'
name           = "hg"
region         = "us-west-2"
ssh_public_key = "ssh-ed25519 AAAA... you@laptop"
public_domain  = "me.example.com"
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
TCP 22 from `ssh_cidrs` only, over IPv4 and IPv6 alike. Lightsail opens an
address family that a rule leaves out to everyone, so when `ssh_cidrs` has
no IPv6 prefix, the SSH rule lists `2001:db8::/128` for IPv6 instead: a
documentation address that is never routed, so nobody can connect from it.
After each apply, OpenTofu also fails if AWS's stored rules let SSH in from
the whole internet. To see the rules:
`aws lightsail get-instance-port-states --instance-name hg`.

From now on, OpenTofu refuses any apply that would destroy or replace the
server, and with it the database (`protect`, see
[Replacing or tearing down the server](#replacing-or-tearing-down-the-server)).

**3. DNS.** At your DNS host, create the two records `dns_records` printed:

```
me.example.com  A     <static IPv4>
me.example.com  AAAA  <IPv6>
```

Wait until `dig +short me.example.com` shows them.

**4. Bootstrap, then configure sign-in, then turn on the domain.** Run these
from the repo root. `tofu output -raw next_steps` prints them with your
values filled in.

```sh
git archive HEAD deploy/host deploy/systemd | ssh admin@me.example.com tar -x
ssh admin@me.example.com 'sudo ./deploy/host/bootstrap.sh --admin-user admin --name hg \
  --no-tailscale --ssh-from any'

ssh admin@me.example.com 'sudo tee -a /etc/byname/byname.local.env' <<'EOF'
BYNAME_SLACK_CLIENT_ID=1234567890.1234567890
BYNAME_SLACK_TEAMS=T0123ABCD
EOF
pbpaste | ssh admin@me.example.com 'sudo sh -c "umask 077; cat > /etc/credstore/byname.slack-client-secret"'

ssh admin@me.example.com 'sudo ./deploy/host/bootstrap.sh --admin-user admin --name hg \
  --no-tailscale --ssh-from any --public-domain me.example.com'
make deploy TARGET=admin@me.example.com
ssh admin@me.example.com sudo byname-verify
```

The first line copies only what `bootstrap.sh` needs to `~/deploy` on the
server: `deploy/host` and `deploy/systemd`, as committed. Never copy
`deploy/cloud` there: OpenTofu's state (which holds the server's user data),
its providers and your `terraform.tfvars` stay on your Mac.

The `pbpaste` line sends the Slack client secret from your clipboard. The
first visit to `https://me.example.com` gets byname its certificate from Let's
Encrypt; `byname-verify` makes that visit for you.

**5. Check from a phone**, as in [public-access.md](public-access.md#4-turn-it-on).

Without Tailscale, the history records each change with the Slack account
that made it, for example `alice@example.com (via Slack)`.

## With Tailscale

**Before you start**

1. **OpenTofu**: `brew install opentofu` (the commands below use `tofu`).
2. **Tailscale policy**: merge
   [`deploy/tailscale/policy.hujson`](../deploy/tailscale/policy.hujson) into
   your tailnet policy. Use `tag:<name>`, and `"admin"` as the SSH user (that
   is the admin account on cloud servers). Keep its `tcp:22` grant: Tailscale
   SSH needs that network rule as well as the `ssh` rule once the default
   allow-all rule is gone.
3. **No other device named `me`** on your tailnet, or the new server becomes
   `me-1`. If you're moving from the Pi, follow
   [Moving between hosts](#moving-between-hosts) first.
4. **A one-off auth key**: admin console → Settings → Keys → *Generate auth
   key*. Set Reusable off, Ephemeral off, Pre-approved on, Tags `tag:me`, and
   a 1-day expiry. The server uses it once on first boot.
5. **Provider credentials.** For AWS, your usual CLI credentials
   (`aws configure` or `AWS_PROFILE`). For Hetzner, a project API token with
   Read & Write.

**Create the server**

AWS Lightsail:

```sh
cd deploy/cloud/aws
cp tailscale.tfvars.example terraform.tfvars    # set name, region, ssh_public_key
export TF_VAR_tailscale_auth_key=tskey-auth-...
tofu init
tofu apply
```

Hetzner:

```sh
cd deploy/cloud/hetzner
cp tailscale.tfvars.example terraform.tfvars    # set name, location, ssh_public_key
export TF_VAR_hcloud_token=... TF_VAR_tailscale_auth_key=tskey-auth-...
tofu init
tofu apply                                      # shows which server type was in stock
```

First boot takes a minute or two. It sets the hostname, creates an `admin`
account with your SSH key, installs Tailscale and joins your tailnet as `me`
with Tailscale SSH on. Its log is `/var/log/cloud-init-output.log`. Once
`tofu apply` finishes, only Tailscale's WireGuard port (UDP 41641) is open
from the internet (plus TCP 22 from `ssh_cidrs`, if you set it), and
WireGuard never answers anyone it doesn't know.

On later `tofu apply` runs the auth key is no longer read, but the variable
still needs some well-formed value: `TF_VAR_tailscale_auth_key=tskey-auth-unused`.

**Bootstrap, deploy, verify**

`tofu output -raw next_steps` prints these with your name filled in. Run
them from the repo root:

```sh
tailscale ping me
git archive HEAD deploy/host deploy/systemd | ssh admin@me tar -x
ssh admin@me 'sudo ./deploy/host/bootstrap.sh --admin-user admin --name me --tag tag:me'
make deploy TARGET=admin@me
ssh admin@me sudo byname-verify
```

The second line copies only `deploy/host` and `deploy/systemd`, as in the
walkthrough above; OpenTofu's files stay on your Mac.

Then check a household device as in
[operations.md, step 7](operations.md#7-check-every-household-device), using
the Tailscale steps only.

## Either way

**First boot.** A new Lightsail instance starts with AWS's default firewall
(SSH and HTTP open) for the minute or so until OpenTofu applies the port
rules. SSH accepts only your key during that window, and nothing answers on
HTTP yet. Hetzner attaches its firewall at creation.

**If first boot fails.** For example, a server with Tailscale never joins
the tailnet (`tailscale ping me` gets no answer after five minutes), because
the auth key expired, wasn't pre-approved or tagged `tag:me`, or your policy
doesn't define the tag. Where you can still log in, the log says why:
`ssh admin@SERVER tail -50 /var/log/cloud-init-output.log`. A new server
holds nothing yet, so the fix is to replace it. Delete any machine it left
in the Tailscale admin console first, so the new one gets the name `me`,
not `me-1`. Then, from `deploy/cloud/aws` (or `hetzner`):

```sh
export TF_VAR_tailscale_auth_key=tskey-auth-...    # a new one-off key, if it uses Tailscale
tofu apply -var protect=false                      # protection off, with no other change
tofu apply -var protect=false -replace=aws_lightsail_instance.server   # Hetzner: -replace=hcloud_server.server
tofu apply                                         # protection back on
```

The new server gets new user data, so it runs first boot again with the new
key. On AWS, its firewall rules and static IP are re-applied with it.

**The server's `name` is fixed** once it exists. Changing it would replace
the server, which `protect` refuses. To rename, create a new server and
move the data over.

**Set up with `scp -r deploy`?** Earlier versions of these steps copied the
whole `deploy` directory to the server, including OpenTofu's state,
providers and `terraform.tfvars`. Remove that copy:
`ssh admin@SERVER rm -rf deploy/cloud`.

**What `bootstrap.sh` does differently on a cloud server:**

- no LAN options;
- with Tailscale, you log in with Tailscale SSH. While it is on, tailscaled
  answers port 22 on the tailnet itself, so OpenSSH is reachable over the
  tailnet only after `sudo tailscale set --ssh=false`;
- OpenSSH from the internet as `--ssh-from` says: required without
  Tailscale, optional with it (`next_steps` adds it whenever `ssh_cidrs` is
  set). `--ssh-from any` leaves the address filter to the provider's
  firewall (`ssh_cidrs`), so a new home address is fixed with `tofu apply`.
  A specific address filters on the server too, but locks you out if your
  address changes;
- the journal kept on disk, capped at 200 MB;
- a 512 MB swap file on small servers.

**Deploys.** `make deploy` builds arm64 and amd64 binaries, and ships the one
that matches the server (Lightsail and the CX/CPX lines are amd64, CAX is
arm64). `byname-verify` checks the same baseline as on the Pi, minus the Pi
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
through a Tailscale relay (DERP) instead of directly. `tailscale ping me`
shows `via DERP(...)` for a relayed path and an address for a direct one.
Changing `ipv4` on Lightsail later replaces the server, which `protect`
refuses until you turn it off.

## Moving between hosts

Moving between the Pi and the cloud (in either direction) keeps every link
and its full history:

1. **Take a snapshot on the old host** and copy it to your Mac:
   ```sh
   ssh you@me 'sudo -u byname /usr/local/lib/byname/current/byname snapshot'     # prints the snapshot's path
   ssh you@me 'sudo cat <that path>' > byname-move.db
   ```
2. **Stop the old host**: `ssh you@me 'sudo systemctl disable --now byname'`.
   With Tailscale, also free the name: `ssh you@me 'sudo tailscale logout'`,
   then delete the machine in the Tailscale admin console.
3. **Create the new host** (above, or the Pi runbook), then bootstrap and
   deploy it. For your own domain, point its DNS at the new server.
4. **Restore on the new host**:
   ```sh
   scp byname-move.db admin@SERVER:
   ssh admin@SERVER 'sudo byname-restore byname-move.db'
   ```

Tailscale devices keep working with no changes, because they only know the
name. Home devices without Tailscale lose access when you move off the Pi.

## Replacing or tearing down the server

The database lives on the server's disk, so both stacks protect the server
by default (`protect = true`). On Hetzner, that is Hetzner's own delete and
rebuild protection. Lightsail has none, so the AWS stack stops any apply
that would destroy or replace the server, before anything is touched:
`tofu destroy`, `-replace`, or a changed `name`, `bundle_id`, `ipv4` or
`availability_zone`.

To do it on purpose, turn protection off in an apply of its own, then run
the destroying command with it still off:

```sh
tofu apply -var protect=false      # with no other change
tofu destroy -var protect=false    # or the apply that replaces the server
```

- **Replacing** a server that holds links: take a snapshot first and
  restore it on the new server, as in [Moving between hosts](#moving-between-hosts).
  On AWS, a replacement keeps the static IP unless `name` changed. Afterwards
  a plain `tofu apply` turns protection back on.
- **Tearing down**: on AWS, `tofu destroy` releases the static IP too. Then
  remove the machine from the Tailscale admin console (if it was on the
  tailnet) and the DNS records (if it had a domain). The S3 backups stay.

## Security differences from the Pi

[security-review.md](security-review.md) covers them:

- the provider can read the disk;
- with Tailscale, the auth key sits in user data;
- break-glass access is OpenSSH from `ssh_cidrs`, or rebuilding and
  restoring, instead of plugging in a keyboard;
- without Tailscale, SSH and the web ports face the internet: SSH limited to
  your addresses, the web ports to anyone.
