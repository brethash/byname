# Public access with Google or Slack sign-in

Optional. Without it, byname is reachable only on your tailnet (and, on a Pi with
`--lan-access`, your home network). A public address puts it on the internet,
and everyone reaching it from there signs in with Google or Slack. Only
accounts on your allow list get in.

There are two ways to publish it. **Tailscale is optional** for the first.

| | Your own domain (`--public-domain`) | Tailscale Funnel (`--funnel`) |
|---|---|---|
| Address | `https://hg.example.com/wiki` | `https://me.<tailnet>.ts.net/wiki` |
| Needs Tailscale | no | yes |
| Who signs in | everyone using the address | everyone except tailnet devices |
| Open ports on the server | TCP 80 and 443 (and 22 from your addresses, without Tailscale) | none |
| TLS certificate | Let's Encrypt, obtained and renewed by byname | Tailscale's, for the tailnet name |
| You provide | a domain whose DNS you control; IPv4 on the server | Tailscale HTTPS + Funnel enabled |

Either way:

- **Tailscale is a bonus.** If the server is on your tailnet, devices running
  Tailscale can still type the short `me/wiki`, with no sign-in. On a Pi
  with `--lan-access`, so can home-network devices.
- **People on the internet see the full address.** They don't have the short
  `me/` name (that needs Tailscale), so byname's pages show them links as
  `hg.example.com/wiki`.
- **One provider means no button page.** With only one provider configured,
  people go straight to Google or Slack. After signing out, they stay on the
  sign-in page.
- **The history records the account.** Changes by people who signed in show
  as, for example, `alice@gmail.com (via Google)`.

`me` below is the default service name. Use yours if you chose another.

## Costs and requirements

- **Your own domain**: no extra cost beyond the domain. The server needs a
  public IPv4 address, because most of the internet still reaches websites
  over IPv4. On Lightsail that means `ipv4 = true`: $5.00 instead of $3.50 a
  month, plus a static IP that's free while attached. On Hetzner, the IPv4
  address is billed separately.
- **Tailscale Funnel**: no extra cost. Needs Tailscale 1.98 or later on the
  host (the deploy keeps Tailscale updated).
- **Google sign-in**: free. Works on IPv6-only servers.
- **Slack sign-in**: free, but `slack.com` has no IPv6 address (checked
  2026-11-25), so the server needs IPv4 to finish a Slack sign-in.
- **Debian 13** (systemd 254 or later), as the bootstrap already requires.

## 1. The address

### Your own domain

1. Create the server with `public_domain` set (deploy/cloud, see
   [cloud.md](cloud.md)), or have a Pi you can port-forward 80 and 443 to.
2. At your DNS host, point the name at the server: an `A` record for its
   IPv4 address and an `AAAA` record for its IPv6 address. `tofu output
   dns_records` prints both. Use a short TTL (5 minutes) at first.
3. Wait until `dig +short hg.example.com` returns those addresses. byname asks
   Let's Encrypt for a certificate on the first HTTPS visit. Let's Encrypt
   checks that the name points at this server, through port 80 or 443.

### Tailscale Funnel

In the Tailscale admin console:

1. **DNS → HTTPS Certificates → Enable.** This lets the server get a
   certificate for `me.<tailnet>.ts.net`. Certificates are published in
   public Certificate Transparency logs, so the machine name and the tailnet
   name become public. They're public anyway once Funnel is on.
2. **Access controls**: merge the `tcp:443` grant from
   [`deploy/tailscale/policy.hujson`](../deploy/tailscale/policy.hujson), and
   its `nodeAttrs` block, uncommented. The `funnel` attribute is what allows
   this host, and only this host, to use Funnel.

Your public address is `https://` + the machine's full tailnet name, shown in
the admin console under Machines, for example `https://me.tail1234.ts.net`.

## 2. Sign-in providers

Set up one or both. Each needs a redirect URL containing your public address.
Below, `https://<address>` is `https://hg.example.com` or
`https://me.<tailnet>.ts.net`.

### Slack

