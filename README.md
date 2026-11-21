# muster-meetings

Meetings for [muster](https://github.com/ZacxDev/muster): a **Meetings** tab
in muster's sidebar where you start a call, share a link, and later read what
was said. It is a muster **plugin**, a small Go program you run beside muster
on a subdomain of muster's host. The design is muster's
[docs/design/meetings.md](https://github.com/ZacxDev/muster/blob/main/docs/design/meetings.md).

**What works today (phase 2a):** the tab opens the Meetings page inside
muster; **New meeting** makes a meeting and shows its link with **Copy link**
(and **Share** where the browser has it); the page lists your meetings, newest
first, and an open meeting's row has its own **Copy link**, so the link is
never lost to a reload. Calls, the guest page and the lobby are phase 2b, so a guest opening
the link today gets a `404`. Transcripts, action items and recordings come
later.

For you, the owner, that is the whole feature: open **Meetings**, press
**New meeting**, copy the link, send it.

## How it fits muster

Meetings is a **server-installed app**: an admin installs it once, for the
whole muster, and every person on it gets the **Meetings** tab. Nobody mints
a token of their own, and adding a person is nothing at all: they open the
tab. muster never calls the plugin; the plugin calls muster.

1. muster's `/apps/<plugin id>` page frames
   `https://meet.<muster host>/enter?muster_code=…&muster_plugin=…`, with a
   one-time hand-off code that the person's own browser just minted
   (muster's [App tab](https://github.com/ZacxDev/muster/blob/main/docs/plugins.md#app-tab)).
2. 🔴 `/enter` refuses unless the browser says it is loading a frame
   (`Sec-Fetch-Dest: iframe`), and it refuses **before** it redeems
   anything. With one token for everyone, a link that opened `/enter` with
   someone else's unspent code would otherwise sign whoever followed it in
   as that someone. A top-level load, a fetch, or a request with no
   `Sec-Fetch-Dest` gets a page saying "Open Meetings from muster", and the
   code stays unspent for the frame it was minted for. The app page carries
   `frame-ancestors <muster's origin>`, so a frame on any other site never
   shows what `/enter` answered. A browser that sends no `Sec-Fetch-*`
   headers at all (Safari and iOS before 16.4) is told it is too old for
   Meetings.
3. `/enter` redeems the code with the one token
   (`POST /plugin/v1/handoffs/redeem`), and only when `muster_plugin` is
   that token's own plugin id. It accepts the answer only when its purpose
   is `app`, its target is an app target, its scope is `server`, and it
   names the person by a **subject**: a random 128-bit id muster made for
   this person under this app, different under every other app. With it
   comes the person's display name. muster never tells the plugin a user id
   or an email, and the plugin has nowhere to keep one.
4. It makes an **owner session** for that subject (32 random bytes, stored
   only as SHA-256, 8 hours) and renders the page with it in the page's
   memory. The page calls `/api/*` with `Authorization: Bearer mmo_…`. **No
   cookie is set or read**: in muster's frame a cookie is third-party
   (Safari blocks it, Chrome partitions it). The page removes the code from
   its address with `history.replaceState`, and every response says
   `Referrer-Policy: no-referrer`. Reloading the frame says "Open Meetings
   from muster again"; reloading muster's page mints a new code.
5. Every 60 s the plugin asks muster two things. `GET /plugin/v1/self`: a
   `401` means the app was uninstalled or its token rotated away, and the
   plugin deletes **every** session and ends **every** open meeting. And
   `POST /plugin/v1/owners/check` with the subjects it holds a live session
   or an open meeting for, which muster answers `live`, `expired` or
   `revoked` for each. **Expired** (the person has not opened the tab in 8
   hours, and nothing else) ends their owner sessions only: their open
   meetings keep running for the guests, and they find them still open when
   they reopen the tab. **Revoked** (they hid the app on Account → Apps, were
   disabled, or signed out everywhere) ends their sessions and their open
   meetings. A subject muster does not answer at all, or answers with any
   other state, is treated as revoked. Nobody else is touched, and only what
   existed when the check was sent is ended: a tab the person reopened while
   the answer was on its way keeps its fresh session. Only an answer ends
   anything: a muster that is down or failing ends nothing, a reply with no
   `states` object is not an answer, and when one batch of 200 subjects
   fails, its people are kept while the answered batches apply. The plugin does both before it serves anyone when
   it starts, so what it slept through is applied first.

## Set it up

You need a muster with plugins on and **server-installed apps** (muster core
PR 1h), served over HTTPS on its own host, and a subdomain of that host for
this plugin (one wildcard certificate covers both). Deployment manifests
come in phase 2c; until then run the binary yourself behind your HTTPS
proxy.

On **muster**, as an admin:

1. Add exactly `https://meet.<muster host>` to `MUSTER_PLUGIN_ORIGINS`
   ([configuration.md](https://github.com/ZacxDev/muster/blob/main/docs/configuration.md#muster-server-optional-features)).
   muster then refuses any request whose `Host` is that name, so the plugin
   can never be served as muster.
2. On `/admin/apps`, install the app: a name, a tab label (say `Meetings`)
   and an icon, origin `https://meet.<muster host>` and entry URL
   `https://meet.<muster host>/enter`. Check **Files tasks** if you want
   **Make task** (phase 6). Install asks you to sign in again. Copy the
   `mpl_…` token: it is shown once. From a pipeline,
   `muster-server app install` does the same and prints the token.

Then **configure and start the plugin**:

```sh
cp .env.example .env
# edit .env (below)
set -a; . ./.env; set +a
go run .
```

It logs `talking to … as server app "<name>" (plugin <id>, …)`, and every
person's tab opens their own Meetings page.

**Rotate token** on `/admin/apps` kills the old token at once: the running
plugin's next `/self` gets `401` and it ends every session and open meeting.
Rotate in the same change window as the plugin's redeploy with the new
token. People's subjects survive a rotation, so their meetings stay
reachable. **Uninstall** is final: a later reinstall gives everyone new
subjects, and their earlier meetings are no longer reachable from muster.

> **Before going live** on a muster other people use: muster's design gates
> the `meet.` subdomain on two core releases, the central origin check for
> cookie-authenticated writes and `__Host-` cookie names (design: "Order and
> gates"). Run it only against a muster that carries both.

## Configuration

Read from the environment only; `.env.example` lists them.

| variable | required | meaning |
|---|---|---|
| `MUSTER_URL` | yes | where the plugin calls muster. `https://`, or `http://` to `localhost`, `127.0.0.1` or `[::1]` only; the address alone |
| `MUSTER_PLUGIN_TOKEN` | yes | **exactly one** `mpl_…` token: the one the install on `/admin/apps` showed. A comma (the old per-person list) refuses to start. Never logged |
| `MUSTER_ORIGIN` | yes | muster's origin as browsers see it, e.g. `https://muster.example.test`. The only page allowed to frame the Meetings app |
| `MEETINGS_PUBLIC_URL` | yes | this plugin's own origin, e.g. `https://meet.muster.example.test`. Guest links are built on it. Refused if it equals `MUSTER_ORIGIN` |
| `MEETINGS_LINK_KEY` | yes | 32 random bytes, base64 (`openssl rand -base64 32`), from the plugin's own Secret. Every meeting's link code is derived from it (below), which is what lets you copy an open meeting's link again. The plugin refuses to start when it is missing or not exactly 32 bytes. **Changing it changes every open meeting's link.** Never logged |
| `MEETINGS_LISTEN` | no | the public listener; default `:8080` |
| `MEETINGS_INTERNAL_LISTEN` | no | the internal listener (`GET /healthz`); default `:8081`. Do not expose it. It answers `503` once muster has refused the token |
| `STATE_DIR` | no | where `meetings.db` (SQLite) lives; default `./state`. Keep it across restarts |

The plugin refuses to start when muster answers the token `401`
(uninstalled or rotated away; it ends every session and open meeting
first), when `/self` says the token is not a server-installed app's (a
person's own token from `/plugins` is refused), when it lacks `app`, or when
it cannot reconcile with `owners/check`. It also refuses a phase-2a database
that holds owners or meetings: those are keyed on muster user ids and need a
one-time map to subjects first.

## The HTTP API (phase 2a)

Owner routes take `Authorization: Bearer mmo_…`. A missing, unknown or
expired session is `401`; another person's meeting is the same `404` as one
that does not exist.

| route | does |
|---|---|
| `GET /enter` | only when loaded in muster's frame (`Sec-Fetch-Dest: iframe`): redeem muster's code, make the owner session, render the app |
| `GET /api/meetings?limit=&cursor=` | your meetings, newest first: `{"meetings": [...], "next": <cursor or null>}`; `limit` 1–100, default 50 |
| `POST /api/meetings` | New meeting: `201 {"id", "join_url", "meeting"}`. It takes no options (no body, or `{}`); record and AI are on |
| `GET /api/meetings/{id}` | `{"meeting", "link"}`: `link` is the guest link while the meeting is open and under 24 hours old, `null` otherwise. This is how the page lets you copy an open meeting's link again |
| `GET /healthz` | internal listener only |

**The link** is `https://meet.<muster host>/j/<code>`. The code is
HMAC-SHA256 under `MEETINGS_LINK_KEY` of the meeting's id, cut to 128 bits
(22 base64url characters). It is **never stored**: the plugin keeps only its
SHA-256, which a guest's `/j/<code>` is looked up by, and derives the code
again when you ask for it. So you can copy an open meeting's link again at
any time, from its row in the list or after re-opening Meetings, and only
you can: another person's meeting is a `404`, and once a meeting has ended or
is 24 hours old there is no link to give. The link dies when the meeting
ends, and at the latest 24 hours after it was made.

**Rotating `MEETINGS_LINK_KEY` changes every open meeting's link.** The
links already sent stop matching what the plugin derives, so guests holding
them are turned away once 2b checks them; send the new links. Meetings that
have ended are unaffected (they have no link).

The code never appears in a log line, an error, the meetings list, or the
database.

## What it stores

One SQLite file (`STATE_DIR/meetings.db`, mode `0600`): the people who opened
the tab (the **subject** muster's redeem gave and the display name, **never
a muster user id or an email**), owner sessions (hashed), meetings (state,
the record and AI switches, times, the link code's hash) and an audit trail
of what happened, which never holds a code, a token, a display name,
transcript text or a guest's name. Every owner-facing query goes through one
repository layer that filters by the session's subject.

## Token scope

The app needs `app` (the tab and the hand-off redeem). Installed with
**Files tasks**, it also gets `tasks`, which phase 6's **Make task** uses;
without it that button is not drawn. A server app can never get
`messages`: it cannot post to anyone's chief, file drafts, or write chief
instructions.

For a person, the app can only act (knock, file a task) by naming their
subject in `Muster-Act-For`, and only for 8 hours after they last opened the
tab; past that, their owner session ends but their open meetings keep
running. Each person can **Hide** it on Account → Apps: the tab goes, an
open frame goes within 30 s (muster's own poll), and within 60 s the plugin
ends that person's sessions and open meetings. **Uninstall** on `/admin/apps`
does the same for everyone.

## Telemetry and data leaving your network

None. In phase 2a the plugin talks only to the muster at `MUSTER_URL`. Later
phases send transcript **text** (never audio) to an AI service for action
items, on by default and off per meeting; this README will say so in the
release that adds it.

## Tests

```sh
go test -race ./...
go run ./scripts/mutate   # the security-gate mutation sweep
cd e2e && npm ci && npx playwright install chromium && npx playwright test
```

The browser test (`e2e/`, Chromium) runs the plugin against the fake muster
through `scripts/e2e-harness`, with a muster-like parent on another origin.
It loads `/enter` top level and checks it is refused with no redeem, then
frames the same code from the parent and checks the app opens and makes a
meeting. On NixOS, set `PLAYWRIGHT_BROWSERS_PATH` to
`nix build --print-out-paths nixpkgs#playwright-driver.browsers` (it must
match `@playwright/test` in `e2e/package.json`).

The tests run against `internal/mustertest`, a fake muster built to the
server-installed app design: an admin's install, Rotate and Uninstall;
people who sign in, sign out everywhere, are disabled, or Hide the app;
hand-off codes (single use, 60 s, bound to the app, carrying the minting
session's sign-in time); the redeem's re-checks and its grant (a pairwise
random subject, `act_until` 8 hours on); one predicate for "may this app act
for this person" (`live`, `expired` or `revoked`), answered by `owners/check` and asked by the `Muster-Act-For`
routes (notify, its withdrawal, and tasks through the app's own ledger). It
has its own tests for those rules.

`scripts/mutate` applies one mutation per security gate (accept an unframed
`/enter`, redeem before refusing it, accept a personal token, drop the
subject filter, ignore an `owners/check` absence, ignore `/self`'s `401`,
send a knock without `Muster-Act-For`, set a cookie, a 64-bit link, log the
query string, …), runs the one test named to catch it, and requires that
test to fail **with its expected message**; a mutant that fails to build or
trips another assertion is reported, not counted. Each sweep also runs a
known-caught mutant and a no-op edit that must survive. CI runs gitleaks,
`gofmt`, `go vet`, `go test -race` and the sweep on every push.

**Tested against:** the fake muster only, built to the design of muster's
core PR 1h (docs/design/meetings.md at muster commit `9bf834178`), before
that core change exists. Not yet run against a live muster.

## Layout

| path | what |
|---|---|
| `main.go` | reads the environment, starts the listeners and loops |
| `internal/muster` | the client for muster's plugin routes (`/self`, the redeem, `owners/check`; notify and tasks for 2b) |
| `internal/mustertest` | a fake muster for tests (`httptest`), server-installed apps included |
| `internal/owners` | the one server token, the boot check and reconcile, and the `/self` and `owners/check` poll |
| `internal/store` | SQLite: schema, sessions, the owner-scoped repository, random codes |
| `internal/server` | the HTTP routes, headers and the owner page (`web/`) |
| `scripts/mutate` | the security-gate mutation sweep |
| `scripts/e2e-harness`, `e2e/` | the browser test of the `/enter` frame gate |

## Licence

MIT. See [LICENSE](LICENSE).
