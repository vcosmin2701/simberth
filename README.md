# simberth

Run a fleet of iOS simulators, each driven by its own Claude agent, and watch what they're testing.

simberth is a fork of [simslim](https://github.com/MobAI-App/simslim) that adds the other half of
"one agent, one simulator": the orchestration and observability layer.

Two things, stacked:

1. **Slim simulators** (inherited from simslim). A freshly booted iOS simulator starts around 180
   background services — Siri, Spotlight indexing, photo analysis, News, iCloud sync. None of it
   matters for development, testing, or CI. Turning them off cuts each simulator's memory roughly
   4x, so a laptop fits a screenful of simulators instead of a handful.
2. **Agents on top** (new here). Spin up N slim simulators, give each one a Claude agent and a
   natural-language scenario, and watch the steps stream live — per simulator — in the macOS app.
   Each run records a deterministic replay file so CI can re-run it without a model in the loop.

## Numbers

One simulator, booted stock and then slimmed, same device and settle time (M1 Pro, 16 GB):

| | Stock | Slim |
|---|---|---|
| Processes | 258 | 70 |
| Memory | 4.0 GB | 0.9 GB |

Memory here is phys_footprint, the figure Activity Monitor shows, which counts compressed and swapped pages. That's what decides how many simulators fit before the machine starts swapping. Run `simberth measure <udid>` to see it for any booted simulator.

`status` and `measure` answer different questions. `status` counts only the
allowlisted launchd labels Simberth manages; it is not a live-process count. A
slim boot still runs required core services plus system apps and extensions.
`measure` walks every process under that simulator's `launchd_sim` and sums
`phys_footprint`. A sum of `ps` RSS values is not comparable because RSS counts
shared mappings once for every process that maps them.

## Install

```sh
go install github.com/vcosmin2701/simberth/cmd/simberth@latest
```

or build from a checkout:

```sh
make cli          # ./simberth
make app          # build/Simberth.app
```

macOS only, and you need Xcode with an iOS Simulator runtime, since simberth
drives simulators through `xcrun simctl`.

Driving simulators with agents additionally needs:

- [AXe](https://github.com/cameroncooke/AXe) for simulator UI control —
  `brew tap cameroncooke/axe && brew install axe`
- Node 18+ for the agent runner, and an Anthropic API key (`ANTHROPIC_API_KEY`)
  or an `ant auth login` profile.

Run `simberth doctor` to check all of this.

## macOS app

The SwiftUI app bundles the CLI and adds:

- Searchable simulator status, disk-size, and live RAM columns.
- Searchable service profiles with per-daemon controls and purpose summaries.
- Read-only disk analysis plus confirmed cleanup of allowlisted data.
- Clone, rename, erase, delete, and Finder shortcuts.

Build it locally with Go and Xcode:

```sh
make app
open build/Simberth.app
```

Memory estimates are guidance rather than additive savings; see the
[measurement method](docs/category-memory.md). Simberth recommends cloning before
service or disk changes so the copy can serve as a backup.

## Usage

```sh
simberth list             # simulators and their slim status (--booted to filter)
simberth profiles         # what a slim boot turns off
simberth profiles <id>    # the launchd labels in one category
simberth on <udid>        # slim a simulator and reboot it slim
simberth off <udid>       # put it back to stock
simberth status <udid>    # managed launchd-label state (not a process count)
simberth verify <udid> --profile ci.json   # exact profile match; non-zero on drift
simberth doctor <udid> --requires push,storekit,universal-links
simberth run --scenario "..." --sims 4   # drive N simulators with agents
simberth replay runs/<id>/replay-<udid>.json   # re-run it deterministically, no model
simberth mirror                          # live screens of every booted simulator
simberth ui describe <udid>   # the actionable elements on screen
simberth ui tap <udid> --label "Sign In"
simberth measure <udid>   # a booted simulator's memory footprint
simberth top              # live fleet monitor; enter a sim for per-daemon RAM/CPU
simberth size <udid>      # total allocated simulator size
simberth disk-plan <udid> # measure reclaimable data; read-only
simberth disk-clean --categories caches,logs --confirm <udid>
simberth clone <udid> <name>
simberth repair-clone <source-udid> <clone-udid>
simberth rename <udid> <name>
simberth boot <udid>      # boot a simulator and wait for its services
simberth shutdown <udid>  # shut down a booted simulator
simberth erase <udid>     # erase apps, data, settings, and slimming overrides
simberth delete <udid>    # permanently delete a simulator
```

Read-only and simulator-management commands accept `--json` for integrations
and the macOS app.

Keep a category you actually need, like Spotlight search:

```sh
simberth on <udid> --except search
```

Or keep one specific daemon, like push notifications:

```sh
simberth on <udid> --keep com.apple.apsd
```

<details>
<summary>All slimming categories</summary>

| ID | Category | Turns off | ~MB |
|---|---|---|---|
| `widgets` | Widgets & Wallpaper | Home and lock screen posters, widgets, and Live Activities. | 675 |
| `siri` | Siri & Intelligence | Siri, Apple Intelligence, speech, and on-device ML model services. | 265 |
| `search` | Spotlight & Search | On-device Spotlight and in-Settings search services. | 50 |
| `icloud` | iCloud & Apple Account | iCloud sync, Apple Account, keychain, and backup services. | 100 |
| `store` | App Store, Push & Media | App Store, push notification, StoreKit, and media services. | 80 |
| `pim` | Mail, Calendar & Contacts | Mail, Calendar, Contacts, Reminders, and related sync services. | 80 |
| `web` | Safari Sync & Web Services | Safari sync, web push, privacy, and universal-link services. | 50 |
| `family` | Family & Screen Time | Family Sharing, Screen Time, and usage tracking. | 65 |
| `health` | Health, Home & Fitness | HealthKit, HomeKit, and Fitness services. | 135 |
| `photos` | Photos & Media Analysis | Photos library, photo analysis, and media analysis services. | 60 |
| `apps` | News, Weather, Maps & Games | News, Weather, Maps, Tips, and game services. | 90 |
| `messaging` | Messaging & FaceTime | iMessage, FaceTime, call, and identity services. | 60 |
| `connectivity` | Sharing & Device Connectivity | AirDrop, Continuity, CarPlay, Watch, and Find My services. | 65 |
| `telemetry` | Ads, Diagnostics & Telemetry | DeviceCheck, ad privacy, analytics, diagnostics, and feedback services. | 105 |
| `other` | Other Background Services | Wallet, business services, assets, and miscellaneous background daemons. | 195 |

Memory figures are iOS 26.5 clean-boot medians and are not additive; see the
[measurement method](docs/category-memory.md). Run `simberth profiles <id>` to
see the daemons in a category.

</details>

### Slow CI runners

`simberth on` boots the simulator, disables ~170 daemons one `launchctl` call at a
time, then reboots — all under a single 10-minute deadline. Shared CI runners (like
GitHub-hosted macOS runners) are slower and less predictable, and can blow that
deadline mid-reconfigure with `context deadline exceeded` errors. Raise it with the
global `--boot-timeout` flag or the `SIMBERTH_BOOT_TIMEOUT` environment variable:

```sh
simberth on <udid> --boot-timeout 15m
# or, for the whole job:
export SIMBERTH_BOOT_TIMEOUT=15m
```

Each individual `launchctl` transition is also bounded by its own 2-minute
timeout, and the first transitions after a cold boot can exceed that on slow
hosts (failed ones are retried automatically). Raise it with the global
`--spawn-timeout` flag or the `SIMBERTH_SPAWN_TIMEOUT` environment variable:

```sh
simberth on <udid> --spawn-timeout 5m
# or, for the whole job:
export SIMBERTH_SPAWN_TIMEOUT=5m
```

### Profile files

For a repeatable setup, commit a JSON profile alongside your project and apply it
per run. A `ci.json` and a `dev.json` can slim differently for each purpose:

```json
{
  "name": "ci",
  "description": "UI test runs",
  "except": ["search", "store"],
  "keep": ["com.apple.apsd"]
}
```

```sh
simberth on <udid> --profile ci.json
```

`except` and `keep` mirror the flags of the same name; `name` and `description`
are for whoever reads the file. Unknown fields, unknown category IDs, and labels
that no category disables are rejected, so a typo fails loudly. `--profile` is the
single source of truth for its run and cannot be combined with `--except` or
`--keep`.

To build one interactively, run `simberth profile ci.json`: name it, then use the
arrow keys and space to tick whole features to keep enabled, or press `→` to open
a feature and keep individual daemons within it. Point it at a directory to save
`<name>.json` there, or omit the path to print to stdout.

### Checking a simulator with doctor

Slimming a simulator turns features off on purpose, so a test suite that needs
one of them wants a fast way to catch a mis-slimmed simulator before it runs.
`doctor` checks a booted simulator against the features you name and exits
non-zero if any of them are broken, which makes it a natural CI preflight:

```sh
simberth doctor <udid> --requires push,storekit,universal-links
```

```
<udid>: 2/3 required features OK
  ok     push
  ok     universal-links
  BROKEN storekit — com.apple.storekitd disabled
```

Feature IDs are finer-grained than the slimming categories: each maps to just
the daemons that back one capability. Run `simberth doctor --list` to see them
all. Both the check and the list support `--json`.

### Verifying a profile still holds

The disable overrides are per-simulator state that is easy to lose without
noticing: a deleted-and-recreated device or a simulator from a new runtime
comes up stock, and nothing else fails loudly when that happens — the simulator
just quietly runs heavy again. `verify` compares a booted
simulator's overrides against a profile — the same `--profile`/`--except`/`--keep`
you passed to `on` — and exits non-zero listing the drift, so a script or CI
step can catch it and re-run `simberth on` (idempotent: it only applies the
missing delta) to repair:

```sh
simberth verify <udid> --profile ci.json || simberth on <udid> --profile ci.json
```

Where `doctor` answers "do the features my tests need still work?", `verify`
answers "is this simulator in exactly the slim state I configured?". Supports
`--json`.

## Running agents

```sh
simberth run \
  --scenario "Sign in with test@example.com, then confirm the home screen loads" \
  --app ~/Build/MyApp.app \
  --sims 4
```

Each simulator is slimmed, booted, given the app, and handed to its own Claude
agent. Agents run concurrently — that is what the slimming is for. Every tool
call becomes a step with a screenshot, streamed as NDJSON under `--json` and
persisted to `~/Library/Application Support/simberth/runs/<id>/`.

The agent reads the screen through the accessibility tree rather than pixels,
and simberth distills that tree before the model sees it: a stock iOS home
screen is ~113k tokens of raw JSON but ~1.5k once reduced to the elements you
can actually act on. That 75x difference is what makes a turn affordable.

### Watching the fleet

The Runs tab shows a live screen per simulator, so you watch the phones rather
than read a step list. Frames come from `axe stream-video` as MJPEG, defaulting
to 5 fps at 40% scale — small enough that a dozen streams cost little, large
enough to stay readable when tiled.

The same stream is available from the command line:

```sh
simberth mirror                          # every booted simulator, frames on stdout
simberth mirror --out /tmp/fleet         # newest frame per simulator, as files
simberth mirror --fps 10 --scale 1.0 <udid>
```

An agent must end with a verdict it can justify from what it saw:

```
VERDICT: PASS Home screen loaded with the account name in the header
VERDICT: FAIL Expected a 'Continue' button; scrolled the whole list and found none
```

Each run also records `replay-<udid>.json` — just the acting steps, addressed by
accessibility label. Replaying needs no model and no API key:

```sh
simberth replay runs/20260907-084955/replay-<udid>.json
```

A recorded run replays in seconds where the agent took a minute, which is what
makes it usable in CI. Explore once, replay forever; re-record when the UI
changes enough to break it.

## Disk cleanup

Disk cleanup is permanent and separate from service slimming. `disk-plan` is
read-only. `disk-clean` shuts down the exact simulator, clears only allowlisted
per-device directories, and refuses to run without `--confirm`.

```sh
simberth disk-categories
simberth disk-plan <udid>
simberth disk-clean --categories caches,logs,temporary --confirm <udid>
# Optional: also remove on-demand language models
simberth disk-clean --categories linguistic-data --confirm <udid>
```

Built-in apps and core OS language resources are part of a signed iOS runtime
shared by every simulator using that version, so simberth never modifies them.
Required Siri assets are measured only because iOS restores them on launch;
on-demand language data is opt-in and may download again when needed.

`disk-plan` also reports a read-only storage breakdown for installed app bundles,
Documents, app data, and user media. Those durable rows are never eligible for
cleanup. See the [disk cleanup safety model](docs/disk-cleanup.md) for recovery
behavior, safeguards, and Xcode 26.6 validation results.

## Use as a Go library

The repo root is an importable package with no external dependencies, so you can
drive simulators from your own tooling instead of shelling out to the CLI:

```sh
go get github.com/vcosmin2701/simberth
```

```go
package main

import (
	"context"
	"fmt"

	"github.com/vcosmin2701/simberth"
)

func main() {
	ctx := context.Background()

	devices, err := simberth.ListDevices(ctx)
	if err != nil {
		panic(err)
	}

	// Slim every booted simulator, keeping Siri and search enabled.
	profile, err := simberth.BuildProfile("", "siri,search", "")
	if err != nil {
		panic(err)
	}
	for _, d := range devices {
		if d.State != "Booted" {
			continue
		}
		changed, err := simberth.EnableSlim(ctx, d.Set, d.UDID, profile, func(msg string) {
			fmt.Println(d.UDID, msg)
		})
		fmt.Println(d.UDID, "changed:", changed, "err:", err)
	}
}
```

The package never writes to stdout — it returns values and reports progress
through the `simberth.Reporter` callback you pass in. `simberth.Categories`,
`simberth.Features`, and `simberth.SlimmableSet()` expose the same allowlist the
CLI uses. macOS only, since everything runs through `xcrun simctl`.

## How it works

`simberth on` writes persistent `launchctl disable` entries for the chosen launchd labels into the simulator's own launchd database, then reboots it. The entries stick across reboots, so the simulator comes up slim in a single boot from then on. `simberth off` clears them and reboots back to stock. Your Mac is never touched, only the simulator you point it at, and only services that are safe to disable. Core workflow services such as `sharingd`, plus the handful that wedge a simulator when turned off, are left running.

The earliest runtime with verified persistence is iOS 18.5. iOS 17.x and 18.3
accept each `launchctl disable`, but the simulator comes back stock after a
reboot. `simberth on` rejects runtimes older than 18.5 before booting or changing
launchd state. Supported runtimes are still read back after the reboot, and the
command fails instead of claiming success if any requested override was lost.

This is per-simulator state, not a global setting. The daemon disables live in
that one simulator's launchd database and nowhere else. `simberth clone` copies
the source's exact Simberth-managed profile along with its installed apps, app
data, and settings. It also rebases simulator-local symlinks, regenerates app
registration databases, and clears copied system caches and temporary files.
Before finishing, it boots the clone and audits its running processes for open
paths into the source. The clone finishes shutdown, and the source returns to
the boot state it had before cloning.

Clones created by an older Simberth build can be repaired in place without
deleting their installed apps or app data:

```sh
simberth repair-clone <source-udid> <clone-udid>
```

The repair preserves the clone's current Simberth profile and boot state. The
source is kept shutdown while copied paths are rebuilt; if direct links or open
files into that source remain, the clone stays shutdown instead of being
restarted. CoreSimulator does not expose clone lineage, so pass the exact source
UDID: Simberth verifies references to the source you specify.

`erase`, `delete` and recreate, and "Erase All Content and Settings" still
produce stock service state, so the simulator's memory climbs back until you
rerun `simberth on`. A simulator created from a new or updated runtime also
starts stock. A tar or filesystem snapshot also preserves the profile. Run
`simberth list` to see current state, or `simberth verify` to check one against a
specific profile.

## What you lose

Turning services off is fine for most development, UI automation, and CI, but some features genuinely stop working. The ones worth knowing:

- Spotlight and in-Settings search return nothing (`search`).
- Push notifications need `apsd`, StoreKit testing needs `storekitd` (`store`).
- Universal links need `swcd` (`web`).
- The Contacts, Photos, and Calendar pickers can act up without their categories.

`simberth profiles` lists every category, so you can keep a category with `--except` or individual daemons with `--keep`.

## Why

Testing is shifting. Once agents are writing apps, you want agents running them too, and the place an iOS app runs is a simulator. One agent, one simulator. So how much work you get through at once comes down to how many simulators a machine can hold, and stock simulators are heavy enough that a laptop fills up fast. Slimming them is the cheapest way to raise that ceiling: more simulators on the box means more agents working in parallel on it.

## Credits

simberth is a fork of [simslim](https://github.com/MobAI-App/simslim) by Interlap, built for
[MobAI](https://mobai.run). All of the simulator-slimming work — the daemon allowlist, the
category model, the launchd override persistence, the disk cleanup, and the SwiftUI app it
extends — is theirs. This fork adds the agent orchestration, the run/replay model, and the Runs
interface on top of it.

Simulator UI control uses [AXe](https://github.com/cameroncooke/AXe) (MIT), which builds on
[idb](https://github.com/facebook/idb) (MIT, Meta).

## License

MIT. Copyright Interlap for the inherited simslim code; see [LICENSE](LICENSE).
