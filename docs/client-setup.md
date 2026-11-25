# Using nym/ links on your devices

(If your service has another name, read it in place of `nym` throughout.)

**With Tailscale** (app signed in and connected, **"Use Tailscale DNS"** on),
`nym/` links work at home and away, and changes are recorded with your name.

**Without Tailscale**, a device on the home network can still use `nym/`
links when byname runs on a Pi at home with LAN access on (not on a cloud
server). Your router resolves the name, and changes are recorded with the
device's IP address instead of a name. It won't work away from home.

Try `http://nym/_/whoami`: it shows your name (Tailscale) or "home network
(no Tailscale)". Either way, links work.

## Per browser

| Browser | Typing `nym/foo` | Setup |
|---|---|---|
| Chrome, Edge, Brave (desktop) | Should open the link | Optional: Settings → Search engine → Manage → Add: name `nym`, shortcut `nym`, URL `http://nym/_/go?q=%s`. Then `nym` + space + `wiki` works too |
| Firefox (desktop) | Searches the web instead | `about:config` → add boolean `browser.fixup.domainwhitelist.byname` = `true`. byname pages also advertise themselves as a search engine, which Firefox can add from the address-bar search options |
| Safari (macOS, iOS) | Often searches instead | Type `http://nym/foo` once; after that Safari autocompletes `nym/…` from history. Bookmark `http://nym/` on the home screen |
| Chrome (Android) | Varies | Use `http://nym/foo` the first time; add the search shortcut as on desktop |

Useful forms:

- `nym/name` opens the link.
- `nym/name/more/path` appends to the destination, or fills `{1}`, `{2}`, `{*}` in a template.
- `nym/name+` shows where a link goes, who changed it, and its history, without following it.
- `nym/` is the home page: search, create, popular links.

## M0 results: fill in during setup

Record what actually happened on each device in the household.

| Device | OS / browser | Tailscale? | `http://nym/_/whoami` shows | Typing `nym/test` | Notes |
|---|---|---|---|---|---|
| | | | | | |
| | | | | | |
| | | | | | |
| | | | | | |

From a device on home Wi-Fi with **Tailscale off**: `nslookup nym` returns
the Pi's LAN address? ______ `nym/test` opens? ______

From guest Wi-Fi (if any): `curl -m 5 http://<pi-lan-ip>/` → expected: times
out. Result: ______
