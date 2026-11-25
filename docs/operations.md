# Operating ot on the Raspberry Pi

Everything here runs from the repo on your Mac unless it says "on the Pi".

## What lives where on the Pi

| Path | What |
|---|---|
| `/usr/local/lib/ot/<version>/ot` | deployed binaries (current + 2 previous) |
| `/usr/local/lib/ot/current` | symlink to the live version |
| `/etc/ot/ot.env` | `ot serve` settings (`OT_HOSTS`, …), rewritten by `bootstrap.sh` |
| `/etc/ot/ot.local.env` | your own `OT_*` overrides (kept across bootstrap runs) |
| `/etc/systemd/system/ot.service.d/10-lan-access.conf` | present only with `--lan-access` |
| `/etc/ot/firewall.nft` | nftables table `inet ot_filter` |
| `/etc/ot/ot-backup.env` | backup bucket/region (optional) |
| `/etc/credstore/ot-backup-aws` | backup AWS key (root 0600, optional) |
| `/var/lib/ot/ot.db` | the database (SQLite, WAL) |
| `/var/lib/ot/snapshots/` | `ot-<UTC time>.db` snapshots (last 14) |
| `/usr/local/sbin/ot-verify`, `ot-restore` | checks and restore helper |
| units | `ot.service`, `ot-firewall.service`, `ot-backup.service` + `.timer` |

## First-time setup (milestone M0)

### 1. Tailscale policy (admin console, before the Pi joins)

Merge [`deploy/tailscale/policy.hujson`](../deploy/tailscale/policy.hujson) into
your tailnet policy: it defines `tag:ot`, lets household members reach it on
port 80, and allows you Tailscale SSH to it. Confirm MagicDNS is on
(admin console → DNS). Make sure no other device is already named `ot`.

### 2. Flash the SD card

Raspberry Pi Imager → **Raspberry Pi OS Lite (64-bit)**. In OS customisation:

- hostname `ot`
- your own username (not `pi`) and password
- **Services → Enable SSH → Allow public-key authentication only**, paste your key
- **no** Wi-Fi settings (wired Ethernet only)
- your locale and time zone

Use the official 5.1 V / 2.5 A supply. Under-voltage is the usual cause of SD
corruption, and `ot-verify` checks for it.

### 3. Router: fixed address and a name for LAN devices

Boot the Pi on Ethernet and, in your router:

1. **Reserve its DHCP address** so it never changes.
2. **Make `ot` resolve to that address** for home-network devices. Many
   routers already register DHCP hostnames, so `ot` (or `ot.<router-domain>`,
   e.g. `ot.lan`) may work as soon as the Pi has booted. Check from a device
   with Tailscale off: `nslookup ot`. If it doesn't resolve, add a local DNS /
   static host entry `ot` → the reserved address. Note your router's local
   domain (often `lan` or `home`).

Devices running Tailscale keep using MagicDNS for `ot`; this entry is only
for devices without it.

### 4. Bootstrap over the LAN

```sh
scp -r deploy <you>@<pi-lan-ip>:
ssh <you>@<pi-lan-ip> 'sudo ./deploy/pi/bootstrap.sh --admin-user <you> --lan-subnet 192.168.1.0/24 --lan-access --lan-domain lan'
```

Use your real LAN subnet and router domain. Leave out `--lan-access` to keep
ot tailnet-only. Running the script again without it turns LAN access off.
The script refuses to load the firewall if your SSH session comes from
outside the LAN subnet. It removes `avahi-daemon`, so `ot.local` stops
resolving. Use the LAN IP or, after the next step, `ot` over Tailscale.

### 5. Join the tailnet

```sh
ssh <you>@<pi-lan-ip>
sudo tailscale up --ssh --hostname=ot --advertise-tags=tag:ot   # open the printed URL
sudo ./deploy/pi/bootstrap.sh --admin-user <you> --lan-subnet 192.168.1.0/24 --lan-access --lan-domain lan   # again: fills OT_HOSTS
sudo reboot                                                       # applies Wi-Fi/BT off
```

From now on, `ssh <you>@ot` works from anywhere on the tailnet (Tailscale SSH;
the "check" rule asks you to re-confirm in the browser now and then).

