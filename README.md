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
the archive, with **Download text** and **Download WebVTT**. Action items
and recordings come later: the two switches (**Record**, **AI action
items**) and the notice are already here, because the notice says what will
happen.

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
| `MEETINGS_STT_ENGINE` | no | the transcript's engine: unset or `off` (no transcript: no egress is started, the ingest refuses every stream, and the notice and badge do not say "transcribed" or name the AI service), `utterance` (each pause-cut utterance, at most 15 s, to the STT service below), or `fake` (tests only; refuses to start unless `MEETINGS_TEST_MODE=1`). `nemotron` (live captions) is phase 3c and refused by this build |
| `MEETINGS_STT_URL` | with `utterance` | the STT service muster's dictation uses, `http(s)://host[:port]`, e.g. `http://stt-cohere:8000`: the plugin calls its `POST /v1/audio/transcriptions` exactly as muster does |
| `MEETINGS_STT_TOKEN` | with `utterance` | that service's bearer: a **copy** of stt-cohere's one token (`STT_API_TOKEN`, muster's `MUSTER_STT_TOKEN`), in the plugin's own Secret; rotating it means updating both. Never logged |
| `MEETINGS_STT_MODEL` | no | the `model` form field, sent only when set (stt-cohere ignores it; speaches needs it) |
| `MEETINGS_INGEST_SECRET` | with an engine | 32 or more random bytes, base64 (`openssl rand -base64 32`): the key of every ingest URL's token (below). Never logged |
| `MEETINGS_INGEST_URL` | with an engine | the ingest listener as **Egress** reaches it, e.g. `ws://muster-meetings:8082`; each track egress streams to `<this>/stt/ingest/<track>/<token>` |
| `MEETINGS_INGEST_LISTEN` | no | the ingest listener; default `:8082`. Egress's alone: never route it |

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
  LiveKit's `:7880`, stt-cohere's `:8000` and DNS. LiveKit admits `:7880`
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
| hostnames, muster's address and origin, trusted proxies, the client-IP header, the transcript | `configMapGenerator` `muster-meetings-params`, `behavior: merge`: `muster-url` (`http://<muster svc>.<ns>.svc.cluster.local.:<port>`, the trailing dot included), `cluster-domain`, `muster-origin`, `public-url`, `livekit-url`, `turn-domain`, `trusted-proxies`, `client-ip-header`, `stt-engine` (`utterance`, or `off`), `stt-url` (stt-cohere's Service) |
| namespace | `namespace:` |
| the plugin's image | `images:` (pin a digest) |
| NodePorts | a patch on `livekit-nodeport` (`/spec/ports/0/nodePort` signalling, `/spec/ports/1/nodePort` TURN) |
| the PVC's StorageClass | a patch on `muster-meetings-state` |
| a Secret's name | a patch on the Deployment's `secretKeyRef` or volume |
| the ingress controller's namespace, muster's pod labels and port, stt-cohere's pod labels and port | a patch on the `muster-meetings` NetworkPolicy (`/spec/egress/0` muster, `/spec/egress/2` stt-cohere) |
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

