# Use It or Lose It

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that sends each request to the Claude account whose weekly limit resets soonest.

## What it does

If you run several Claude OAuth accounts through CLIProxyAPI, each one has a weekly limit that resets at its own time. Whatever an account has not used by its reset is gone. This plugin routes requests to the account that resets next, so the quota that is about to expire gets used first. Accounts that reset later keep their quota for later. Use it or lose it.

## How it picks

For every request, the plugin orders the candidate accounts by these rules, in this order:

1. Full accounts go last. An account is full when its 5-hour or weekly limit is at 100% and has not reset yet.
2. Accounts with a known weekly reset time come before accounts without one.
3. Sooner weekly reset comes first.
4. Higher CLIProxyAPI priority comes first.
5. Account ID, as a tie-breaker.

The plugin steps aside and lets CLIProxyAPI's built-in routing decide when:

- any candidate is not a Claude account, or
- it has no usage data yet for the top account.

## Features

- Picks the account whose weekly limit resets soonest.
- Moves full accounts to the bottom (can be turned off).
- Checks usage with Anthropic every 30 minutes by default. Requests never trigger a check.
- Also checks shortly after a known reset passes, since the order changes then.
- Updates live between checks from the `anthropic-ratelimit-unified-*` headers on every Claude response.
- Clears CLIProxyAPI's cooldown on an account when a check shows it has room again, for example after an early reset.
- Keeps 15 days of usage history in a small JSON file.
- Status page in the management panel: pick order, pace ("X% would go unused at reset"), weekly charts, and a card for accounts whose login expired.
- Watch-only mode: see what the plugin would pick without changing routing.

## Install

1. Download the zip for your platform from [Releases](https://github.com/akhilasuraj/claude-use-it-or-lose-it/releases):
   `claude-use-it-or-lose-it_<version>_<os>_<arch>.zip`. Check it against `checksums.txt` if you like.
2. Unzip it. It contains one file: `claude-use-it-or-lose-it.dll`, `.so` or `.dylib`.
3. Put that file in your plugins folder (the folder set by `plugins.dir`, `plugins` by default).
4. Enable plugins and this plugin in `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    claude-use-it-or-lose-it:
      enabled: true
      "Check usage every (minutes)": 30
      "Weekly limit to order by": "All models"
      "Move full accounts to the bottom": true
      "Watch only, do not change routing": false
      "Folder for usage history": ""
```

All settings are optional. You can also change them on the Plugins page of the management panel.

## Settings

| Setting | Type | Default | What it does |
|---|---|---|---|
| `Check usage every (minutes)` | integer | `30` | How often usage is checked with Anthropic. Minimum 5. |
| `Weekly limit to order by` | `All models`, `Opus`, `Sonnet` | `All models` | Which weekly limit's reset time decides the order. Keep `All models` unless your plan has separate Opus or Sonnet weekly limits. |
| `Move full accounts to the bottom` | boolean | `true` | Accounts at 100% of their weekly or 5-hour limit are tried last. |
| `Watch only, do not change routing` | boolean | `false` | Requests keep using the normal routing. The status page shows what the plugin would have picked. |
| `Folder for usage history` | string | empty | Where `history.json` is kept. Empty means your user config folder, in a `claude-use-it-or-lose-it` subfolder. In Docker, point it at a mounted volume. |

Older snake_case keys (`refresh_interval_minutes`, `weekly_window`, `deprioritize_exhausted`) still work.

## Status page

Open the management panel and choose **Use It or Lose It** in the plugin menu. The page shows:

- the pick order, with each account's weekly reset, weekly and 5-hour usage, and pick counts,
- the pace for each account: when it runs out, or how much would go unused at reset,
- a chart of this week's usage per account, with the current pace drawn to the reset,
- a card for any account whose login expired,
- a **Refresh now** button.

![Status page, light theme](docs/assets/screenshot-light.png)
![Status page, dark theme](docs/assets/screenshot-dark.png)

## Permissions and privacy

The plugin uses these CLIProxyAPI host calls:

| Call | Why |
|---|---|
| `host.auth.list` | Lists your credentials to find the Claude accounts. |
| `host.auth.get` | Reads a Claude credential. Only the OAuth access token is used, and only to call Anthropic's usage endpoint. |
| Usage records | Reads the provider, account and response headers of each finished request, to pick up the rate-limit headers. |
| `host.routing.reset_cooldown` | Clears the cooldown on an account that has room again. |
| `host.log` | Writes its own log lines to the CLIProxyAPI log. |

Network: the only outside request it makes is `GET https://api.anthropic.com/api/oauth/usage`, with that account's own token. It sends nothing anywhere else.

Disk: it writes one file, `history.json`. It holds each account's e-mail address, usage percentages, reset times and timestamps. Readings older than 15 days are removed.

## Compatibility

- CLIProxyAPI **v8.0.29 or newer**. The plugin is built against the v8.0.29 SDK (plugin schema version 6). Older hosts lack features it uses.
- Claude OAuth accounts. API-key accounts have no weekly limit and are not managed.
- Windows, Linux and macOS, on amd64 and arm64.

## Build from source

You need Go (see `go.mod`) and a C compiler, because the plugin is a cgo shared library.

```sh
go test ./...
CGO_ENABLED=1 go build -buildmode=c-shared -trimpath -ldflags "-s -w" -o build/claude-use-it-or-lose-it.so .
```

Use `.dll` on Windows and `.dylib` on macOS. Copy only the library into your plugins folder; the generated `.h` file is not needed.

## Uninstall

1. Remove the `claude-use-it-or-lose-it` block from `plugins.configs` (or set `enabled: false`).
2. Delete the library from your plugins folder.
3. Optional: delete the history folder, `claude-use-it-or-lose-it` in your user config folder (or the folder you set).

CLIProxyAPI goes back to its built-in routing.

## FAQ

**Does it slow down requests?**
No. Picking only reads cached data. Usage checks run in the background on a timer.

**I mix Claude with other providers. What happens?**
When a request's candidates include a non-Claude account, the plugin leaves the choice to the built-in routing.

**Can I try it without changing anything?**
Yes. Turn on `Watch only, do not change routing`. The status page shows what it would pick.

**An account shows "Login expired". What do I do?**
Log in to that account again on the Auth Files page. Until then the plugin cannot read its usage.

## License

[MIT](LICENSE) © 2026 Akhila Abesinghe
