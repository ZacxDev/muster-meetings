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

muster never calls the plugin; the plugin calls muster.

1. muster's `/apps/<plugin id>` page frames
   `https://meet.<muster host>/enter?muster_code=…&muster_plugin=…`, with a
   one-time hand-off code it minted for you (muster's
   [App tab](https://github.com/ZacxDev/muster/blob/main/docs/plugins.md#app-tab)).
2. `/enter` redeems the code with the plugin token that `muster_plugin`
   names (`POST /plugin/v1/handoffs/redeem`). It accepts the answer only when
   its purpose is `app`, its target is an app target, and its owner is that
   token's own owner; anything else is refused.
3. It makes an **owner session** (32 random bytes, stored only as SHA-256,
   8 hours) and renders the page with it in the page's memory. The page calls
   `/api/*` with `Authorization: Bearer mmo_…`. **No cookie is set or read**:
   in muster's frame a cookie is third-party (Safari blocks it, Chrome
   partitions it). The page removes the code from its address with
   `history.replaceState`, and every response says `Referrer-Policy:
   no-referrer`. Reloading the frame says "Open Meetings from muster again";
   reloading muster's page mints a new code.
4. Every 60 s the plugin calls `GET /plugin/v1/self` with each token. When
   muster answers `401` (you revoked the plugin, or your account was
   disabled), it deletes every session minted through that token and ends your
   open meetings. A token revoked while the plugin was down is applied the
   same way when it starts.

## Set it up

You need a muster with plugins on, served over HTTPS on its own host, and a
subdomain of that host for this plugin (one wildcard certificate covers
both). Deployment manifests come in phase 2c; until then run the binary
yourself behind your HTTPS proxy.

On **muster**:

1. Add exactly `https://meet.<muster host>` to `MUSTER_PLUGIN_ORIGINS`
   ([configuration.md](https://github.com/ZacxDev/muster/blob/main/docs/configuration.md#muster-server-optional-features)).
   muster then refuses any request whose `Host` is that name, so the plugin
   can never be served as muster.
2. On `/plugins`, create a plugin with **App tab** checked: origin
   `https://meet.<muster host>`, entry URL `https://meet.<muster host>/enter`,
   a tab label (say `Meetings`) and an icon. Copy the `mpl_…` token: it is
   shown once.

Then **configure and start the plugin**:

```sh
cp .env.example .env
# edit .env (below)
set -a; . ./.env; set +a
go run .
```

It logs `serving owner <id> through plugin <id>`, and the tab opens your
Meetings page.

> **Before going live** on a muster other people use: muster's design gates
> the `meet.` subdomain on two core releases, the central origin check for
> cookie-authenticated writes and `__Host-` cookie names (design: "Order and
> gates"). Run it only against a muster that carries both.

## Configuration

Read from the environment only; `.env.example` lists them.

| variable | required | meaning |
|---|---|---|
| `MUSTER_URL` | yes | where the plugin calls muster. `https://`, or `http://` to `localhost`, `127.0.0.1` or `[::1]` only; the address alone |
| `MUSTER_PLUGIN_TOKEN` | yes | the `mpl_…` token from `/plugins`, with **App tab**. Several, comma-separated, to serve several owners (each owner's own token); one is the common case. Never logged |
| `MUSTER_ORIGIN` | yes | muster's origin as browsers see it, e.g. `https://muster.example.test`. The only page allowed to frame the Meetings app |
| `MEETINGS_PUBLIC_URL` | yes | this plugin's own origin, e.g. `https://meet.muster.example.test`. Guest links are built on it. Refused if it equals `MUSTER_ORIGIN` |
| `MEETINGS_LINK_KEY` | yes | 32 random bytes, base64 (`openssl rand -base64 32`), from the plugin's own Secret. Every meeting's link code is derived from it (below), which is what lets you copy an open meeting's link again. The plugin refuses to start when it is missing or not exactly 32 bytes. **Changing it changes every open meeting's link.** Never logged |
| `MEETINGS_LISTEN` | no | the public listener; default `:8080` |
| `MEETINGS_INTERNAL_LISTEN` | no | the internal listener (`GET /healthz`); default `:8081`. Do not expose it |
| `STATE_DIR` | no | where `meetings.db` (SQLite) lives; default `./state`. Keep it across restarts |

The plugin refuses to start when a token lacks `app`, belongs to a team (an
app tab is a person's), or when muster refuses every token.

## The HTTP API (phase 2a)

Owner routes take `Authorization: Bearer mmo_…`. A missing, unknown or
expired session is `401`; another owner's meeting is the same `404` as one
that does not exist.

| route | does |
|---|---|
| `GET /enter` | redeem muster's code, make the owner session, render the app |
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
you can: another owner's meeting is a `404`, and once a meeting has ended or
is 24 hours old there is no link to give. The link dies when the meeting
ends, and at the latest 24 hours after it was made.

**Rotating `MEETINGS_LINK_KEY` changes every open meeting's link.** The
links already sent stop matching what the plugin derives, so guests holding
them are turned away once 2b checks them; send the new links. Meetings that
have ended are unaffected (they have no link).

The code never appears in a log line, an error, the meetings list, or the
database.

## What it stores

One SQLite file (`STATE_DIR/meetings.db`, mode `0600`): the owners it serves
(muster's id and login, and which plugin token serves them), owner sessions
(hashed), meetings (state, the record and AI switches, times, the link
code's hash) and an audit trail of what happened, which never holds a code,
a token, transcript text or a guest's name. Every owner-facing query goes
through one repository layer that filters by the session's owner.

## Token scope

The token needs `app` (the tab and the hand-off redeem). Phase 6's
**Make task** will also use `tasks`; without it that button is not drawn. It
never needs `messages`: it cannot post to your chief, file drafts, or write
chief instructions. Revoke it on `/plugins` at any time: the tab disappears,
an open page loses its frame within 30 s (muster's own poll), and within
60 s the plugin ends your sessions and open meetings.

## Telemetry and data leaving your network

None. In phase 2a the plugin talks only to the muster at `MUSTER_URL`. Later
phases send transcript **text** (never audio) to an AI service for action
items, on by default and off per meeting; this README will say so in the
release that adds it.

## Tests

```sh
go test ./...
go run ./scripts/mutate   # the security-gate mutation sweep
```

The tests run against `internal/mustertest`, a fake muster that answers
`GET /plugin/v1/self` and `POST /plugin/v1/handoffs/redeem` the way muster's
[plugins.md](https://github.com/ZacxDev/muster/blob/main/docs/plugins.md)
says (single-use 60-second codes, bound to the plugin they were minted for),
for several tokens at once.

`scripts/mutate` applies one mutation per security gate (accept a viewer
code, drop the owner filter, set a cookie, ignore `/self`'s `401`, a 64-bit
link, log the query string, …), runs the one test named to catch it, and
requires that test to fail **with its expected message**; a mutant that fails
to build or trips another assertion is reported, not counted. Each sweep
also runs a known-caught mutant and a no-op edit that must survive. CI runs
gitleaks, `gofmt`, `go vet`, `go test -race` and the sweep on every push.

**Tested against:** the fake muster only, built to muster's documented
hand-off at muster commit `3a5917fe2`. Not yet run against a live muster.

## Layout

| path | what |
|---|---|
| `main.go` | reads the environment, starts the listeners and loops |
| `internal/muster` | the client for muster's plugin routes (`/self`, the redeem) |
| `internal/mustertest` | a fake muster for tests (`httptest`) |
| `internal/owners` | the plugin's tokens, the boot check and the `/self` poll |
| `internal/store` | SQLite: schema, sessions, the owner-scoped repository, random codes |
| `internal/server` | the HTTP routes, headers and the owner page (`web/`) |
| `scripts/mutate` | the security-gate mutation sweep |

## Licence

MIT. See [LICENSE](LICENSE).