**LiveKit now depends on `redis-meetings`**: one replica, persisting nothing. What a Redis restart does to a running LiveKit and Egress (whether LiveKit refuses to start without it, and whether rooms and egresses survive the restart) is **UNVERIFIED**: it was not measured. Treat a Redis restart as one that may drop the calls and transcripts in progress. The plugin's own meetings, links and transcripts are in its SQLite and are not affected.

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
| `GET /api/meetings/{id}/events` | your event stream (read with `fetch`: it needs the bearer): `meeting` events with the meeting, its notice, whether the switches are locked, and the lobby; the transcript (`segment` `{"id", "speaker", "kind": "text"\|"gap", "start_ms", "end_ms", "text"}`, a gap sent again when it closes; `partials`; `transcript` `{"status"}`). While it is open no knock is pushed to your phone |
| `POST /api/meetings/{id}/lobby/{pid}/admit` · `/deny` | let a waiting guest in, or not. At most 7 guests in the call (8 with you): past that `429` |
| `DELETE /api/meetings/{id}/participants/{pid}` | remove a guest from the call |
| `POST /api/meetings/{id}/end` | End meeting |
| `GET /api/meetings/{id}/transcript?format=json\|txt\|vtt` | the meeting's transcript: `{"meeting", "segments"}`, or a download as text or WebVTT. Yours alone: another person's meeting is `404` |
| `GET /j/{code}` | the guest page; `410` once the meeting has ended or is 24 hours old, `404` for an unknown code |
| `POST /j/{code}/knock` `{"name", "notice": {"record", "ai", "transcribed"}}` | `201 {"ticket"}`. `notice` is what the page showed; another one (or one without `transcribed`) is `409 {"notice": <the current one>}` |
| `POST /j/{code}/wait` `{"ticket"}` | an event stream: `waiting` (with the current notice), then `admitted {"livekit_url", "token", "identity", "ice"}`, `denied` or `ended` |
| `POST /j/{code}/events` `{"ticket"}` | an admitted guest's stream: a fresh `token` (the reconnect after a drop), the current notice (its badge), the same transcript events as the owner's, then `removed` or `ended` |
| `POST /j/{code}/leave` `{"ticket"}` | the guest's own Leave: `204`, dropped from the room, the ticket spent |
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
- **Mobile:** below 40rem the call is one column: the active speaker or a
  shared screen on top, the others in a strip, the controls in a bottom bar
  of 44 px targets, the lobby rows and the badge pinned at the top.

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
6. Lines reach the owner's event stream and every **admitted** guest's
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
stored), and an audit trail
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
`MEETINGS_STT_URL`, which gets each utterance's audio. Audio and video go
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
after End meeting.

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
owner watches, the reconcile loop, …), runs the one test named to catch it, and requires that
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

## Layout

| path | what |
|---|---|
| `main.go` | reads the environment, starts the listeners and loops |
| `internal/muster` | the client for muster's plugin routes (`/self`, the redeem, `owners/check`, notify, tasks) |
| `internal/livekit` | LiveKit: access tokens and their grants, the RoomService API, webhook verification |
| `internal/livekittest` | a fake LiveKit for tests (`httptest`): rooms, participants, tracks, and Egress (it streams PCM to the ingest) |
| `internal/ingest` | the ingest: the websocket server that never pings, Egress's PCM parser and resampler, the URL token |
| `internal/stt` | the `Engine` interface, the endpointer, the `utterance` engine and its client for muster's STT service, the `fake` engine |
| `internal/egresstest` | a fake Egress client and Egress-format PCM fixtures |
| `internal/lobby` | the guest name, the notice's words, the change hub, the knock limit and the knock notifier |
| `internal/mustertest` | a fake muster for tests (`httptest`), server-installed apps included |
| `internal/owners` | the one server token, the boot check and reconcile, and the `/self` and `owners/check` poll |
| `internal/store` | SQLite: schema, sessions, the owner-scoped repository, the lobby and its state machine, random codes |
| `internal/server` | the HTTP routes, headers, the webhook and reconcile loop, and the pages (`web/`; LiveKit's browser client vendored in `web/static/vendor/`, by `scripts/vendor-livekit-client.sh`) |
| `Dockerfile`, `.github/workflows/image.yml` | the image and its GHCR publish |
| `deploy/kubernetes`, `deploy/example-overlay` | the kustomize base (the plugin, LiveKit, Services, NetworkPolicies) and an example tenant overlay |
| `compose.meetings.yml` | the fragment for muster's Compose stack |
| `deploy/deploy_test.go` | the rendered objects and both LiveKit configs, pinned |
| `scripts/mutate` | the security-gate mutation sweep |
| `scripts/e2e-harness`, `e2e/` | the browser tests: the `/enter` frame gate, the lobby at 390×844, two owner devices, and the transcript in both panels |

## Licence

MIT. See [LICENSE](LICENSE).
