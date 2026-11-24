# muster-meetings

Meetings for [muster](https://github.com/ZacxDev/muster): a **Meetings** tab
in muster's sidebar where you start a call, share a link, and later read what
was said. It is a muster **plugin**, a small Go program you run beside muster
on a subdomain of muster's host. The design is muster's
[docs/design/meetings.md](https://github.com/ZacxDev/muster/blob/main/docs/design/meetings.md).

**What works today (phases 2a, 2b and 3a):** the tab opens the Meetings page
inside muster; **New meeting** puts you in a call (LiveKit) with the link
and **Copy link** at the top. A guest opens the link, reads what the meeting
does with what they say, types a name and asks to join; their row appears at
the top of your call ("Grace (guest) wants to join · Admit · Deny", **Admit
all** when several wait), and your phone gets a push when you are not looking
at the meeting. Camera, microphone and **Share screen** for everyone in the
call, **Remove** for a guest, **End meeting** for you. The page also lists
your meetings, newest first, with **Open** and **Copy link** on an open one.
**The live transcript (3a):** each microphone in the call is transcribed,
and the lines appear under each speaker's name in a panel that you and every
admitted guest see alike; after the meeting, **Transcript** on its row opens
the archive, with **Download text** and **Download WebVTT**. **The
recording (phase 5, optional: only with a bucket configured,
[Recording (optional)](#recording-optional)):** with **Record** on, the
call is recorded from the first admitted guest to the end (LiveKit's room
composite, MP4, to your S3-compatible store); everyone in the call sees the
badge, and "Recording failed" while the recorder is down. The archive
plays the video beside the transcript, and a click on a line plays the
video from that moment. **Share recording** makes a link anyone can open
to watch it, with no muster sign-in, until the recording is deleted (30
days after the meeting by default, or 365). Action items come later: the
**AI action items** switch and its notice are already here, because the
notice says what will happen.

For you, the owner, that is the whole feature: open **Meetings**, press
**New meeting**, copy the link, send it, admit whoever knocks.

## How it fits muster

Meetings is a **server-installed app**: an admin installs it once, for the
whole muster, and every person on it gets the **Meetings** tab. Nobody mints
a token of their own, and adding a person is nothing at all: they open the
tab. muster never calls the plugin; the plugin calls muster.

1. muster's `/apps/<plugin id>` page frames
   `https://meet-<x>.<domain>/enter?muster_code=…&muster_plugin=…`, with a
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
   fails, its people are kept while the answered batches apply. muster
   allows 60 `owners/check` calls a minute: past it the plugin waits out
   muster's `Retry-After`, and each poll resumes after the last person the
   previous one judged, so everyone is judged within a bounded number of
   polls. The plugin does both before it serves anyone when it starts
   (waiting out the limit until everyone is judged), so what it slept
   through is applied first.

## Set it up

You need a muster with plugins on and **server-installed apps** (muster core
PR 1h), served over HTTPS, three hosts of your own for Meetings (the plugin,
LiveKit signalling, and TURN over TLS, the only media path: design, "Media:
LiveKit, TURN, Egress"), and a **LiveKit** server. [Deploying](#deploying)
has the Kubernetes base, the Compose fragment and the image; to run the
binary yourself, put it behind your HTTPS proxy and point LiveKit's webhook
at the plugin's **internal** listener, `http://<plugin>:8081/livekit/webhook`,
signed with the same API key.

On **muster**, as an admin:

1. Add exactly the plugin's origin, `https://meet-<x>.<domain>`, to `MUSTER_PLUGIN_ORIGINS`
   ([configuration.md](https://github.com/ZacxDev/muster/blob/main/docs/configuration.md#muster-server-optional-features)).
   muster then refuses any request whose `Host` is that name, so the plugin
   can never be served as muster.
2. On `/admin/apps`, install the app: a name, a tab label (say `Meetings`)
   and an icon, origin `https://meet-<x>.<domain>` and entry URL
   `https://meet-<x>.<domain>/enter`. Check **Files tasks** if you want
   **Make task** (phase 6), and then also do step 3. Install asks you to sign in again. Copy the
   `mpl_…` token: it is shown once. From a pipeline,
   `muster-server app install` does the same and prints the token.
3. For **Make task**: add the plugin's task namespace, `meetings`, to
   `MUSTER_PLUGIN_TASK_NAMESPACES` on muster
   ([configuration.md](https://github.com/ZacxDev/muster/blob/main/docs/configuration.md)),
   e.g. `MUSTER_PLUGIN_TASK_NAMESPACES=meetings` (comma-separated beside
   any others). Each task the plugin files is keyed `meetings:<id>`. With
   the variable unset muster files no plugin tasks (`503`), and a namespace
   it does not list is refused (`400`), so **Make task** cannot work until
   it is set.

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
> the Meetings host on two core releases, the central origin check for
> cookie-authenticated writes and `__Host-` cookie names (design: "Order and
> gates"). Run it only against a muster that carries both.

## Configuration

Read from the environment only; `.env.example` lists them.

| variable | required | meaning |
|---|---|---|
| `MUSTER_URL` | yes | where the plugin calls muster. `https://`, or `http://` only to `localhost`, `127.0.0.1`, `[::1]`, a **fully-qualified** Service name written with its trailing dot, `<svc>.<ns>.svc.<cluster domain>.` (e.g. `http://muster.tenant.svc.cluster.local.:8105`), or exactly the host `MEETINGS_MUSTER_HTTP_HOST` names. The short (`muster.tenant.svc`) and dotless forms are refused for http: with the pod default `ndots:5` they go through the node's search domains first, and a wildcard record there would receive the token in clear text. The address alone |
| `MEETINGS_CLUSTER_DOMAIN` | no | the cluster's DNS domain for that rule; default `cluster.local` |
| `MEETINGS_MUSTER_HTTP_HOST` | no | ONE host to which `MUSTER_URL` may be plain http, matched exactly (no wildcard, no suffix match), e.g. `muster` in Compose, where muster's service is on the same network. Empty allows none |
| `MUSTER_PLUGIN_TOKEN` | yes | **exactly one** `mpl_…` token: the one the install on `/admin/apps` showed. A comma (the old per-person list) refuses to start. Never logged |
| `MUSTER_ORIGIN` | yes | muster's origin as browsers see it, e.g. `https://muster.example.test`. The only page allowed to frame the Meetings app |
| `MEETINGS_PUBLIC_URL` | yes | this plugin's own origin, e.g. `https://meet-muster.example.test`. Guest links are built on it. Refused if it equals `MUSTER_ORIGIN` |
| `MEETINGS_LINK_KEY` | yes | 32 random bytes, base64 (`openssl rand -base64 32`), from the plugin's own Secret. Every meeting's link code is derived from it (below), which is what lets you copy an open meeting's link again. The plugin refuses to start when it is missing or not exactly 32 bytes. **Changing it changes every open meeting's link.** Never logged |
| `MEETINGS_LISTEN` | no | the public listener; default `:8080` |
| `MEETINGS_INTERNAL_LISTEN` | no | the internal listener (`GET /healthz`, and LiveKit's webhook `POST /livekit/webhook`); default `:8081`. Do not expose it. `/healthz` answers `503` once muster has refused the token |
| `MEETINGS_LIVEKIT_URL` | yes | where browsers connect for the call, e.g. `wss://meet-rtc-muster.example.test` (`ws://` to a loopback host only). The pages' CSP allows exactly this origin and its `https` twin |
| `MEETINGS_LIVEKIT_API_URL` | yes | LiveKit's server API as the plugin reaches it, e.g. `http://livekit:7880` (inside the cluster; plain http allowed) |
| `LIVEKIT_API_KEY`, `LIVEKIT_API_SECRET` | yes | LiveKit's API key and secret. The secret signs every participant token and the plugin's own API calls, and verifies the webhook; it never reaches a browser and is never logged |
| `MEETINGS_LIVEKIT_ICE` | no | `relay` (default): the browser uses only TURN relay candidates, since TURN over TLS on 443 is the deployment's only media path. `all` for a local LiveKit with no TURN |
| `MEETINGS_TRUSTED_PROXIES` | no | comma-separated CIDRs of the ingress in front of the plugin. A knock or stream from one of them is counted against the `X-Forwarded-For` client it names (the right-most hop that is not itself trusted); without it every one counts against the ingress's own address, and a request carrying the header is warned about once (log and `/healthz`). `0.0.0.0/0` or `::/0` refuses to start |
| `MEETINGS_CLIENT_IP_HEADER` | no | behind a CDN, the one header that carries the client's address, e.g. `CF-Connecting-IP`. Read **only** when the request's peer is inside `MEETINGS_TRUSTED_PROXIES`, and then in place of `X-Forwarded-For`; from any other peer it is ignored, so a client cannot name itself. A header that is missing, repeated, a list or not an address counts the request against the proxy (it can only refuse knocks, never admit more). Set without `MEETINGS_TRUSTED_PROXIES`, or set to `X-Forwarded-For`, it refuses to start. Unset: the `X-Forwarded-For` behaviour above |
| `STATE_DIR` | no | where `meetings.db` (SQLite) lives; default `./state`. Keep it across restarts |
| `MEETINGS_STT_ENGINE` | no | the transcript's engine: unset or `off` (no transcript: no egress is started, the ingest refuses every stream, and the notice and badge do not say "transcribed" or name the AI service), `utterance` (each pause-cut utterance, at most 15 s, to the STT service below), `nemotron` (live captions from muster's `stt-nemotron` server, falling back per participant to `utterance`; [the transcript](#the-transcript)), or `fake` (tests only; refuses to start unless `MEETINGS_TEST_MODE=1`) |
| `MEETINGS_STT_URL` | with `utterance` or `nemotron` (its fallback) | the STT service muster's dictation uses, `http(s)://host[:port]`, e.g. `http://stt-cohere:8000`: the plugin calls its `POST /v1/audio/transcriptions` exactly as muster does |
| `MEETINGS_STT_TOKEN` | with `utterance` or `nemotron` | that service's bearer: a **copy** of stt-cohere's one token (`STT_API_TOKEN`, muster's `MUSTER_STT_TOKEN`), in the plugin's own Secret; rotating it means updating both. Never logged |
| `MEETINGS_STREAM_STT_URL` | with `nemotron` | the Nemotron server's stream endpoint, `wss://host[:port]/v1/stream` (`ws://` inside the cluster, e.g. `ws://stt-nemotron:8000/v1/stream`; the path may be left off). No user, query or fragment |
| `MEETINGS_STREAM_STT_TOKEN` | with `nemotron` | the Nemotron server's bearer: a **copy** of `stt-nemotron`'s `STT_API_TOKEN`. Visible ASCII only. Never logged. 🔴 `nemotron` needs **all four** of `MEETINGS_STREAM_STT_URL`, `MEETINGS_STREAM_STT_TOKEN`, `MEETINGS_STT_URL` and `MEETINGS_STT_TOKEN`, or the plugin refuses to boot |
| `MEETINGS_STT_MODEL` | no | the `model` form field, sent only when set (stt-cohere ignores it; speaches needs it) |
| `MEETINGS_INGEST_SECRET` | with an engine | 32 or more random bytes, base64 (`openssl rand -base64 32`): the key of every ingest URL's token (below). Never logged |
| `MEETINGS_INGEST_URL` | with an engine | the ingest listener as **Egress** reaches it, e.g. `ws://muster-meetings:8082`; each track egress streams to `<this>/stt/ingest/<track>/<token>` |
| `MEETINGS_INGEST_LISTEN` | no | the ingest listener; default `:8082`. Egress's alone: never route it |
| `MEETINGS_CONTACTS_URL` | no (with `_TOKEN`) | the **contacts** plugin's internal API, `http(s)://host[:port]`, e.g. `http://muster-contacts:8081` ([Contacts (optional)](#contacts-optional)). Unset: no guest card, exactly as before contacts |
| `MEETINGS_CONTACTS_TOKEN` | with `MEETINGS_CONTACTS_URL` | its bearer: a **copy** of the contacts plugin's `CONTACTS_API_TOKEN`, 32+ characters, never `MUSTER_PLUGIN_TOKEN`. Never logged. 🔴 Both or neither: one alone refuses to start |
| `MEETINGS_S3_ENDPOINT` | no (with the three below) | the recordings store's API, `http(s)://host[:port]` with no path, e.g. `http://minio.example.test:9000` ([Recording (optional)](#recording-optional)). Unset: no recording, exactly as before |
| `MEETINGS_S3_BUCKET` | with `MEETINGS_S3_ENDPOINT` | the bucket, e.g. `meetings` |
| `MEETINGS_S3_ACCESS_KEY`, `MEETINGS_S3_SECRET_KEY` | with `MEETINGS_S3_ENDPOINT` | a key scoped to that bucket alone; Egress uploads with the same key. Never logged. 🔴 All four or none: some alone refuse to start |
| `MEETINGS_S3_REGION` | no | the signing region; default `us-east-1` (MinIO's). Set alone it means nothing |
| `MEETINGS_FFMPEG` | no | the ffmpeg that samples frames from a recording; default `ffmpeg` on `PATH` (the image carries a minimal one). Missing: the timeline still lists frame times, and the frame route answers `503` |

The plugin refuses to start when muster answers the token `401`
(uninstalled or rotated away; it ends every session and open meeting
first), when `/self` says the token is not a server-installed app's (a
person's own token from `/plugins` is refused), when it lacks `app`, or when
it cannot reconcile with `owners/check`. It also refuses a phase-2a database
that holds owners or meetings: those are keyed on muster user ids and need a
one-time map to subjects first.

## Deploying

Everything runs in the muster tenant: muster's Kubernetes namespace, or
muster's Compose project.

### The three hosts

Each is **one label** under your domain, because an edge provider's free
certificate covers one level (`meet-rtc-muster.example.test`, never
`rtc.meet.example.test`). They are configuration, never built in:

| host (example) | serves | how it is reached |
|---|---|---|
| `meet-muster.example.test` | the plugin's `:8080`: the owner app, the guest page | your HTTPS ingress or proxy (a CDN may sit in front) |
| `meet-rtc-muster.example.test` | LiveKit signalling (`wss`), `:7880` | your HTTPS ingress or proxy (WebSockets on) |
| `meet-turn-muster.example.test` | LiveKit's TURN over TLS, `:5349` | an **SNI passthrough** on public TCP 443; LiveKit terminates TLS with its own certificate |

No UDP port is public, and LiveKit's direct media listeners (`7881/tcp`, the
UDP ICE range) are on no Service and no published port: every client, LAN
clients included, connects through TURN over TLS.

### The image

`ghcr.io/zacxdev/muster-meetings`: `:<version>` for a tag `v<version>`,
`:main` and `:sha-<short sha>` for `main` (`.github/workflows/image.yml`,
`linux/amd64` and `linux/arm64`). A static binary on distroless, uid 65532,
with the pages and LiveKit's browser client inside it. The root filesystem
may be read-only: it writes only `/state` (SQLite; a volume) and `/tmp`
(SQLite's temporary files; a tmpfs or `emptyDir`). Build it yourself with
`docker build -t muster-meetings .`.

`:main` is published only after `ci` passed on that commit, and it moves:
**pin a digest** (or a released version) in a deployment. A package's first
publish to GHCR is **private**: make it public once in the package's
settings (or give the cluster a pull secret).

### The Secrets

| Secret (Kubernetes name) | keys | where the value comes from |
|---|---|---|
| `muster-meetings` | `MUSTER_PLUGIN_TOKEN` | the `mpl_…` token `muster-server app install` (or `/admin/apps`) prints once |
| | `MEETINGS_LINK_KEY` | `openssl rand -base64 32` |
| | `MEETINGS_INGEST_SECRET` | `openssl rand -base64 32` (the ingest URL tokens' key) |
| | `MEETINGS_STT_TOKEN` | a copy of stt-cohere's bearer (`STT_API_TOKEN`) |
| | `MEETINGS_STREAM_STT_TOKEN` | the `nemotron` engine only (optional otherwise): a copy of stt-nemotron's bearer (`STT_API_TOKEN`) |
| `livekit-keys` | `LIVEKIT_API_KEY`, `LIVEKIT_API_SECRET` | your choice (`livekit-server generate-keys`); the plugin and LiveKit read the same Secret |
| `meetings-turn-tls` | `tls.crt`, `tls.key` (type `kubernetes.io/tls`) | a **publicly trusted** certificate for the TURN host; browsers refuse TURN/TLS with a self-signed one. Nothing serves port 80 for that name, so issue it with a DNS-01 challenge, and renew it (LiveKit reads it at start: restart LiveKit after a renewal) |

```sh
kubectl -n <muster namespace> create secret generic muster-meetings \
  --from-literal=MUSTER_PLUGIN_TOKEN=mpl_… --from-literal=MEETINGS_LINK_KEY="$(openssl rand -base64 32)" \
  --from-literal=MEETINGS_INGEST_SECRET="$(openssl rand -base64 32)" --from-literal=MEETINGS_STT_TOKEN=…
kubectl -n <muster namespace> create secret generic livekit-keys \
  --from-literal=LIVEKIT_API_KEY=API… --from-literal=LIVEKIT_API_SECRET=…
kubectl -n <muster namespace> create secret tls meetings-turn-tls --cert=tls.crt --key=tls.key
```

### Kubernetes

`deploy/kubernetes` is a kustomize **base** with no real name in it; a
tenant overlay sets every value. `deploy/example-overlay` sets each one with
`example.test` values; copy it. What the base holds:

- `muster-meetings`: a Deployment (1 replica, `Recreate`: one SQLite
  writer; nonroot, read-only root filesystem; startup, readiness and
  liveness probes on `:8081/healthz`, the startup one allowing the 15
  minutes the boot check may take), a 1 Gi PVC on `/state`, and a Service
  with `http` `:8080` (route the plugin's host here) and `internal` `:8081`
  (never route it).
- `livekit`: a Deployment of `livekit-server` 1.13.11 **pinned by digest**,
  on the pod network (not `hostNetwork`), nonroot, read-only root
  filesystem. Its config is the `LIVEKIT_CONFIG` env entry: `turn.enabled`,
  `turn.domain` from the params, `turn.tls_port: 5349` with the certificate
  from `meetings-turn-tls`, no TURN/UDP; `rtc.node_ip` the pod's own
  address, also the one entry in `turn.allow_restricted_peer_cidrs`;
  `rtc.port_range` 50000–50199 and `turn.relay_range` 40000–40199, both
  outside the NodePort range; `rtc.interfaces` pinned to `eth0`;
  `room.auto_create: false` (only the plugin creates rooms), `empty_timeout`
  and `departure_timeout` 600; the webhook to
  `http://muster-meetings:8081/livekit/webhook`, keys from `livekit-keys`.
- `egress`: LiveKit's Egress v1.15.0 **pinned by digest**, as its image's
  own user (uid 1001), with `cpu_cost.track_cpu_cost: 0.1` and
  `room_composite_cpu_cost: 2` (spike S3: LiveKit's defaults refused most
  track starts at a meeting's start). It is the one container that writes
  its root filesystem: its entrypoint recreates PulseAudio's state at every
  start. And `redis-meetings`: Redis 7.2 pinned by digest, persisting
  nothing, Egress's channel to LiveKit (LiveKit's config names it too).
  Neither is muster's.
- Services: `livekit` (ClusterIP `7880`: the API for the plugin, signalling
  for your ingress), `livekit-nodeport` (NodePort `signal` 7880 and
  `turn-tls` 5349, for a route from outside the cluster), `redis-meetings`
  (ClusterIP `6379`), and the plugin's `ingest` `:8082` on `muster-meetings`
  (never route it).
- NetworkPolicies: every pod denies by default. The plugin admits `:8080`
  from the ingress controller's namespace, `:8081` from LiveKit alone and
  `:8082` (the ingest) from Egress alone, and reaches muster's pods,
  LiveKit's `:7880`, stt-cohere's `:8000`, DNS and stt-nemotron's `:8000`
  (the `nemotron` engine's; the last egress rule). LiveKit admits `:7880`
  and `:5349` from anywhere (both come through the NodePort, and both
  authenticate every client) and Egress's media (`7881/tcp`, the ICE range)
  from Egress alone, and reaches the plugin's `:8081`, its Redis, Egress's
  ICE (UDP) and DNS. Egress reaches LiveKit, the plugin's ingest, its Redis
  and DNS, nothing else (phase 5 adds the object store). Redis admits
  LiveKit and Egress and reaches nothing. muster's own NetworkPolicies, if
  any, must admit the plugin's pods (to muster, and to stt-cohere's `:8000`).

What an overlay sets:

| knob | how |
|---|---|
| hostnames, muster's address and origin, trusted proxies, the client-IP header, the transcript | `configMapGenerator` `muster-meetings-params`, `behavior: merge`: `muster-url` (`http://<muster svc>.<ns>.svc.cluster.local.:<port>`, the trailing dot included), `cluster-domain`, `muster-origin`, `public-url`, `livekit-url`, `turn-domain`, `trusted-proxies`, `client-ip-header`, `stt-engine` (`utterance`, `nemotron`, or `off`), `stt-url` (stt-cohere's Service), `stream-stt-url` (stt-nemotron's, `ws://<svc>:8000/v1/stream`; read only by `nemotron`) |
| namespace | `namespace:` |
| the plugin's image | `images:` (pin a digest) |
| NodePorts | a patch on `livekit-nodeport` (`/spec/ports/0/nodePort` signalling, `/spec/ports/1/nodePort` TURN) |
| the PVC's StorageClass | a patch on `muster-meetings-state` |
| a Secret's name | a patch on the Deployment's `secretKeyRef` or volume |
| the ingress controller's namespace, muster's pod labels and port, stt-cohere's and stt-nemotron's pod labels and port | a patch on the `muster-meetings` NetworkPolicy (`/spec/egress/0` muster, `/spec/egress/2` stt-cohere, `/spec/egress/4` stt-nemotron) |
| `rtc.node_ip`, if not the pod's address | a patch on `livekit`'s `LIVEKIT_NODE_IP` env entry |

```sh
kubectl kustomize deploy/example-overlay   # what it renders
```

The base has no Ingress objects: add the plugin's and signalling's routes in
your ingress, with the plugin's host to `muster-meetings:8080` and the
signalling host to `livekit:7880`.

### Docker Compose

`compose.meetings.yml` joins muster's own Compose file (muster's file is
unchanged):

```sh
docker compose -f docker-compose.yml -f compose.meetings.yml --profile meetings up -d
```

It adds `muster-meetings` and `livekit` under the `meetings` profile, with
the same LiveKit config as Kubernetes (a test holds the two equal). Its
variables (in muster's `.env`) are listed at the top of the file. The plugin
joins muster's default network and calls `http://muster:8105`, allowed
because the file sets `MEETINGS_MUSTER_HTTP_HOST=muster`: the token stays on
the Compose network and never passes your CDN or proxy. It
publishes, on `127.0.0.1` only, the plugin (`8106`), signalling (`8107`) and
TURN/TLS (`8108`) for your reverse proxy: HTTPS for the first two, an SNI
passthrough on 443 for the third. It also adds `egress` and
`redis-meetings` (the same images and Egress config as Kubernetes; neither
publishes anything). The ingest is on an **internal** network of its own,
`meetings-ingest`, that only the plugin and Egress join, and the plugin
listens for it on its own address there (`MEETINGS_INGEST_ADDR`, default
`172.31.251.2`). The transcript calls muster's `stt-cohere` (its compose
profile) by name, with `MEETINGS_STT_TOKEN` the same value as muster's
`MUSTER_STT_TOKEN`; set `MEETINGS_STT_ENGINE=off` without one.

### Upgrading from 2c

3a adds the transcript, and **the shipped defaults turn it on** (`stt-engine=utterance` in the base's params, `MEETINGS_STT_ENGINE=utterance` in Compose). Before you apply 3a's base:

1. Add two keys to the `muster-meetings` Secret: `MEETINGS_INGEST_SECRET` (`openssl rand -base64 32`) and `MEETINGS_STT_TOKEN` (a copy of stt-cohere's bearer). Without them the plugin's pod does not start (`CreateContainerConfigError`).
2. Expect two new Deployments from `egress.yaml`: `egress` (LiveKit's Egress) and `redis-meetings` (its Redis, with a Service). LiveKit's config now names that Redis, so LiveKit restarts with it.
3. Point the plugin at your stt-cohere. Set `stt-url` in the params to its Service (e.g. `http://muster-stt-cohere:8000`), and patch `/spec/egress/2/to/0/podSelector/matchLabels` of the `muster-meetings` NetworkPolicy to stt-cohere's pod labels (the example overlay does both). stt-cohere's own NetworkPolicy, if it has one, must admit the plugin's pods on `:8000`.
4. Compose: add `MEETINGS_INGEST_SECRET` and `MEETINGS_STT_TOKEN` (= `MUSTER_STT_TOKEN`) to muster's `.env`, and run muster's `stt-cohere` profile.

**No STT service?** Set `stt-engine=off` in the params (Compose: `MEETINGS_STT_ENGINE=off`). Nothing is transcribed, and nothing else in this list is needed for the plugin to start. The notice and the badge then no longer say "transcribed" or name the AI service. `egress` and `redis-meetings` still run; you may leave them out of your overlay.

### Upgrading from 3a

3c adds the `nemotron` engine. **The default engine is unchanged** (`stt-engine=utterance`), so applying 3c's base to a 3a deployment changes nothing it runs: the new `stream-stt-url` param has a default and is read as an **optional** key (an overlay whose params ConfigMap lacks it still starts the pod), `MEETINGS_STREAM_STT_TOKEN` is an **optional** key of the Secret (a Secret without it still starts the pod), and the new NetworkPolicy rule (stt-nemotron's `:8000`) is appended **last**, so an overlay's patches by index (`/spec/egress/2` for stt-cohere, `/spec/egress/3` for DNS) keep their targets. To turn live captions on:

1. Run muster's `stt-nemotron` image (its README) beside stt-cohere, with its own `STT_API_TOKEN`.
2. Add `MEETINGS_STREAM_STT_TOKEN` (a copy of that token) to the `muster-meetings` Secret.
3. Set `stt-engine=nemotron` and `stream-stt-url` (its Service, `ws://<svc>:8000/v1/stream`) in the params; keep `stt-url` and `MEETINGS_STT_TOKEN`: stt-cohere is the per-participant fallback, and without it the plugin refuses to boot.
4. Patch `/spec/egress/4/to/0/podSelector/matchLabels` of the `muster-meetings` NetworkPolicy to stt-nemotron's pod labels; its own NetworkPolicy, if any, must admit the plugin's pods on `:8000`.
5. Size its `STT_MAX_STREAMS` to at least 8 × the meetings you want captioned at once; a participant over it is on the fallback ("Captions delayed"), never queued.

Compose: set `MEETINGS_STT_ENGINE=nemotron`, `MEETINGS_STREAM_STT_URL` and `MEETINGS_STREAM_STT_TOKEN` (this stack runs no stt-nemotron of its own).

**LiveKit now depends on `redis-meetings`**: one replica, persisting nothing. What a Redis restart does to a running LiveKit and Egress (whether LiveKit refuses to start without it, and whether rooms and egresses survive the restart) is **UNVERIFIED**: it was not measured. Treat a Redis restart as one that may drop the calls and transcripts in progress. The plugin's own meetings, links and transcripts are in its SQLite and are not affected.

### Contacts (optional)

With the **muster-contacts** plugin deployed beside it (muster's
`docs/design/contacts.md`, phase 1), Meetings offers each admitted guest an
optional card when the meeting ends for them, by **End** or by their own
**Leave**: their name (prefilled from the knock) and email, the sentence
"Your name and email go to *owner*, who will see that you were in this
meeting. Nothing else is shared.", **Send** and **Skip**. Once per guest per
meeting; never the owner; never a guest who only waited, was denied or was
removed; only while contacts says it accepts that owner (asked in the
background at most once a minute; a failed ask keeps the last answer for up
to 24 h, and with no answer there is no card). The card's id lives in the
guest page's memory and is spent once (30 minutes at most). "Once per guest"
means once per knock: a guest who leaves and knocks again is a new
participant, and is offered a new card when that visit ends.

🔴 **No URL, no change.** With `MEETINGS_CONTACTS_URL` and
`MEETINGS_CONTACTS_TOKEN` both unset, no card route exists (`/card` is a
`404` like any unknown path), no card is minted, the guest's `ended` event
and the Leave answer (`204`) are byte for byte what they were before, no
call is made to anything new, and any outbox rows left by an earlier
configured boot are **deleted** at start (their names and emails have
nowhere to go). `TestNoContactsURLChangesNothing` compares all of this with
goldens taken at the commit before contacts.

**Sending is never on the call's path.** Send writes the submission to the
plugin's own `contacts_outbox` table in the transaction that spends the
card, and answers at once with one message ("Sent to *owner*."), whatever
contacts later does with it. A worker delivers it to `POST /v1/submissions`
(the contacts plugin's own wire types and field checks:
`github.com/ZacxDev/muster-contacts/contactsapi`, pinned in `go.mod`) with
an `Idempotency-Key` of `card:<card id>` (the card's public id: its SHA-256,
never the secret the guest's page holds), RFC 3339 UTC times, a 3 s
timeout, and a backoff of 5 s doubling to 10 minutes: `2xx` delete the row
(contacts answers a card for an owner who is not accepting with the same
`202`, and drops it), `400`/`409`/`413` (and any other `4xx` but `401` and
`429`) delete it and log the event, and `401`, `429` (its `Retry-After`), `5xx`
and no answer retry. A row 72 h old is deleted undelivered and counted in
the log (`meetings_contacts_outbox_dropped_total`). Knock, admit, the token
and End never wait on contacts: with it black-holed they take as long as
without it. Log lines name events, statuses and row ids, never a name, an
email, a card id or the token.

**Limits:** 10 card posts per source address per 10 minutes (`429`; the
cards a meeting can mint are bounded by the guests the owner admitted, and
contacts caps each owner's cards a day), 2 KiB bodies, and the contacts
plugin's own field rules (`contactsapi.CleanName`, `contactsapi.CheckEmail`):
a name as at the knock (1–40 characters), one plain email address (no
display name, no list).

To turn it on (Kubernetes):

1. Deploy muster-contacts and install it linked to Meetings (its README).
2. Add `MEETINGS_CONTACTS_TOKEN` (a copy of its `CONTACTS_API_TOKEN`) to the
   `muster-meetings` Secret, and set `contacts-url` in the params to its
   internal listener (e.g. `http://muster-contacts:8081`).
3. Let the plugin reach it: append one rule to the `muster-meetings`
   NetworkPolicy's egress, **last**, so the patches by index keep their
   targets (the base adds nothing; `deploy/example-overlay` shows it):

   ```yaml
   - op: add
     path: /spec/egress/-
     value:
       to: [{podSelector: {matchLabels: {app.kubernetes.io/name: muster-contacts}}}]
       ports: [{protocol: TCP, port: 8081}]
   ```

   The contacts plugin's own NetworkPolicy must admit the plugin's pods on
   that port.
4. Redeploy. The startup line ends `; contacts: http://… (the guest card is
   offered)`.

Compose: set `MEETINGS_CONTACTS_URL` and `MEETINGS_CONTACTS_TOKEN` in
muster's `.env`, with the contacts service on a network the plugin joins.

**Rollback.** Unset both variables (or empty `contacts-url` and remove the
Secret key) and redeploy: no card from then on, and the outbox is emptied
at start. 🔴 **Do not roll back by running an older image**: this release
adds migration **5** (three nullable `participants` columns for the card,
and the `contacts_outbox` table), and migrations here are one-way. An older
build refuses a database at schema 5 ("newer than this build"); the added
columns and table are unused when contacts is unset. To go back to an older
image anyway, restore the volume from a backup taken before this release.

### Recording (optional)

With a bucket configured, a meeting whose **Record** switch is on (it is on
for a new meeting; the owner can turn it off at any time, and on again only
before a guest has knocked) is recorded from the moment its first guest is
admitted until it ends: LiveKit's **room composite** egress (its grid
layout, which shows a shared screen large) writes one H.264/AAC MP4 per
**part** to `meetings/<owner subject>/<meeting id>/recording-<n>.mp4`.
About **1.4 GB per recorded hour** (720p30 at ~3 Mb/s, spike S3), and Egress
needs about 1.7 CPUs per recording at once (the sizing rule in
`egress.yaml`). Without a bucket nothing on this page exists: no egress, no
route, no sweeper (`internal/server/norecording_test.go` pins every answer to
the pre-recording commit's).

- **The badge.** Everyone in the call sees "Recording · …" while Record is
  on, and **"Recording failed · …"** while the recording should run and does
  not: its start was refused (no Egress capacity), or its egress died and the
  next has not begun. The plugin retries on the track egress's backoff (1, 2,
  4 … 30 s); each new egress is a new part.
- **Gaps are recorded, as the transcript's are.** The stretch from the
  admit to the first file's first frame (`starting`, a few seconds of
  Chrome starting up) and from a part's end to the next part's first frame
  (`failed`) are stored as recording gaps; the share page names them.
- **Time alignment.** A part's start is stored in **meeting time** (the
  transcript's time base: ms from when the owner first joined) as **the
  egress itself reported it**: Egress's `FileInfo.started_at`, set when its
  pipeline reached PLAYING (the file's first frame), never when its webhook
  arrived. A segment at meeting time `T` is in the part `p` with
  `p.start ≤ T < p.start + p.duration`, at offset `T − p.start`; nothing per
  segment is stored (`internal/timeline`). Screen shares are recorded from
  LiveKit's `track_published` / `track_unpublished` webhooks, so their times
  are the webhooks' arrival (LiveKit sends no other time for them).
- **Playback.** The archive asks `POST /api/meetings/{id}/recording-url` for
  a signed `/media/<meeting>/<part>?exp=&sig=` per part (HMAC over the path
  and the expiry, under a key derived from `MEETINGS_LINK_KEY`; 15 minutes,
  asked again when it lapses) and plays it in a `<video>`; the plugin
  streams the object from the store with `Range`, so the store is never
  exposed to a browser.
- **The share link** (`POST /api/meetings/{id}/share`, after the meeting
  ended): `https://meet-<x>.<domain>/r/<token>`, 256 random bits stored only
  as SHA-256, a page with the video and no script, valid until the
  recording's purge time. A new link turns the old one off;
  `DELETE /api/meetings/{id}/share` turns it off. An unknown, revoked or
  expired link, or a part that is not there, answers one and the same `404`,
  byte for byte. The token never reaches a log (`/r/{token}`). **Why stream,
  not a presigned URL:** the store is reached over the mesh and is not public,
  a SigV4 presigned URL lives at most 7 days (the link lives 30 or 365) and
  cannot be revoked before it expires, and it would name the store's host.
- **Retention.** Each owner chooses 30 (default) or 365 days on the Meetings
  page; a meeting is given its `purge_recording_at` when it ends (by any
  path), so a later change applies from the next meeting. An hourly sweeper
  deletes each part's **object, then its row**; with the store down it
  deletes neither and tries again next hour, and an object already gone is
  not an error. It logs counts, never a key or a title. 🔴 **The backstop is
  a lifecycle rule on the bucket**, so a dead sweeper cannot keep recordings
  forever: expire `meetings/` after **366 days** (the longest choice plus a
  day). There is no "forever" choice.
- **For the analysis** (`GET /api/meetings/{id}/timeline`, owner only): the
  meeting, its parts and gaps, its screen shares, every segment with its
  `recording: {part, offset_ms}` (or `null`) and `screen_share`, and a frame
  ref per text segment said while a screen was shared and recorded (at the
  middle of the overlap). `GET /api/meetings/{id}/frame?at=<meeting ms>`
  answers that moment as a JPEG (at most 1280 wide), with `X-Meeting-Ms`,
  `X-Recording-Part` and `X-Recording-Offset-Ms`. It runs ffmpeg against the
  store through a loopback range proxy (`internal/frames`), never copying the
  file to disk. No model is called by this release.

**Setting it up** (the operator; nothing here is applied for you):

1. **The bucket**, e.g. `meetings`, private, with the lifecycle rule. With
   MinIO's client:

   ```sh
   mc mb <alias>/meetings
   mc ilm rule add --expire-days 366 --prefix meetings/ <alias>/meetings
   ```

2. **One key scoped to that bucket alone**, for the plugin and Egress. The
   policy (MinIO `mc admin policy create <alias> meetings-recordings policy.json`,
   then a user or service account attached to it):

   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       {"Effect": "Allow", "Action": ["s3:GetBucketLocation", "s3:ListBucket", "s3:ListBucketMultipartUploads"],
        "Resource": ["arn:aws:s3:::meetings"]},
       {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"],
        "Resource": ["arn:aws:s3:::meetings/meetings/*"]}
     ]
   }
   ```

   (Egress needs `PutObject` and the multipart actions; the plugin
   `GetObject`, `DeleteObject`; split them into two keys if you prefer, with
   the same bucket.)
3. **The Secret:** add `MEETINGS_S3_ACCESS_KEY` and `MEETINGS_S3_SECRET_KEY`
   to `muster-meetings` (both the plugin and Egress read them there).
4. **The params:** `s3-endpoint` (the store's API as the pods reach it) and
   `s3-bucket`; `s3-region` if it is not `us-east-1`. Egress's
   `storage.s3` block (`egress.yaml`) is filled from the same params and
   keys, so the two cannot name different buckets.
5. **The network:** append one egress rule to BOTH the `muster-meetings` and
   the `egress` NetworkPolicies, to the store's address and port (the
   example overlay does it with an `ipBlock`). The base reaches no store.
6. Compose: set the five `MEETINGS_S3_*` variables in muster's `.env`; the
   plugin and Egress both read them.

**Upgrading, and rolling back.** This release adds migration **6**: the
owner's recording retention (default 30 on every existing owner), each
meeting's `purge_recording_at` (meetings already ended get 30 days from
their end), and the `recordings`, `recording_gaps`, `recording_shares` and
`screen_shares` tables. Nothing an older build reads changes, and without a
bucket they stay empty. To turn the recording off, unset the variables (or
empty `s3-endpoint`/`s3-bucket` and remove the keys): no new recording from
then on (the existing objects stay until the lifecycle rule expires them).
🔴 **Do not roll back by running an older image**: migrations are one-way,
and an older build refuses a database at schema 6 ("newer than this build").
To go back anyway, restore the volume from a backup taken before this
release.

**The image** carries a minimal static ffmpeg (7.1.5, the release tarball
pinned by SHA-256: protocols file/pipe/tcp/http, the mov demuxer, the H.264
and VP9 decoders, scale, the MJPEG encoder): **+6.3 MB on amd64, +4.7 MB on
arm64**, measured on this release's build. Its build stage is cross-compiled,
so the arm64 image needs no emulation.

**Not in this release:** `MEETINGS_MAX_RECORDINGS` (the design's cap on
recordings at once: Egress's own capacity check refuses a composite it
cannot take, and the plugin retries); **Delete now**; **Apply to existing**
(a retention change applies from the next meeting); recording while the
owner is alone in the call (it starts at the first admit, as designed).

### Capacity and limits

- **About 100 concurrent participants per LiveKit pod.** Each relayed
  participant takes ports from both pinned ranges, `rtc.port_range`
  50000–50199 and `turn.relay_range` 40000–40199 (200 each, about two per
  participant). To widen, patch both ranges in the `LIVEKIT_CONFIG` env
  entry (the Kubernetes and Compose copies together): keep each clear of the
  NodePort range 30000–32767, of each other, and of 5349, 7880 and 7881.
- **IPv4 pod addresses only.** The config puts `$(LIVEKIT_NODE_IP)/32` in
  the TURN allow-list and binds TURN on `0.0.0.0`; with an IPv6-only pod
  address LiveKit refuses to start ("no matching node IP for relay").

### muster's side

- `MUSTER_PLUGIN_ORIGINS` gets exactly the plugin's origin,
  `https://meet-<x>.<domain>`.
- `MUSTER_PLUGIN_TASK_NAMESPACES` gets `meetings` (for **Make task**).
- Web push on, for knock notifications.
- `muster-server app install` (or `/admin/apps`): origin
  `https://meet-<x>.<domain>`, entry URL `https://meet-<x>.<domain>/enter`;
  its token goes into the `muster-meetings` Secret.

### Behind a CDN

The plugin counts knocks per client address. Behind a CDN that names the
client in its own header, set `MEETINGS_TRUSTED_PROXIES` to the CIDRs your
ingress connects to the plugin **from**, and `MEETINGS_CLIENT_IP_HEADER` to
the CDN's header (for Cloudflare, `CF-Connecting-IP`). The header is believed
only from those addresses; make sure the ingress is reachable only through
the CDN, or a client that bypasses it can set the header itself.

### The TURN route (operator steps, in order)

Each step touches shared or outward-facing infrastructure; do them in this
order (design: "Phase-2 deploy item: the `meet-turn` route").

1. **Certificate:** a publicly trusted certificate for the TURN host (DNS-01),
   in the `meetings-turn-tls` Secret, with its renewal set up. LiveKit reads
   it once, at start: **restart LiveKit after every renewal** (e.g. a
   `rollout restart` from the renewal hook), or it serves the old one until
   it expires.
2. **LiveKit:** apply the overlay; check its log says `Starting TURN server`
   with `turn.portTLS: 5349`. Nothing public changes yet.
3. **The 443 route:** on the public address that will serve TCP 443 for the
   TURN host, add **one SNI passthrough** route: SNI = the TURN host →
   `livekit-nodeport`'s `turn-tls` NodePort on a cluster node. TLS is **not**
   terminated on the way; every other SNI on that 443 is unchanged.
4. **DNS:** an A record for the TURN host to the address serving that 443,
   **not proxied** by a CDN (a CDN carries neither TURN nor TLS passthrough).
   The plugin's and signalling's hosts may be proxied.
5. **Firewall:** confirm nothing else was opened: no UDP, no relay port range,
   no `7881`/`7882`. Record any new listener in your firewall's ledger.
6. **Verify:** a phone on a mobile network (Wi-Fi off) and a laptop with UDP
   blocked join one meeting; each browser's `getStats()` shows a selected
   candidate pair of type `relay` over `tls`.

## The HTTP API

Owner routes take `Authorization: Bearer mmo_…`. A missing, unknown or
expired session is `401`; another person's meeting is the same `404` as one
that does not exist.

| route | does |
|---|---|
| `GET /enter` | only when loaded in muster's frame (`Sec-Fetch-Dest: iframe`): redeem muster's code, make the owner session, render the app |
| `GET /api/meetings?limit=&cursor=` | your meetings, newest first: `{"meetings": [...], "next": <cursor or null>}`; `limit` 1–100, default 50 |
| `POST /api/meetings` | New meeting: `201 {"id", "join_url", "meeting"}`. It takes no options (no body, or `{}`); record and AI are on |
| `GET /api/meetings/{id}` | `{"meeting", "link"}`: `link` is the guest link while the meeting is open and under 24 hours old, `null` otherwise. This is how the page lets you copy an open meeting's link again |
| `POST /api/meetings/{id}/token` | your LiveKit token for the call (identity `owner`, the owner grant below); `410` once the meeting has ended, `503` "Calls are unavailable" when LiveKit does not answer |
| `POST /api/meetings/{id}/record` · `/ai` `{"on"}` | the two switches. Off at any time; back on only before any guest has knocked (`409` after) |
| `GET /api/meetings/{id}/events` | your event stream (read with `fetch`: it needs the bearer): `meeting` events with the meeting, its notice, whether the switches are locked, and the lobby; the transcript (`segment` `{"id", "speaker", "kind": "text"\|"gap", "start_ms", "end_ms", "text"}`, a gap sent again when it closes; `amend` `{"id", "text"}`, a line already sent whose text changed: its late closing mark, see [the transcript](#the-transcript); `partials`; `transcript` `{"status"}`). While it is open no knock is pushed to your phone |
| `POST /api/meetings/{id}/lobby/{pid}/admit` · `/deny` | let a waiting guest in, or not. At most 7 guests in the call (8 with you): past that `429` |
| `DELETE /api/meetings/{id}/participants/{pid}` | remove a guest from the call |
| `POST /api/meetings/{id}/end` | End meeting |
| `GET /api/meetings/{id}/transcript?format=json\|txt\|vtt` | the meeting's transcript: `{"meeting", "segments"}`, or a download as text or WebVTT. Yours alone: another person's meeting is `404` |
| `GET /j/{code}` | the guest page; `410` once the meeting has ended or is 24 hours old, `404` for an unknown code |
| `POST /j/{code}/knock` `{"name", "notice": {"record", "ai", "transcribed"}}` | `201 {"ticket"}`. `notice` is what the page showed; another one (or one without `transcribed`) is `409 {"notice": <the current one>}` |
| `POST /j/{code}/wait` `{"ticket"}` | an event stream: `waiting` (with the current notice), then `admitted {"livekit_url", "token", "identity", "ice"}`, `denied` or `ended` |
| `POST /j/{code}/events` `{"ticket"}` | an admitted guest's stream: a fresh `token` (the reconnect after a drop), the current notice (its badge), the same transcript events as the owner's, then `removed` or `ended` |
| `POST /j/{code}/leave` `{"ticket"}` | the guest's own Leave: `204`, dropped from the room, the ticket spent. With contacts configured and a card offered, `200 {"card": {"id", "name", "owner"}}` instead |
| `POST /card` `{"id", "name", "email"}` | contacts only ([Contacts (optional)](#contacts-optional)): the card's Send. `200 {"message"}`, the same whatever contacts does; `400 {"error", "field"}` spends nothing; `404` for a spent, unknown or expired card; `429` past the limits. The `ended` event carries the card as `"card"` |
| `POST /card/skip` `{"id"}` | contacts only: Skip. `204`; nothing is sent anywhere |
| `GET` · `PUT /api/settings` `{"recording_retention_days": 30\|365}` | with a bucket only: your recording retention |
| `GET /api/meetings/{id}/recording` | with a bucket only: `{"parts": [{"part", "state", "start_ms", "duration_ms", "bytes"}], "gaps": [{"from_ms", "to_ms", "why"}], "purge_at", "share": {"live", "expires_at"}}` |
| `POST /api/meetings/{id}/recording-url` | with a bucket only: `{"parts": [{"part", "start_ms", "duration_ms", "url"}], "expires_at"}`, a signed `/media/…` URL per playable part, 15 minutes |
| `GET /media/{id}/{part}?exp=&sig=` | the part, streamed from the store with `Range`; `403` for a wrong or expired signature |
| `POST` · `DELETE /api/meetings/{id}/share` | with a bucket only: `201 {"url", "expires_at"}` (a new link; the old one is turned off), or turn it off; `409` before the meeting has a finished recording |
| `GET /r/{token}` · `/r/{token}/{part}` | the share link's page and its video (with `Range`); one `404` for every link that opens nothing |
| `GET /api/meetings/{id}/timeline` | with a bucket only: the time-aligned bundle ([Recording (optional)](#recording-optional)) |
| `GET /api/meetings/{id}/frame?at=<meeting ms>` | with a bucket only: one JPEG of the recording at that moment; `404` outside it, `503` without ffmpeg |
| `POST /livekit/webhook` | internal listener only: LiveKit's signed webhook |
| `WS /stt/ingest/{track}/{token}` | the ingest listener only: one track egress's audio (above) |
| `GET /healthz` | internal listener only |

**The link** is `https://meet-<x>.<domain>/j/<code>`. The code is
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
them get a `404`; send the new links. Meetings that
have ended are unaffected (they have no link).

The code never appears in a log line, an error, the meetings list, or the
database.

## The lobby and the call

- **A link is a right to knock, not to join.** A knock answers a **ticket**
  (128 random bits, stored only hashed), which the guest page keeps in
  memory and sends only in request bodies: every route that takes one
  refuses a query string, so a ticket never reaches a URL, a log or a
  `Referer`. A ticket opens only its own meeting's streams.
- **A guest holds no LiveKit token before the owner admits them**: the token
  is minted only on the admitted guest's wait stream, after one admission
  check (`store.MayBeInRoom`), so the lobby cannot be skipped by talking to
  LiveKit directly. Guest tokens live **2 minutes** (only to connect) and
  grant join, publish (camera, microphone, screen share and its audio) and
  subscribe, for the meeting's own room: **no** `roomAdmin`, **no**
  `canPublishData` (written as `false`: LiveKit reads an absent one as
  true), not hidden, not a recorder, and the guest cannot rename itself.
  The owner's adds `roomAdmin` and `canPublishData`.
- **A dropped connection is not a revocation.** An admitted guest stays
  admitted until the owner removes them, they press **Leave**, or the
  meeting ends. When their connection fails or drops, the page shows
  "Can't connect media" with **Retry**: a fresh token from their `/events`
  stream, and the call again, without knocking. LiveKit's
  `participant_left` changes nothing.
- **Removing a guest** marks their identity removed in the plugin's
  database, then drops them through LiveKit's `RemoveParticipant`; their
  ticket is dead. A LiveKit token stays valid for its two minutes, so the
  plugin does not rely on it: on every `participant_joined` webhook it
  removes at once any identity that is not the owner's or an admitted
  guest's, and every 15 s a **reconcile loop** lists each live meeting's
  room and does the same, in case a webhook was lost. The loop also deletes
  the room of any meeting no longer live (ended, 24 hours old, its owner's
  grant revoked). A guest who pressed **Leave** knocks again to come back.
- **You on two devices.** You are one identity in your call (`owner`: no
  subject of yours reaches a guest), so opening the meeting on a second
  device moves the call there; the first page says "Opened on another
  device" with **Use here** to take it back, and never rejoins by itself.
- **End meeting** ends it for everyone: the link answers `410`, waiting
  guests are told it ended, the room is deleted (which disconnects
  everyone), and no identity of it passes the admission check any more, so
  a token someone still holds gets them thrown out. A room LiveKit closes
  by itself after 10 minutes empty (`room_finished`) ends the meeting too.
- **The webhook** is acted on only when its `Authorization` JWT is signed
  with the API secret by the API key, is within its life, and carries the
  SHA-256 of the exact body; anything else is `401` and changes nothing.
- **Limits:** 20 guests waiting per meeting, 10 knocks per source address
  per 10 minutes, 8 people in the call; and open event streams: one per
  ticket (a new one closes the old), 16 per client address across the
  guests' and the owner's streams, 2000 in all. Over any of them is `429`.
- **Behind a proxy:** set `MEETINGS_TRUSTED_PROXIES` to the ingress's CIDRs.
  The client is then the right-most `X-Forwarded-For` hop that is not a
  trusted proxy, never anything left of an untrusted hop. Unset, the header
  is ignored, and the first request that carries one is logged as a warning
  and shown on `/healthz`, since every client is then counted as the proxy.
  A catch-all (`0.0.0.0/0`, `::/0`) refuses to start.
- **The guest's name** is trimmed and must be 1 to 40 characters with no
  control, format (which holds every text-direction control) or separator
  characters, and at least one character that shows (not only joiners,
  Hangul fillers, the blank Braille pattern, other default-ignorable
  characters, or combining marks with no base). It is shown with "(guest)" after it, and every page writes it
  with `textContent`, never as markup. It is never logged or audited.
- **The consent notice.** Record and AI action items are on for a new
  meeting. Before the name field the guest reads one sentence per switch
  that is on: "This meeting is recorded and transcribed." (or "…is
  transcribed." with Record off) and "The transcript text (not the audio) is
  sent to an AI service (OpenRouter) to find action items." Everyone in the
  call sees a badge naming what is on ("Recording · Transcribing · AI"),
  always the **current** state. A switch turned off stays off once a guest
  has knocked, so the notice can only shrink after anyone read it; and a
  knock carries the notice its page showed, so a switch the owner changed
  in the meantime is a `409` and the guest reads the new notice first.
- **The knock notification:** when a guest knocks and you do not have the
  meeting open, muster pushes "Someone is waiting to join your meeting" to
  your phone (muster writes the words; the guest's name is never sent).
  Again only when the count of waiting guests rises above the last count
  pushed, which follows the count down, so 2 → 1 → 2 is pushed again;
  withdrawn when you open the meeting or nobody waits any more.
- **Mobile:** below 40rem the call is one column: the tiles (or a shared
  screen on top, the others in a strip), the controls in a bottom bar of
  44 px targets within thumb reach, the consent badge pinned at the top, and
  the lobby and the transcript as a sheet over the stage.

## The meeting UI

The pages draw with **muster's UI kit** (`docs/ui-kit.md` in muster), served
from the **plugin's own origin** at `/kit/v1/kit.css` and `/kit/v1/kit.js`:
the plugin fetches them from muster (`MUSTER_URL/ui-kit/v1/`, server-side,
the same address it already calls), keeps them in memory, revalidates them
with muster's ETag every minute, and serves them. So the theme an admin
picks in muster (a preset, or their own accent) reaches the pages on their
next load, while **no page makes the browser contact muster**: a guest who
cannot reach muster (a LAN-only muster, a firewall) is never held on it,
and muster never sees a guest's address. Both pages' CSP allows script,
style and images from `'self'` only. Each upstream fetch gives up after
2 s; then the last copy from muster is served if there is one, else the kit
bundled with the plugin (`static/kit-fallback.css` and
`static/vendor/muster-ui-kit-v1.js`: muster's own render in its default
theme, by `scripts/vendor-ui-kit.sh` from the commit the tests pin), and
muster is asked again 10 s later. `X-Kit-Source` on the response says which
(`muster`, `muster-stale`, `bundled`). `static/app.css` only places the
kit's components and writes **no colour** (every colour, radius and
duration is a `--mu-*` token; `TestThePluginCSSWritesNoColour` fails on a
literal).

- **Owner, the first time:** New meeting opens the **device check**: the
  camera preview, the microphone's level, the camera and microphone
  pickers, mute and camera toggles, and the meeting's link with Copy (and
  Share where the browser has it). **Join** waits for the check (refused or
  missing devices join without them; a browser prompt left unanswered for
  5 s lets Join go ahead without them). A finished check is remembered in
  the browser (`static/prefs.js`: one `localStorage` key on the plugin's
  origin, holding which camera and microphone and whether each is on;
  nothing else is ever stored).
- **Owner, after that:** New meeting goes straight into the call, with the
  link on an invite card until someone joins. A remembered device that is
  gone, or a permission refused since, joins anyway with what works and
  says so. The camera and microphone pickers stay in the call (the settings
  button), and a device picked there is the remembered one. Storage that
  is blocked means the check again.
- **Guest:** always the check, after the consent notice (in full: this is
  where they agree to it), then their name and **Ask to join**.
- **The call:** a control bar of icon buttons (each with a tooltip and an
  `aria-label`): mute, camera, share screen, the layout, the transcript,
  People (the owner's, with the waiting count as a badge), and leave (the
  owner's opens Leave or End meeting for everyone). A knock is also a toast
  with **Admit**; the People panel admits, denies and removes with icon
  buttons. The consent notice is a compact badge; its popover has the full
  notice and, for the owner, the two switches.
- **Tiles** name each person (as text) and show a muted microphone, a camera
  that is off (with their initials in place of the picture) and LiveKit's
  connection quality as a signal icon; the active speaker's tile has the
  theme's ring.
- **Layouts:** a grid, and a presentation view (one tile on the stage, the
  rest in a filmstrip) that the call switches to by itself when anyone
  shares a screen, and back when the last share stops; the layout button
  overrides it. Your own share shows "You are presenting" with Stop, and the
  browser's own Stop sharing ends it the same way. Where the browser cannot
  share (`getDisplayMedia` missing: phones) the button is not drawn.
- **Keyboard:** M mutes, V turns the camera off, S shares (each button's
  tooltip names its key); never while typing in a field.
- **Motion:** tiles fade and scale in and out, layout changes move tiles
  (FLIP), panels and rows rise in, and toggles animate their pressed state,
  all in the kit's duration tokens, so `prefers-reduced-motion` (which sets
  them to 0) turns every one of them off.

## The transcript

1. When someone who may be in the call (the owner, or an admitted guest)
   publishes a **microphone**, LiveKit's webhook says so, and the plugin
   starts a LiveKit **track egress** for that track: Egress streams its raw
   audio to the plugin's ingest listener,
   `ws://<ingest>/stt/ingest/<track>/<token>`. Egress sends no credential on
   that websocket (spike S2), so the URL's token is the only one: an HMAC
   under `MEETINGS_INGEST_SECRET` of the meeting, the track, a fresh nonce
   per egress, and the meeting's 24 h cap, compared in constant time. A
   token opens one track of one meeting, only while it is that track's
   **current** egress, and never after the meeting ends (`410`) or with
   the transcript off (`404`). The access log names `/stt/ingest/<track>`,
   never the token; Egress's own log does record the full URL it was given,
   which is why a token dies with its egress.
2. The ingest is a small websocket server that **never pings** (Egress
   answers no ping; S2's streams died at exactly 40 s under a 20 s ping) and
   sets no read deadline. It reads Egress's format exactly: binary frames of
   1920 bytes per 10 ms, 16-bit little-endian stereo at 48 kHz, the two
   channels identical. It takes the first channel, filters and resamples it
   to 16 kHz mono, and hands it on in 80 ms chunks. A frame that is not a
   whole number of stereo samples closes the stream.
3. **Times come from the sample count**, never from arrival: a segment is
   placed at the meeting time of its stream's first sample plus its own
   sample time, so a burst after a stall (S2 saw stalls of 7 s) lands where
   it was said.
4. The engine: the plugin's endpointer (energy, after a DC-removing
   high-pass, so a clipped or offset microphone still pauses) cuts the
   audio at 700 ms of silence, at most 15 s per utterance, and the
   `utterance` engine sends each one, as a WAV, to the STT service muster's
   dictation uses, `POST /v1/audio/transcriptions` with the same fields. At
   most 4 are in flight per plugin (it shares the GPU with dictation); each
   participant's queue holds 30 s of audio, and past it the oldest is
   dropped and stored as a gap. A constant signal (silence, a stuck level)
   is never sent.
   **A connected stream ends the retries:** LiveKit can time a start out
   while the Egress it asked does start and connect (measured with no Egress
   attached: the start fails after about 7–10 s). Once the track has a
   stream, no further attempt is made, since a new attempt's token would cut
   that stream off; if that stream (whose egress the plugin never learned)
   ends while the track is published, the plugin starts again on the backoff.
5. **Gaps are stored, not hidden.** When a start is refused (retried after 1,
   2, 4 … 30 s), when an egress dies (`egress_ended` while the track is still
   published: a new egress is started, since Egress never reconnects), when
   the plugin restarts mid-meeting, or when the engine drops or fails an
   utterance, a gap marker is stored and the panel says "Transcript
   paused"; the gap closes when audio flows again. The archive shows where
   text is missing rather than looking complete.
6. **The `nemotron` engine** (live captions; design: "Serving Nemotron").
   Each microphone track gets one websocket to muster's `stt-nemotron`
   server (its README is the protocol): every 80 ms chunk, **silence
   included**, goes as a 2560-byte frame (the server's clock is the samples
   it received, so a frame left out would move every later time); the
   endpointer's end of an utterance sends `flush`, and the server answers
   with that utterance's `final`; when the track ends, the last frame is
   zero-padded and `end` carries the **true** sample count. The server's
   `partial`s are the panel's live captions (in italics, replaced by the
   line when its final comes) and are **never stored**; finals are stored.
   **Late closing marks:** the model emits a sentence's closing mark
   (`.` `?` `!` `,` …) only when the NEXT speech begins, so the server sends
   a final at once without it, and the mark comes as the next final's
   `lead` (`{"text": "Alice will", "lead": "?"}`; `{"text": "", "lead": "."}`
   when nothing more was said). The plugin appends the `lead` to the same
   participant's previous line, if that is their latest stored segment, has
   text and does not already end with a mark (so never `??` or `.?`); it
   is stored, every open panel updates that line in place (an `amend`
   event), and the archive and the exports read the amended line. Marks with
   no such line are dropped; a final with only a `lead` stores no line. No
   schema change.
   When Egress itself stops delivering for 20 s (under the server's 30 s
   idle close), the plugin ends the stream cleanly rather than invent
   silence, and the next audio opens a new one at its own place in the
   track. **Per participant, when the server is full or down:** a refusal
   (`401`, `503`, `503` + `Retry-After`) or a failed connect at open sends
   that participant to the `utterance` engine at once, and the panel says
   **"Captions delayed"**; Nemotron is retried after `max(30 s,
   Retry-After)`, and the participant moves back (the status clears) when a
   stream opens. A stream that drops after it opened (a cut connection, or
   an error close such as `bad_frame`, logged by its code) is reconnected up
   to 3 times, at 1, 2 and 4 s, with a gap marker over the audio no engine
   took (from the last final when a `flush` is unanswered, else from the
   start of the utterance in progress, else from the drop: silence loses no
   words); a reconnect answered `503` goes to the fallback at once. The 3
   reconnects come back only once a connection has delivered a final, so a
   server that closes every new stream before it has (an error on every
   stream) sends the participant to the fallback. A `1013` close
   (`overloaded`) is the server shedding load: the fallback at once, as for
   a `503`, retried after 30 s. When the
   fallback is also full, its own caps apply and the panel says
   **"Transcript paused"**, which wins over "Captions delayed". Lines the
   fallback took are stored as the `utterance` engine's.
   **The silent-start check:** a track with 20 s of speech energy (the
   endpointer's) and no word back from any engine, or 20 s of one constant
   non-zero level (a stuck capture, which the DC filter makes look silent),
   is logged by its track id and counted on `/healthz`; digital silence
   (a muted microphone) is not. Nothing is stored or shown for it.
7. Lines reach the owner's event stream and every **admitted** guest's
   (`segment`, `partials`, `transcript`), never a waiting, denied or removed
   ticket's. Ending the meeting closes the streams and stops the egresses.

🔴 **What was said is untrusted text.** A guest is a stranger, and so is what
they say. The panels and the archive render every line and name with
`textContent` only; the text and WebVTT exports put each line on one line
and escape what the format would read as markup, so no one can forge a line,
a cue or a speaker. In the text export a speaker's name is fenced, with
"(guest)" outside the fence: `[0:05] «Eve» (guest): …`; any `«`, `»` or
`\` in a name is backslash-escaped. A guest named `Ada L.: I resign. Eve`
reads as `«Ada L.: I resign. Eve» (guest): …`, never as the owner speaking.
Lines never reach a log or the audit trail, and whatever
later hands them to a model (phase 4's action items) must fence them as
data.

## What it stores

One SQLite file (`STATE_DIR/meetings.db`, mode `0600`): the people who opened
the tab (the **subject** muster's redeem gave and the display name, **never
a muster user id or an email**), owner sessions (hashed), meetings (state,
the record and AI switches, times, the link code's hash), each knock (the
name typed, the LiveKit identity, the ticket's hash, its state and times; no
address), the transcribed tracks (the track id, the egress, the current
nonce, the speaker's name) and the transcript itself (finals and gap
markers, with the speaker's name and the meeting time; partials are never
stored), with contacts configured each guest card's state and its
id's hash, and until it is delivered (72 h at most) each card's submission,
guest name and email included, in `contacts_outbox`; with a bucket
configured each owner's recording retention, each meeting's purge time, its
recording parts (object key, size, length, state, and the file's start in
meeting time), its recording gaps, its share links (hashed) and when a
screen was shared (and by whom, by name); and an audit trail
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

None. The plugin talks only to the muster at `MUSTER_URL`, to your
LiveKit, and (with the transcript on) to your own STT service at
`MEETINGS_STT_URL`, which gets each utterance's audio, and (with the
`nemotron` engine) your own Nemotron server at `MEETINGS_STREAM_STT_URL`,
which gets each microphone's audio as it is spoken, and (with a bucket
configured) your own S3-compatible store at `MEETINGS_S3_ENDPOINT`, which
Egress writes each recording to and the plugin reads and deletes it from,
and (with contacts
configured) your own contacts plugin at `MEETINGS_CONTACTS_URL`, which gets
the name and email a guest chose to send on the card, the meeting's id,
title and times, and that guest's knock name and times. Audio and video go
between browsers and your LiveKit; only the microphones' audio reaches the
plugin, from your Egress, and it is not stored. Later phases send transcript **text** (never audio) to an AI
service for action items, on by default and off per meeting, as the notice
already tells guests; this README will say so in the release that adds it.

## Tests

```sh
go test -race ./...
go run ./scripts/mutate   # the security-gate mutation sweep
cd e2e && npm ci && npx playwright install chromium && npx playwright test
```

The browser tests (`e2e/`, Chromium) run the plugin against the fake muster
and a fake LiveKit through `scripts/e2e-harness`, with a muster-like parent
on another origin. One loads `/enter` top level and checks it is refused
with no redeem, then frames the same code from the parent and checks the
app opens and makes a meeting. The other, on a 390×844 screen, has a guest
knock with a name that looks like markup, checks the notice comes first and
the owner's lobby row shows the name as text and fits the screen, admits
them, and checks at the fake LiveKit that the guest connected with a valid
guest token; the fake refuses the media, and **Retry** reconnects with a
fresh token and no new knock. A third, against a real `livekit-server`
(`scripts/e2e-harness -livekit=real`), opens one meeting on two owner pages:
the newer takes the call, the older shows "Opened on another device" and
**Use here** takes it back. A fourth (`transcript.spec.ts`) admits a guest,
has the fake LiveKit act as Egress for the owner's and the guest's
microphones (streaming Egress's own PCM format to the ingest), and checks
that the owner's panel and the guest's show the same four lines of the
**fake** engine under the right names, that a scripted line that looks like
markup stays text, and that the archive and its text download hold them
after End meeting. A fifth and sixth (`captions.spec.ts`, at 390×844 and
1280×800) run the plugin's real `nemotron` engine against the fake
Nemotron server (`scripts/e2e-harness -stt=nemotron`): a partial caption
appears in the panel and is then replaced by the final line.

**The UI** (`ui-*.spec.ts`, at 390×844 and 1280×800) runs against the fake
muster, which also serves muster's UI kit from `e2e/fixtures/ui-kit`
(muster's own render at a pinned commit, by `scripts/vendor-ui-kit.sh`;
the harness refuses a fixture that does not match `SHA256SUMS`), so no test
depends on a live muster. `ui-call.spec.ts` serves a **fake LiveKit client**
(`e2e/fixtures/fake-livekit-client.js`, the slice of livekit-client 2.25's
API the page uses) and plays the room's other people: the pre-join (Join
held during the device check, the preview, the level, the pickers, the
toggles), a remote mute and camera-off on the tile within 1 s and unmute
restoring it, the speaking ring following the speaker, the connection
indicator following the quality, a share switching to presentation and back
with the toggle overriding, your own share and the browser's Stop, the
shortcuts, reduced motion (every computed transition and animation 0 s, and
no Web Animation on a layout change), every button's accessible name
(enumerated), axe (no serious or critical violation) on the pre-join, the
call, the lobby and the past meetings, and a phone with no screen sharing.
`ui-call.spec.ts` also checks that tiles that stay put do not re-animate on a
join, a speaker change or a mute. `ui-remember.spec.ts`: the owner's first New meeting shows the check and the
next goes straight in with the link on screen (storage holding only the
device key), a remembered device gone and a permission refused since, blocked
storage meaning the check, a guest always checked, Join (and Ask to join)
let through after an unanswered camera prompt, and a guest's consent notice
above their preview with nothing sent to LiveKit (no room, no track, no
request off the plugin's origin) before they are admitted.
`ui-kit.spec.ts`: the kit served through the plugin with no request from
either page to muster, a second theme reaching the page on reload, the
guest page's CSP, the bundled kit when muster answers `404` for `kit.css`,
`kit.js` or both, and a page not held on a muster that hangs (the plugin's
2 s fetch timeout; `kit_internal_test.go` pins the fetch, the ETag
revalidation, the timeout and which copy is served). `ui-real-livekit.spec.ts` runs
the indicators and a screen share between two browsers through the real
`livekit-server`. `ui-visual.spec.ts` compares goldens
(`e2e/goldens/`: the pre-join, the grid, the presentation view, the lobby
and the transcript at both sizes); pixels depend on fonts, so it runs only
in the pinned Playwright image (CI's `visual` job; remake them with
`scripts/update-goldens.sh`, which needs docker). `go run ./scripts/mutate
-e2e` is the UI's mutation sweep: each mutant (an indicator not updated, the
ring not following, presentation not switching, an `aria-label` missing,
reduced motion ignored, …) must turn its named Playwright test red.

**Contacts** (`contacts-card.spec.ts`, at 390×844 and 1280×800, against the
fake contacts API, `scripts/e2e-harness -contacts=fake`): the card after
End, prefilled, with the whole consent sentence, an invalid email refused
in place, Send reaching contacts once, and none in the owner's frame; the
card after the guest's own Leave, Skip sending nothing, and no second card
at End; the same answer with contacts down and delivery once it is back;
no card for an owner contacts is not accepting; no rise or fade under
reduced motion; and, with no contacts URL, no card on either path and no
card route. In Go, `internal/contacts` runs the contacts plugin's own
contract (`contactsapi.RunContract`, every case of its `routes.json`)
against `internal/contactstest`, the fake every other test talks to, and
checks the body meetings sends against `contactsapi.Submission`'s check and
the outbox's action for every status the contract answers;
`internal/server` checks the card's fields against its `fields.json`; the outbox (each answer class, the backoff, the 72 h
drop, a retry after a lost answer making exactly one submission, insertion
order with one failure holding nothing back, no PII in its log), and
`internal/server`'s `card_test.go` and `nourl_test.go` the eligibility (each
condition alone false, and all true), the spending, the uniform answer, the
limits, contacts black-holed, and the no-URL goldens.

**The recording** (phase 5): `recording.spec.ts` runs the harness with
`-s3=fake` (the fake S3 store, `internal/s3test`, which checks every
request's SigV4; the fake LiveKit as the room composite Egress, which
"uploads" a 20 s VP9 MP4 from `internal/frames/testdata`). At 390×844 and
1280×800 the owner and a guest see "Recording · Transcribing · AI", then
"Recording failed · …" when the egress is killed and its restarts are
refused, then "Recording …" again once a restart is taken, the badge on
screen each time. At 1280×800 the archive plays the video beside the
transcript (the video's column left of the lines), the browser reads its
length (20 s), and a click on each line (and Enter on one) sets the video's
`currentTime` to that line's offset; and a share link opens in a context
with no muster, plays the video from byte ranges, and answers `404` "This
recording is not available" once **Stop sharing** is pressed. In Go: the
offset math with literal values at the meeting's start and mid-meeting after
a restart (`internal/timeline`); a part's start is the egress's reported
start, not the webhook's arrival (reported 6.2 s, arriving 9 s); a failure
restarts as part 2 with its gap and the badge for owner and guest, and each
segment's offset into its part (`TestAFailedEgressRestartsAsANewPart…`);
a refused start; Record off and End stop the egress; the reconcile ends a
part whose webhook was lost; the share door's one 404 (unknown, malformed,
missing part, revoked, expired; page and media); its Range answers; the
signed media URL (expired, tampered, another meeting's path: `403`); the
sweeper (object then row; the store down leaves both; a repeat is
harmless); retention 30/365 and the purge time every end gives; migration 6
on a schema-5 database; the SigV4 signer against AWS's own published
example; frame sampling at four meeting times on the H.264 and the VP9
fixtures, read through byte ranges (`internal/frames`, ffmpeg needed:
`MEETINGS_REQUIRE_FFMPEG=1` in CI fails rather than skips); and the
no-bucket golden. The fixtures were made with ffmpeg 7.1 (`nullsrc` →
`geq` colouring each second red, green, blue, black; 64×64, 5 fps, 20 s;
`libx264` and `libvpx-vp9`, `+bitexact`); the H.264 one is also the image
build's smoke test.

**Against a real LiveKit:** `internal/server/livekit_integration_test.go`
starts `livekit-server` and joins with LiveKit's CLI (`lk room join`): the
plugin's rooms, tokens and webhook verification against the real server; two
admitted guests in the room; a removed guest rejoining with a valid
credential and being thrown out by the plugin (`PARTICIPANT_REMOVED`); an
admitted guest whose connection drops reconnecting and staying; two `owner`
connections, the newer one winning and neither removed by the plugin; End
meeting deleting the room; a real `room_finished` ending a meeting.
`transcript_integration_test.go` has the CLI publish a real Opus microphone
(`testdata/tone.ogg`): the real `track_published` is read as the owner's
microphone, LiveKit's real answer to `StartTrackEgress` with no Egress
attached (a `408` after 7 s) is retried with a gap, and the real
`track_unpublished` ends the track. **LiveKit's Egress is not in nixpkgs**
and is not run in CI, so in that test (and every other) the test itself, or
the fake LiveKit, plays Egress: it dials the ingest with the token of the
plugin's current attempt and streams Egress's measured PCM format. The real
Egress first runs in a deployment. It runs
when `livekit-server` and `lk` are on `PATH` (`nix-shell -p livekit
livekit-cli`) or named by `MEETINGS_LIVEKIT_SERVER` and `MEETINGS_LK_CLI`,
and skips otherwise, unless `MEETINGS_REQUIRE_LIVEKIT=1` (CI downloads
pinned releases and sets it), when it fails. On NixOS, set `PLAYWRIGHT_BROWSERS_PATH` to
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
is held to core's real behaviour (checked against muster PR #936): the
door's order, every revoke cause, `owners/check` answering every subject
and its 60 calls a minute, and notify's and tasks' answers, keys and caps,
so a 2b or phase-6 test cannot pass against the fake and fail against
muster. It has its own tests for those rules.

`scripts/mutate` applies one mutation per security gate (accept an unframed
`/enter`, redeem before refusing it, accept a personal token, drop the
subject filter, ignore an `owners/check` absence, ignore `/self`'s `401`,
send a knock without `Muster-Act-For`, set a cookie, a 64-bit link, log the
query string; and for 2b: a token on knock, a ticket on another meeting,
a ticket as an owner bearer, a deny that admits, `roomAdmin` or
`canPublishData` for a guest, a removal that skips LiveKit, no
`participant_joined` check, an unverified webhook, an End that keeps the
room, a knock that ignores its notice, a switch that turns back on, a bidi
name, a name rendered as markup, each knock limit, a knock pushed while the
owner watches, the reconcile loop, …; and for 3c: each of the `nemotron`
boot checks, `end.samples` counting the padding, an unpadded last frame,
silence left out, no `flush`, no DC removal, no fallback, never retrying,
ignoring `Retry-After` or the 30 s floor, reconnecting without the waits,
falling back too early or never, no gap marker, a reconnected stream's
times from zero, the token in a refusal's error, each half of the
silent-start check, a partial stored, "Captions delayed" never shown or
never cleared, the stream token made required), runs the one test named to catch it, and requires that
test to fail **with its expected message**; a mutant that fails to build or
trips another assertion is reported, not counted. Each sweep also runs a
known-caught mutant and a no-op edit that must survive. CI runs gitleaks,
`gofmt`, `go vet`, `go test -race` and the sweep on every push, and renders
`deploy/kubernetes` and `deploy/example-overlay` through `kubeconform`.
`deploy/deploy_test.go` reads what `kubectl kustomize` renders and the LiveKit
config as LiveKit parses it; without `kubectl` it skips, unless
`MEETINGS_REQUIRE_KUBECTL=1` (CI and the sweep set it).

**Tested against:** the fake muster only, built to the design of muster's
core PR 1h (docs/design/meetings.md at muster commit `9bf834178`), before
that core change exists, and a real `livekit-server` 1.13.11 with `lk`
2.19.4 (no media: signalling, rooms, tokens and webhooks). Not yet run
against a live muster, and no browser has carried media through it: that is
phase 2d's click path, with TURN, after 2c deploys it.

### The action-item evaluation (phase 4a)

`cmd/items-eval` replays the labelled corpus in `testdata/items-eval/`
(twelve synthetic meetings; its README is the format) through the worker's
own code: `items.Loop` (a tick every 10 s, `items.Select`'s window 90 s
back and anchored on the last segment a successful call covered, the
adapter, `items.Guard`, and the failure, backoff and "paused after 3"
rules that phase 4b's worker drives on the wall clock). It scores by cited segments (no model judges a model), runs
its four controls first (null, oracle, duplicator, liar: a control that
does not score as stated stops the run before any model is called), and
picks the default model. **It costs money and needs a key, so it is run by
hand, never in CI:**

```sh
OPENROUTER_API_KEY=… go run ./cmd/items-eval \
  -prefs 'deepseek/deepseek-v4.1-flash-1017={"order":["<provider>"],"allow_fallbacks":false}' \
  -md docs/items-eval-$(date -u +%F).md
go run ./cmd/items-eval -controls-only   # the lint and the controls alone: no key, no cost
```

Every request goes through production's client with production's
`provider` object (`items.ParsePrefs`: `data_collection: "deny"` always,
and a pin for a `chat_json` model). CI runs the scoring against a
hand-computed fixture, the corpus lint, the controls on the real corpus,
both adapters against a fake OpenRouter, and a mutant for every guard. No
action item reaches a meeting yet: the worker, its table and its panel are
phase 4b.

## Layout

| path | what |
|---|---|
| `main.go` | reads the environment, starts the listeners and loops |
| `internal/muster` | the client for muster's plugin routes (`/self`, the redeem, `owners/check`, notify, tasks) |
| `internal/livekit` | LiveKit: access tokens and their grants, the RoomService API, webhook verification |
| `internal/livekittest` | a fake LiveKit for tests (`httptest`): rooms, participants, tracks, and Egress (it streams PCM to the ingest) |
| `internal/ingest` | the ingest: the websocket server that never pings, Egress's PCM parser and resampler, the URL token |
| `internal/stt` | the `Engine` interface, the endpointer, the `utterance` engine and its client for muster's STT service, the `nemotron` engine and its websocket client, the `fake` engine |
| `internal/stttest` | a fake Nemotron server for tests: the exact protocol, scripted text, every refusal and error close, drops |
| `internal/egresstest` | a fake Egress client and Egress-format PCM fixtures |
| `internal/lobby` | the guest name, the notice's words, the change hub, the knock limit and the knock notifier |
| `internal/mustertest` | a fake muster for tests (`httptest`), server-installed apps included |
| `internal/owners` | the one server token, the boot check and reconcile, and the `/self` and `owners/check` poll |
| `internal/store` | SQLite: schema, sessions, the owner-scoped repository, the lobby and its state machine, random codes, the recording's parts, gaps, share links and retention |
| `internal/s3`, `internal/s3test` | the recordings bucket's client (SigV4: read with `Range`, size, delete) and a fake S3-compatible store that checks every signature |
| `internal/timeline` | meeting time ↔ recording part and offset |
| `internal/frames` | frame sampling with ffmpeg through a loopback range proxy to the store; its fixtures in `testdata/` |
| `internal/server` | the HTTP routes, headers, the webhook and reconcile loop, and the pages (`web/`; LiveKit's browser client vendored in `web/static/vendor/`, by `scripts/vendor-livekit-client.sh`) |
| `Dockerfile`, `.github/workflows/image.yml` | the image and its GHCR publish |
| `deploy/kubernetes`, `deploy/example-overlay` | the kustomize base (the plugin, LiveKit, Services, NetworkPolicies) and an example tenant overlay |
| `compose.meetings.yml` | the fragment for muster's Compose stack |
| `deploy/deploy_test.go` | the rendered objects and both LiveKit configs, pinned |
| `internal/items` | action items: the `extract` and `chat_json` adapters, their OpenRouter client, the guards after the model, the window selection and the worker's loop (`items.Loop`), `MEETINGS_ITEMS_PROVIDER_PREFS` |
| `cmd/items-eval`, `testdata/items-eval` | the action-item evaluation (run by hand) and its labelled synthetic corpus |
| `scripts/mutate` | the security-gate mutation sweep |
| `scripts/e2e-harness`, `e2e/` | the browser tests: the `/enter` frame gate, the lobby at 390×844, two owner devices, the transcript in both panels, live captions, and the UI (`ui-*.spec.ts`, goldens in `e2e/goldens/`); the harness also serves muster's UI kit from `e2e/fixtures/ui-kit` |
| `scripts/vendor-ui-kit.sh`, `scripts/update-goldens.sh` | render muster's UI kit at a pinned commit (the fixtures and the built-in fallback); remake the visual goldens in the pinned Playwright image |

## Licence

MIT. See [LICENSE](LICENSE).