At [api.slack.com/apps](https://api.slack.com/apps):

1. **Create New App → From scratch**. Name it after the service (`hg`), in
   your workspace.
2. **OAuth & Permissions → Redirect URLs**: add
   ```
   https://<address>/_/auth/slack/callback
   ```
   and **Save URLs**. Under **Scopes → User Token Scopes**, add `openid`,
   `email` and `profile`.
3. **Basic Information → App Credentials**: copy the **Client ID** and the
   **Client Secret**.
4. Your workspace ID is the `T…` part of the address when Slack is open in a
   browser (`app.slack.com/client/T0123ABCD/…`).

Who may sign in (`BYNAME_SLACK_TEAMS`): workspace IDs. Every member of those
workspaces can sign in, including single- and multi-channel guests. To let in
people from a workspace other than the app's own, Slack also requires turning
on distribution for the app (**Manage Distribution**).

### Google

In the [Google Cloud console](https://console.cloud.google.com/) (any
project; create one called `me` if you like):

1. **Google Auth Platform → Branding**: app name `me`, your email as the
   support and developer contact.
2. **Audience**: *External*. While the app is in *Testing*, only the test
   users you add can sign in (up to 100), which suits a household. *In
   production* needs no Google review here, because byname asks only for
   `openid email profile`. Either way, only addresses on byname's allow list get
   in.
3. **Clients → Create client → Web application**, with the authorized
   redirect URI:
   ```
   https://<address>/_/auth/google/callback
   ```
4. Copy the client ID and the client secret.

Who may sign in (`BYNAME_GOOGLE_ALLOW`, comma-separated):

- full addresses: `alice@gmail.com`. Google only vouches for Gmail
  addresses, and for addresses in a Google Workspace domain. So a listed
  address like `dave@company.com` matches only if company.com uses Google
  Workspace and Dave signs in with that Workspace account. A personal Google
  account registered with that address is refused: the address was checked
  once, when the account was made, and may have changed hands since.
- a Google Workspace domain: `example.com`. byname matches this against the
  account's Workspace membership (the `hd` claim), never against the end of
  the address, because a personal Google account can use any address.

## 3. Configure the host

Run `bootstrap.sh` once without a public flag first, so `/etc/byname` exists.
`SERVER` below is how you reach the host over SSH: its tailnet name, or its
address or domain without Tailscale.

Settings go in `/etc/byname/byname.local.env`, which bootstrap never overwrites:

```sh
ssh admin@SERVER 'sudo tee -a /etc/byname/byname.local.env' <<'EOF'
BYNAME_SLACK_CLIENT_ID=1234567890.1234567890
BYNAME_SLACK_TEAMS=T0123ABCD
BYNAME_GOOGLE_CLIENT_ID=1234567890-abc.apps.googleusercontent.com
BYNAME_GOOGLE_ALLOW=alice@gmail.com,bob@gmail.com
EOF
```

(Leave out the provider you don't use.)

Secrets go in `/etc/credstore` (root only), never in the env file. Copy each
secret to your clipboard and pipe it over, so it never sits in a file or your
shell history:

```sh
pbpaste | ssh admin@SERVER 'sudo sh -c "umask 077; cat > /etc/credstore/byname.slack-client-secret"'
pbpaste | ssh admin@SERVER 'sudo sh -c "umask 077; cat > /etc/credstore/byname.google-client-secret"'
```

systemd hands them to byname as credentials (`ImportCredential=byname.*`), readable
only by the service.

## 4. Turn it on

Re-run the bootstrap with the flags you used before, plus `--public-domain
DOMAIN` or `--funnel`:

```sh
# your own domain, no Tailscale (a cloud server):
ssh admin@SERVER 'sudo ./deploy/host/bootstrap.sh --admin-user admin --name hg \
  --no-tailscale --ssh-from any --public-domain hg.example.com'
# or Tailscale Funnel:
ssh admin@me 'sudo ./deploy/host/bootstrap.sh --admin-user admin --name me --funnel'

ssh admin@SERVER sudo byname-verify
```

The bootstrap checks that sign-in is configured before it changes anything.
It then:

- sets `BYNAME_PUBLIC_URL`;
- adds the systemd drop-in `20-public.conf`: credentials, internet access,
  and a requirement that the firewall be running;
- for a domain, opens TCP 80 and 443 in the firewall;
- for Funnel, runs `tailscale funnel`. If Funnel isn't allowed yet, it stops
  and says which admin-console setting is missing.

`byname-verify` adds a **Public address** section:

- For a domain, it checks that DNS resolves and that the firewall is open. It
  also makes a real HTTPS request, which proves the certificate is valid and
  that byname sends a visitor without a session to sign-in.
- For Funnel, it checks Funnel and the socket.
- Either way, it checks that the secrets are in place and that Google and
  Slack are reachable from the server.

Then check from outside, on a phone with Wi-Fi (and Tailscale) off:

1. open `https://<address>/_/whoami`: Slack or Google asks you to sign in;
2. sign in with an allowed account: you get your account details;
3. sign out, and sign in with an account that isn't allowed: it's refused.

The bootstrap is declarative. Running it again without the public flag
turns the public address off: ports closed, Funnel off, the drop-in removed.
With Funnel, never point it at anything else on this host (for example
`tailscale funnel 80`). `byname-verify` fails if it finds that.

## Day to day

- **Add or remove people**: edit `BYNAME_GOOGLE_ALLOW` or `BYNAME_SLACK_TEAMS` in
  `/etc/byname/byname.local.env`, then `sudo systemctl restart byname`. Someone you
  remove is signed out on their next request. Suspending someone in Google
  Workspace or deactivating them in Slack does **not** sign them out of byname.
  Their session lasts until it expires, so also run `byname sessions revoke`
  (below).
- **See who is signed in**:
  `ssh admin@SERVER 'sudo -u byname /usr/local/lib/byname/current/byname sessions'`.
- **Sign someone out everywhere**: `… byname sessions revoke alice@gmail.com`,
  or `… byname sessions revoke --all`.
- **Rotate a client secret**: create a new one in the provider's console,
  pipe it into `/etc/credstore` as above, `sudo systemctl restart byname`, then
  delete the old one.
- **Sign-in length**: 30 days by default. Set `BYNAME_SESSION_TTL` in
  `byname.local.env` to change it, for example `BYNAME_SESSION_TTL=168h`.
- **Certificates** (your own domain): byname renews them itself, about 30 days
  before they expire. They live in `/var/lib/byname/acme`. Set
  `BYNAME_ACME_EMAIL=you@…` in `byname.local.env` if you want Let's Encrypt's expiry
  warnings.
- **Logs**: the journal records every sign-in, refusal and sign-out with the
  account and address: `journalctl -u byname | grep -E 'signed|sign-in'`.

## What changes for security

[security-review.md](security-review.md) has the full review. In short:

- Anyone can see the sign-in page and the address. Everything else needs a
  session from an allow-listed account. Sign-in requests are rate-limited
  per address.
- **Your own domain**: the server answers the internet directly on TCP 80
  and 443, so it gets scanned like any web server.
  - byname terminates TLS itself (Go's TLS, TLS 1.2+).
  - It only ever requests a certificate for your domain.
  - It trusts no proxy headers, and treats every request there as coming
    from the internet.
  - Port 80 only redirects to https and answers Let's Encrypt. Peers on your
    tailnet or LAN still get the short name there; localhost gets only the
    health check.
  - Connections from the internet are capped: 256 per port, and 32 per
    address. Over the cap they're dropped at once, so tailnet and LAN users
    never wait behind them.
  - Don't put byname behind another proxy or tunnel. In this mode it must
    terminate TLS itself, and it trusts no forwarding headers.
  - On a Pi, don't combine `--lan-access` with `--public-domain` if your
    router rewrites the source address of forwarded connections: internet
    clients would then look like LAN devices.
- **Funnel**: traffic reaches byname over a socket that only root (tailscaled) and
  byname can open, not over the network. On the plain-HTTP port, byname refuses
  anything Funnel delivers, so a Funnel pointed at it by mistake can't skip
  sign-in.
- **The plain-HTTP port**: the firewall and byname's peer-address check still
  guard it. systemd's IP filter no longer does: allowing the internet in
  for the public address also lifts it. To make up for it, byname won't start
  without the firewall.
- **What a signed-in person can do**: whatever a tailnet member can: create,
  edit and delete links. Every change is recorded with their account.
- **Taken-over accounts**: someone who takes over an allowed Google or Slack
  account, or steals a session cookie, gets the same access until you remove
  the account or revoke the session.
