# Public access with Google or Slack sign-in

Optional. By default ot is reachable only on your tailnet (and, on a Pi with
`--lan-access`, your home network). Turning on the public address puts it on
the internet as well:

```
https://ot.<tailnet>.ts.net/wiki
```

| Who | How they reach it | Sign-in |
|---|---|---|
| Devices running Tailscale | `ot/wiki` as before, or the https address | none (Tailscale identifies them) |
| Home-network devices (Pi with `--lan-access`) | `ot/wiki` as before | none |
| Anyone else on the internet | the https address | **Google or Slack**, and only accounts on your allow list |

People coming in from the internet don't have the short `ot/` name (that
needs Tailscale), so for them ot's pages show links as
`ot.<tailnet>.ts.net/wiki`. With only one provider configured, there's
nothing to choose, so they go straight to Google or Slack. After signing
out, they stay on the sign-in page.

Changes made by people who signed in are recorded as `alice@gmail.com (via
Google)` in the history.

**How it works.** [Tailscale Funnel](https://tailscale.com/kb/1223/funnel)
publishes the server's tailnet name on the internet. Tailscale's relays
carry the traffic to the server over the tailnet, and the server's own
Tailscale terminates TLS with a certificate for that name. No firewall port is opened: the
server's public IP addresses stay closed, and a Pi behind your home router
needs no port forwarding. Tailscale hands each request to ot over a private
socket and marks it as coming from the internet or from a tailnet device.
ot asks the internet ones to sign in.

`ot` below is the default service name. Use yours if you chose another.

## Costs and requirements

- **Tailscale Funnel**: no extra cost. Needs Tailscale 1.98 or later on the
  host (the deploy keeps Tailscale updated).
- **Google**: free. Works on IPv6-only cloud servers.
- **Slack**: free, but `slack.com` has no IPv6 address (checked
  2026-11-25), so the server needs IPv4 to finish a Slack sign-in. A Pi at
  home has IPv4. On Lightsail set `ipv4 = true`, which costs $5.00 instead
  of $3.50 a month and replaces the server when changed later. On Hetzner, the
  IPv4 address is an extra monthly fee.
- **Debian 13** (systemd 254 or later), as the bootstrap already requires.

## 1. Tailscale

In the admin console:

1. **DNS → HTTPS Certificates → Enable.** This lets the server get a
   certificate for `ot.<tailnet>.ts.net`. Certificates are published in
   public Certificate Transparency logs, so the machine name and the tailnet
   name become public. The name is public anyway once Funnel is on.
2. **Access controls**: merge the `tcp:443` grant from
   [`deploy/tailscale/policy.hujson`](../deploy/tailscale/policy.hujson), and
   its `nodeAttrs` block, uncommented. The `funnel` attribute is what allows
   this host, and only this host, to use Funnel.

Your public address is `https://` + the machine's full tailnet name, shown in
the admin console under Machines, for example
`https://ot.tail1234.ts.net`.

## 2. Sign-in providers

Set up one or both. Each needs a redirect URL with your public address in it.

### Google

In the [Google Cloud console](https://console.cloud.google.com/) (any
project; create one called `ot` if you like):

1. **Google Auth Platform → Branding**: app name `ot`, your email as the
   support and developer contact.
2. **Audience**: *External*. While the app is in *Testing*, only the test
   users you add can sign in (up to 100), which suits a household. *In
   production* needs no Google review here, because ot asks only for
   `openid email profile`. Either way, only addresses on ot's allow list get
   in.
3. **Clients → Create client → Web application**, with the authorized
   redirect URI:
   ```
   https://ot.<tailnet>.ts.net/_/auth/google/callback
   ```
4. Copy the client ID and the client secret.

Who may sign in (`OT_GOOGLE_ALLOW`, comma-separated):

- full addresses: `alice@gmail.com`. Google only vouches for Gmail
  addresses, and for addresses in a Google Workspace domain. So a listed
  address like `dave@company.com` matches only if company.com uses Google
  Workspace and Dave signs in with that Workspace account. A personal Google
  account registered with that address is refused: the address was checked
  once, when the account was made, and may have changed hands since.
- a Google Workspace domain: `example.com`. ot matches this against the
  account's Workspace membership (the `hd` claim), never against the end of
  the address, because a personal Google account can use any address.

### Slack

At [api.slack.com/apps](https://api.slack.com/apps):

1. **Create New App → From scratch**, in your workspace.
2. **OAuth & Permissions → Redirect URLs**: add
   ```
   https://ot.<tailnet>.ts.net/_/auth/slack/callback
   ```
   Under **Scopes → User Token Scopes**, add `openid`, `email` and `profile`.
3. **Basic Information → App Credentials**: copy the client ID and the
   client secret.
4. Your workspace ID is the `T…` part of the address when Slack is open in a
   browser (`app.slack.com/client/T0123ABCD/…`).

Who may sign in (`OT_SLACK_TEAMS`): workspace IDs. Every member of those
workspaces can sign in, including single- and multi-channel guests. To let in people from a workspace other than the
app's own, Slack also requires turning on distribution for the app (**Manage
Distribution**).

## 3. Configure the host

If the host was bootstrapped before, `/etc/ot` exists. Otherwise run
`bootstrap.sh` once without `--public` first.

Settings go in `/etc/ot/ot.local.env`, which bootstrap never overwrites:

```sh
ssh admin@ot 'sudo tee -a /etc/ot/ot.local.env' <<'EOF'
OT_GOOGLE_CLIENT_ID=1234567890-abc.apps.googleusercontent.com
OT_GOOGLE_ALLOW=alice@gmail.com,bob@gmail.com
OT_SLACK_CLIENT_ID=1234567890.1234567890
OT_SLACK_TEAMS=T0123ABCD
EOF
```

Secrets go in `/etc/credstore` (root only), never in the env file. Copy each
secret to your clipboard and pipe it over, so it never sits in a file or your
shell history:

```sh
pbpaste | ssh admin@ot 'sudo sh -c "umask 077; cat > /etc/credstore/ot.google-client-secret"'
pbpaste | ssh admin@ot 'sudo sh -c "umask 077; cat > /etc/credstore/ot.slack-client-secret"'
```

systemd hands them to ot as credentials (`ImportCredential=ot.*`), readable
only by the service.

## 4. Turn it on

Re-run the bootstrap with the flags you used before, plus `--public`:

```sh
ssh admin@ot 'sudo ./deploy/host/bootstrap.sh --admin-user admin --name ot --public'
ssh admin@ot sudo ot-verify
```

The bootstrap checks that sign-in is configured before it changes anything.
It then sets `OT_PUBLIC_URL`, adds the systemd drop-in `20-public.conf`
(the socket directory, the credentials, and outbound HTTPS so ot can reach
Google and Slack), and runs `tailscale funnel`. If Funnel isn't allowed yet,
the bootstrap stops and says which admin-console setting is missing.

`ot-verify` adds a **Public address** section. It checks Funnel, the socket's
permissions, the secrets, that Google and Slack are reachable from the
server, and that ot sends an internet request without a session to sign-in.
Then check from outside: on a phone with **Wi-Fi and Tailscale both off**:

1. open `https://ot.<tailnet>.ts.net/_/whoami`: you get the sign-in page;
2. sign in with an allowed account: you get your account details;
3. sign out, and sign in with an account that isn't allowed: it's refused.

The bootstrap is declarative. Running it again without `--public` turns the
public address off: Funnel off, the drop-in removed, ot tailnet-only again.
Never point Funnel at anything else on this host (for example `tailscale
funnel 80`). `ot-verify` fails if it finds that.

## Day to day

- **Add or remove people**: edit `OT_GOOGLE_ALLOW` or `OT_SLACK_TEAMS` in
  `/etc/ot/ot.local.env`, then `sudo systemctl restart ot`. Someone you
  remove is signed out on their next request. Suspending someone in Google
  Workspace or deactivating them in Slack does **not** sign them out of ot.
  Their session lasts until it expires, so also run `ot sessions revoke`
  (below).
- **See who is signed in**:
  `ssh admin@ot 'sudo -u ot /usr/local/lib/ot/current/ot sessions'`.
- **Sign someone out everywhere**: `… ot sessions revoke alice@gmail.com`,
  or `… ot sessions revoke --all`.
- **Rotate a client secret**: create a new one in the provider's console,
  pipe it into `/etc/credstore` as above, `sudo systemctl restart ot`, then
  delete the old one.
- Sign-ins last 30 days (`OT_SESSION_TTL`, for example `OT_SESSION_TTL=168h`
  in `ot.local.env`).
- The journal records every sign-in, refusal and sign-out with the account and
  address: `journalctl -u ot | grep -E 'signed|sign-in'`.

## What changes for security

[security-review.md](security-review.md#public-address-with-sign-in-2026-11-25)
has the full review. In short:

- The sign-in page and the name `ot.<tailnet>.ts.net` are visible to anyone.
  Everything else needs a session from an allow-listed account. Sign-in
  requests are rate-limited per address.
- Funnel traffic reaches ot over a socket that only root (tailscaled) and
  ot can open, not over the network. On the plain-HTTP port, ot refuses
  anything Funnel delivers, so a Funnel pointed at it by mistake can't skip
  sign-in. `ot-verify` fails if Serve or Funnel publishes anything other
  than ot's socket.
- The firewall and ot's peer-address check still guard the plain-HTTP port.
  systemd's IP filter no longer does: allowing outbound HTTPS also allows
  inbound. To make up for it, ot won't start without the firewall.
- A signed-in person can do what a tailnet member can: create, edit and
  delete links. Every change is recorded with their account.
- Someone who takes over an allowed Google or Slack account, or steals a
  session cookie, gets the same access until you remove the account or
  revoke the session.
- ot needs outbound internet access while the public address is on, for the
  token exchange with Google and Slack.
