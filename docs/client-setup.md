# Using ot/ links on your devices

Every device needs the **Tailscale app, signed in and connected**, with
**"Use Tailscale DNS"** turned on. That is what makes the name `ot` resolve,
at home or away.

Then try `http://ot/_/whoami`. It should show your name. If it does, links work.

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

| Device | OS / browser | `http://ot/_/whoami` shows user? | Typing `ot/test` | Notes |
|---|---|---|---|---|
| | | | | |
| | | | | |
| | | | | |
| | | | | |

Also from one device on home Wi-Fi with **Tailscale off**:
`curl -m 5 http://<pi-lan-ip>/` → expected: times out. Result: ______
