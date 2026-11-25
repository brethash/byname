# Using ot/ links on your devices

(If your service has another name, read it in place of `ot` throughout.)

**With Tailscale** (app signed in and connected, **"Use Tailscale DNS"** on),
`ot/` links work at home and away, and changes are recorded with your name.

**Without Tailscale**, a device on the home network can still use `ot/`
links when ot runs on a Pi at home with LAN access on (not on a cloud
server). Your router resolves the name, and changes are recorded with the
device's IP address instead of a name. It won't work away from home.

Try `http://ot/_/whoami`: it shows your name (Tailscale) or "home network
(no Tailscale)". Either way, links work.

## Per browser

| Browser | Typing `ot/foo` | Setup |
|---|---|---|
| Chrome, Edge, Brave (desktop) | Should open the link | Optional: Settings → Search engine → Manage → Add: name `ot`, shortcut `ot`, URL `http://ot/_/go?q=%s`. Then `ot` + space + `wiki` works too |
| Firefox (desktop) | Searches the web instead | `about:config` → add boolean `browser.fixup.domainwhitelist.ot` = `true`. ot pages also advertise themselves as a search engine, which Firefox can add from the address-bar search options |
| Safari (macOS, iOS) | Often searches instead | Type `http://ot/foo` once; after that Safari autocompletes `ot/…` from history. Bookmark `http://ot/` on the home screen |
| Chrome (Android) | Varies | Use `http://ot/foo` the first time; add the search shortcut as on desktop |

Useful forms:

- `ot/name` opens the link.
- `ot/name/more/path` appends to the destination, or fills `{1}`, `{2}`, `{*}` in a template.
- `ot/name+` shows where a link goes, who changed it, and its history, without following it.
- `ot/` is the home page: search, create, popular links.

## M0 results: fill in during setup

Record what actually happened on each device in the household.

| Device | OS / browser | Tailscale? | `http://ot/_/whoami` shows | Typing `ot/test` | Notes |
|---|---|---|---|---|---|
| | | | | | |
| | | | | | |
| | | | | | |
| | | | | | |

From a device on home Wi-Fi with **Tailscale off**: `nslookup ot` returns
the Pi's LAN address? ______ `ot/test` opens? ______

From guest Wi-Fi (if any): `curl -m 5 http://<pi-lan-ip>/` → expected: times
out. Result: ______
