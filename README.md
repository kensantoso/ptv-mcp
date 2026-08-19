# ptv-mcp

MCP server for Melbourne public transport, over Victoria's published GTFS feed.
Departures and journey planning across train, tram and bus, answered locally
from an indexed copy of the timetable.

The point of it is the questions the official apps answer badly:

**A connection that no longer works.** With a realtime key, a journey is flagged
when delays have eaten the change it depends on: the incoming service now
arrives too late to make the outgoing one, counting the walk between platforms.
No amount of staring at either leg tells you this — each one looks fine on its
own, and a delay notice on one line does not say your trip is broken. It is the
difference between finding out now and finding out on the platform.

**A change can beat every direct service.** Melbourne's City Loop reverses
between morning and afternoon, so out of the eastern suburbs the afternoon
"city" train reaches Flinders Street only — Parliament and Melbourne Central
then need a change *even though direct services to the city are running*. Direct
and one-change journeys are searched together and ranked as one list, so the
faster change surfaces instead of hiding behind a direct that is slower.

**Which station should I walk to?** Two stations within walking distance are not
equivalent: an express stop eight hundred metres away, with a direct run, beats
the local station you are standing at once you count the change. Given a
location and a destination, every nearby stop is planned and ranked.

Plus the ordinary things done properly: next departures; the last train home,
with after-midnight times handled so 1:30am belongs to Friday's timetable rather
than Saturday's; platform numbers on both sides of a change; replacement buses
marked as buses instead of trains; whether a service takes the loop or runs
direct; and live delays where the feed has them.

## Tools

| Tool | What it does |
|---|---|
| `find_stop` | Name to stop id, when you want the id itself |
| `next_departures` | Upcoming departures from a stop, with live delays |
| `plan_trip` | Journeys between two stops, direct or one change |
| `last_service` | What is left tonight, including after midnight |
| `service_alerts` | Current disruptions and how late each line is running |
| `stops_near` | What is around a point, grouped by mode |
| `compare_departure_stops` | Which nearby stop reaches a destination soonest, and by which mode |
| `leg_calls` | Every station a leg calls at, and the ones it runs through |
| `database_status` | How old the local timetable is, and whether it has gone stale |
| `rebuild_database` | Refresh it from the published feed, in the background |