### 6. Deploy and verify

```sh
make deploy PI=<you>@ot
ssh <you>@ot sudo ot-verify
```

`ot-verify` checks the whole baseline: radios off, SSH hardening, updates,
journal in RAM, firewall, LAN access consistent across firewall, systemd and
app, Tailscale name and tag, the sandbox exposure score (≤ 2.0), health and
WhoIs over the tailnet, and the DNS-rebinding guard. Its exit code is the
number of failures.

### 7. Check every household device

On each device with Tailscale (app connected, "Use Tailscale DNS" on):

1. open `http://ot/_/whoami`. It should show that person's Tailscale login.
2. create `ot/test` once, then type `ot/test` in the address bar.

On a device **without** Tailscale on the home Wi-Fi, `http://ot/_/whoami`
should say "home network (no Tailscale)", and `ot/test` should open.

Record what happens in [client-setup.md](client-setup.md). If you have a
guest Wi-Fi, from it `curl -m 5 http://<pi-lan-ip>/` should time out.

## Deploying changes

```sh
make deploy PI=<you>@ot
```

This runs the full gate (vet, staticcheck, race tests, govulncheck,
shellcheck), builds a static arm64 binary, then on the Pi:

1. verifies the binary's sha256;
2. snapshots the database **with the current binary** (before any migration);
3. switches the `current` symlink atomically and restarts `ot`;
4. polls `/_/healthz` for the new version for 20 s; on failure it switches
   back and restarts the previous version.

It refuses a dirty working tree unless you set `FORCE=1`.

If a release that changed the database schema has to be rolled back, the old
binary refuses the newer schema on purpose. Restore the pre-deploy snapshot
that step 2 wrote: `sudo ot-restore $(ls -1 /var/lib/ot/snapshots/ot-*.db | tail -1)`.

## Backups (S3)

`ot serve` writes a snapshot 60 s after any change and every 24 h. The hourly
`ot-backup.timer` uploads the newest one if it hasn't uploaded it yet.

### One-time AWS setup (from your Mac, with admin credentials)

```sh
B=<bucket-name> R=<region>
aws s3api create-bucket --bucket $B --region $R --create-bucket-configuration LocationConstraint=$R   # omit the last flag in us-east-1
aws s3api put-public-access-block --bucket $B --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
aws s3api put-bucket-versioning --bucket $B --versioning-configuration Status=Enabled
aws s3api put-bucket-encryption --bucket $B --server-side-encryption-configuration \
  '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
aws s3api put-bucket-lifecycle-configuration --bucket $B --lifecycle-configuration file://deploy/aws/lifecycle.json

sed "s/BUCKET_NAME/$B/" deploy/aws/iam-policy.json > /tmp/ot-backup-policy.json
aws iam create-user --user-name ot-backup
aws iam put-user-policy --user-name ot-backup --policy-name ot-backup-put --policy-document file:///tmp/ot-backup-policy.json
aws iam create-access-key --user-name ot-backup      # note the key id and secret
```

The key can only `PutObject` (with SSE) under `ot/`. It cannot list, read or
delete, and versioning keeps every earlier object even if a key is
overwritten. A stolen Pi therefore can't destroy the backup history. Cost
for a household's links is a fraction of a cent a month.

### On the Pi

```sh
sudo install -m 0600 /dev/null /etc/credstore/ot-backup-aws
sudo nano /etc/credstore/ot-backup-aws
#   [default]
#   aws_access_key_id = AKIA...
#   aws_secret_access_key = ...
sudo cp /etc/ot/ot-backup.env.example /etc/ot/ot-backup.env && sudo nano /etc/ot/ot-backup.env
sudo systemctl enable --now ot-backup.timer
sudo systemctl start ot-backup.service && journalctl -u ot-backup -n 5
```

## Restore

From a local snapshot (on the Pi):

```sh
ls /var/lib/ot/snapshots/
sudo ot-restore /var/lib/ot/snapshots/ot-20261124T120000.000Z.db
```

