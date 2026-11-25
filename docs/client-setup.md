# Using me/ links on your devices

(If your service has another name, read it in place of `me` throughout.)

**With Tailscale** (app signed in and connected, **"Use Tailscale DNS"** on),
`me/` links work at home and away, and changes are recorded with your name.

**Without Tailscale**, a device on the home network can still use `me/`
links when byname runs on a Pi at home with LAN access on (not on a cloud
server). Your router resolves the name, and changes are recorded with the
device's IP address instead of a name. It won't work away from home.

Try `http://me/_/whoami`: it shows your name (Tailscale) or "home network
(no Tailscale)". Either way, links work.

## Per browser

| Browser | Typing `me/foo` | Setup |
|---|---|---|
| Chrome, Edge, Brave (desktop) | Should open the link | Optional: Settings → Search engine → Manage → Add: name `me`, shortcut `me`, URL `http://me/_/go?q=%s`. Then `me` + space + `wiki` works too |
| Firefox (desktop) | Searches the web instead | `about:config` → add boolean `browser.fixup.domainwhitelist.byname` = `true`. byname pages also advertise themselves as a search engine, which Firefox can add from the address-bar search options |
| Safari (macOS, iOS) | Often searches instead | Type `http://me/foo` once; after that Safari autocompletes `me/…` from history. Bookmark `http://me/` on the home screen |
| Chrome (Android) | Varies | Use `http://me/foo` the first time; add the search shortcut as on desktop |

Useful forms:

- `me/name` opens the link.
- `me/name/more/path` appends to the destination, or fills `{1}`, `{2}`, `{*}` in a template.
- `me/name+` shows where a link goes, who changed it, and its history, without following it.
- `me/` is the home page: search, create, popular links.

## M0 results: fill in during setup

Record what actually happened on each device in the household.

| Device | OS / browser | Tailscale? | `http://me/_/whoami` shows | Typing `me/test` | Notes |
|---|---|---|---|---|---|
| | | | | | |
| | | | | | |
| | | | | | |
| | | | | | |

From a device on home Wi-Fi with **Tailscale off**: `nslookup me` returns
the Pi's LAN address? ______ `me/test` opens? ______

From guest Wi-Fi (if any): `curl -m 5 http://<pi-lan-ip>/` → expected: times
out. Result: ______