See [What you can ask](#what-you-can-ask) for the prompts that reach each of
them.

The timetable needs no API key. A realtime key adds live delays, cancellations
and disruption alerts, and is free.

The indexing and planning are done by
[ptv-gtfs-go](https://github.com/kensantoso/ptv-gtfs-go); this repo is a thin
MCP layer over it.

## Getting a realtime key

Optional, and free. Without one you get the published timetable; with one you
get what is actually happening — how late each service is running, current
disruptions, and a warning when delays have broken a connection you were
relying on. Every tool says plainly which of the two it is answering from.

1. Create an account at
   [opendata.transport.vic.gov.au](https://opendata.transport.vic.gov.au).
2. Confirm the email, sign in, and open **Profile** (top right).
3. The page lists your **Primary key** and a secondary one. Either works.
4. Give it to the server by whichever route suits your host:

```sh
ptv-mcp -set-key YOUR_KEY          # stored owner-only beside the database
PTV_REALTIME_KEY=YOUR_KEY ptv-mcp  # or the environment
```

The Claude Desktop extension prompts for it during install and stores it in the
keychain, so neither of the above is needed there.

The key is for the GTFS-Realtime feeds. It is not the older PTV Timetable API
credential — that is a different product with a developer id and a signing
secret, and this server does not use it.

## Caveats

**The database goes stale, and this is the thing that bites.** The feed is
republished weekly and reshaped at timetable boundaries, so a copy left alone
quietly plans against services that have changed.

The assistant can see this and fix it: `database_status` reports the age,
`rebuild_database` refreshes in the background while the existing copy keeps
answering. Ask it to check if a result looks wrong. From a shell it is
`ptv-mcp -rebuild`.

Once the feed's timetable period has passed entirely, queries fail with an error
rather than returning nothing and letting you read that as "no services".

**Times are absolute** — `08:16`, never "in 3 minutes". A reply is read some time
after it is generated, and a countdown is wrong by then.

**"No live data" is not "on time".** Realtime reaches about an hour ahead, so
anything further out carries no prediction. Without a key, everything reports
this way.

**Change times are cautious, and you can say otherwise.** How long a platform
change takes comes from the operator's `pathways.txt`, walked as a graph at a
pace chosen not to strand anyone — so a change you make in ninety seconds can be
published as three minutes, and a connection you would comfortably catch gets
dropped as impossible. Put a `transfers.json` beside the database
(`ptv-mcp -index-path` prints where) and it is believed ahead of the feed:

```json
{
  "Richmond":        {"*": "2m", "8-3": "60s"},
  "Flinders Street": {"*": "90s"}
}
```

Station names and platform numbers, not stop ids. `"*"` covers every change
there and a specific pair overrides it. Durations are Go syntax: `90s`, `2m`.
A missing file is fine; a wrong one — unknown station, unknown platform,
unparseable duration — refuses to start rather than running with an override you
think is active and is not.

**Walking distances are as the crow flies**, capped at 350 m — fine across a
forecourt, wrong where a rail corridor or river stands between two stops. Changes
*within* a station use the operator's own `pathways.txt` instead.

**Re-sign the binary if you replace one inside an installed bundle.** macOS caches
a signature against a path, so overwriting in place gets the process killed on
launch with no output at all — an empty log and exit 137 is always this.

```sh
codesign --force -s - .../server/ptv-mcp
```

## Disk use

A full build is about **1.5 GB**: Victoria publishes every mode in one feed, and
bus and tram are 92% of it. `-modes` loads only what you ask about — anything
left out is absent from every answer.

```sh
ptv-mcp -modes train -rebuild
```

| `-modes` | Database | Trips |
|---|---:|---:|
| `train` | 97 MB | 49,751 |
| `train,tram` | ~475 MB | 144,663 |
| omitted (everything) | 1.5 GB | 340,340 |

The ~250 MB download is the same either way, and dominates build time. The
database sits in your cache directory (`ptv-mcp -index-path`) and can be deleted
at any time.

## Install

Requires Go 1.25 or later.

```sh
go install github.com/kensantoso/ptv-mcp/cmd/ptv-mcp@latest
ptv-mcp -rebuild                 # downloads ~250 MB and builds the database, once
ptv-mcp -modes train -rebuild    # or just trains: 97 MB instead of 1.5 GB
```

Then point any MCP client at the binary. The server speaks MCP over stdio with
no vendor extensions, so anything that runs a local server works:

```json
{
  "mcpServers": {
    "ptv": {
      "command": "/absolute/path/to/ptv-mcp",
      "env": { "PTV_REALTIME_KEY": "optional, see above" }
    }
  }
}
```

| Client | Where |
|---|---|
| Claude Code | `claude mcp add -s user ptv -- /path/to/ptv-mcp` |
| Claude Desktop | `~/Library/Application Support/Claude/claude_desktop_config.json`, or `%APPDATA%\Claude\` on Windows |
| Cursor, Windsurf, Zed, Amp | same JSON, their own config file |

Restart the client afterwards; most read their config only at startup.

### Packaged bundle

Claude Desktop also takes a `.mcpb`, which installs by double-click and stores
the key in the keychain instead of a config file. Build one with:

```sh
go build -o bundle/server/ptv-mcp ./cmd/ptv-mcp
codesign --force -s - bundle/server/ptv-mcp     # macOS; see Caveats
npx @anthropic-ai/mcpb pack bundle ptv-mcp.mcpb
```

The packed binary is for the architecture you built on, and the manifest
declares `darwin`. On first launch it serves immediately and builds the database
in the background, reporting progress through the tools rather than making the
client wait several minutes on a handshake.

## What you can ask

Tools take station names directly, so most of these are a single call rather
than a lookup followed by the real question. The reply names the stop it matched
and lists near misses, so a wrong guess is visible. Pass ids instead when you
have them and want no ambiguity.

**Departures and last services**

> *When is the next train from Flinders Street?*
> *Trams only from Federation Square.*
> *What's the last train home from the city?*

**Journeys**

> *How do I get from Southern Cross to Box Hill?*
> *Get me to Parliament by 9am — when should I leave?*
> *Fewest changes to Werribee, I have luggage.*

**Least time travelling, bounded** — a different question from soonest arrival.
Say how long you will wait, or it will send you off to wait ninety minutes to
save three.

> *I'm free for the next half hour — quickest run to Ringwood even if I wait.*
> *I'd rather wait 15 minutes for a direct train than go via the loop.*

**Changes and the loop**

> *Get me to Parliament, changing at Richmond.*
> *I'm at Flinders Street and need to double back to Melbourne Central.*
> *Fastest way to Richmond that skips the loop.*

**Which stop to leave from**

> *I'm at Richmond, what's the fastest way into the city?*
> *From here, is the tram or the train quicker to Southern Cross?*
> *What stops are near Federation Square?*

**Where a service actually stops**

> *Does the 17:35 stop at East Richmond?*
> *Which stations does this one skip?*

**Live delays** — needs a realtime key.

> *Are there delays on the Belgrave line right now?*
> *My train is at 5:42 — is it running late?*
> *Will I still make my connection at Richmond?*

## plan_trip options

| Argument | Effect |
|---|---|
| `prefer` | `fastest` (default, soonest arrival), `shortest` (least time travelling), `fewest_transfers`, `leave_latest` |
| `max_wait_minutes` | How long you will wait. Pairs with `shortest`, which is unbounded without it |
| `via` | Only journeys changing at this stop id |
| `max_changes` | `0` for direct only, `1` to allow a change |
| `mode` | `train`, `tram` or `bus` |
| `when` | Local `YYYY-MM-DD HH:MM`, default now |

`from` and `to` take names; `from_stop_id` and `to_stop_id` take ids and win if
both are given. The same applies to `next_departures` (`stop`), and to
`stops_near` and `compare_departure_stops` (`near`, `to`).

## Layout

```
cmd/ptv-mcp      the server, over stdio
internal/tools   the five tools
internal/render  library results -> the shapes the tools return
```

`render` turns library results into the shapes the tools return. A near-copy of
it backs the web front end, deliberately rather than as a shared package:
presentation differs per client — an assistant and a browser want different
shapes — and coupling two separately deployed apps through one wire format buys
less than it costs.

## Data

Timetable data is published by the Department of Transport and Planning.

> Source: Licensed from Public Transport Victoria under a Creative Commons
> Attribution 4.0 International Licence.

Anything you build that surfaces this data carries the same attribution
requirement. This is an unofficial tool, not affiliated with or endorsed by
Public Transport Victoria.