From S3 (the Pi's key cannot read, so fetch from your Mac):

```sh
aws s3 ls s3://$B/ot/
aws s3 cp s3://$B/ot/ot-20261124T120000.000Z.db.gz .
scp ot-20261124T120000.000Z.db.gz <you>@ot:
ssh <you>@ot sudo ot-restore ot-20261124T120000.000Z.db.gz
```

`ot-restore` integrity-checks the file, snapshots the current database first
(so the restore itself can be undone), swaps the file and restarts `ot`.

### Restore drill (do this once in M3, then yearly)

Non-destructively, on your Mac:

```sh
aws s3 cp s3://$B/ot/<newest>.db.gz - | gunzip > /tmp/drill.db
bin/ot export --db /tmp/drill.db | head -40     # links are there
```

## Day to day

| Task | Command |
|---|---|
| Logs | `ssh <you>@ot journalctl -u ot -f` (debug detail: `OT_LOG_LEVEL=debug` in `/etc/ot/ot.env`) |
| Health | `curl http://ot/_/healthz` (version, link count, under-voltage flags) |
| Who changed what | `http://ot/_/activity`, or a link's page (`ot/name+`) |
| Undo a bad edit | link page → History → "Restore this version" |
| Export | `http://ot/_/export` or `sudo -u ot /usr/local/lib/ot/current/ot export` |
| Bulk import | `sudo -u ot /usr/local/lib/ot/current/ot import --hosts ot --dry-run file.json` |
| Change settings | edit `/etc/ot/ot.local.env` (yours; `ot.env` is managed), then `sudo systemctl restart ot` |
| Turn LAN access on/off | re-run `bootstrap.sh` with or without `--lan-access` |
| Rotate the AWS key | create a new key, replace `/etc/credstore/ot-backup-aws`, delete the old key |
| Re-check baseline | `sudo ot-verify` |

OS, Raspberry Pi firmware and Tailscale update themselves daily. The Pi
reboots at 04:00 when an update needs it.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Browser searches for `ot/foo` | See [client-setup.md](client-setup.md); type `http://ot/foo` once, or use the keyword shortcut |
| "ot is not reachable from this network" (403) | The device's address isn't allowed: it's outside the LAN subnet (guest Wi-Fi?) without Tailscale, or LAN access is off and Tailscale is off on that device |
| `ot` doesn't resolve on a device without Tailscale | Router DNS has no `ot` entry; see step 3. Meanwhile `http://<pi-lan-ip>/` works (bootstrap adds the Pi's LAN address to `OT_HOSTS`); re-run bootstrap if that address changed |
| "unknown host" (421) | A name other than those in `OT_HOSTS` (e.g. `ot.home` when you passed `--lan-domain lan`); re-run bootstrap with the right `--lan-domain`, or add it via `OT_HOSTS` in `/etc/ot/ot.local.env` |
| "cross-origin request blocked" (403) | A page on another site tried to change a link. That's working as designed. If it was you, use the ot UI itself |
| `/_/whoami` shows no Tailscale user | WhoIs failed; `journalctl -u ot \| grep whois`; check `/var/run/tailscale/tailscaled.sock` exists |
| Locked out of SSH | Tailscale SSH (`ssh <you>@ot`) bypasses the LAN rule. Otherwise use a keyboard and HDMI, then `sudo systemctl stop ot-firewall` |
| Deploy rolled back | Read the journal lines it printed; `ssh <you>@ot journalctl -u ot -n 50` |

## Security model in one paragraph

Only tailnet devices, plus home-LAN devices when `--lan-access` is on, can
reach ot: nftables, systemd `IPAddressAllow` and the app itself each check
that. Anyone who can reach it may create or edit links; there are no logins.
Each change is recorded in an audit log that the database refuses to rewrite,
with the person's Tailscale identity, or just the IP address for LAN devices
without Tailscale. Any earlier version can be restored with one click.
LAN traffic is plain HTTP, so other devices on the home network could in
principle watch or tamper with it; Tailscale traffic is encrypted. What a hostile web page or
DNS-rebinding attacker could otherwise do is blocked by the Host allowlist
and cross-origin checks on every write. Destinations are restricted to plain
`http(s)` URLs with no embedded credentials, and redirects are never cached.
What this does **not** stop is a household member deliberately changing a
link. That is visible in the history and can be undone. If it ever matters,
"locked links" (edits restricted to named tailnet users) is the planned
upgrade.
