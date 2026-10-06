# The AMM on a skein

A constant-product AMM between BSV and one Mandala token per pool, served by
a skein overlay app. Decided in shruggr/skein#120: Mandala is components (a
topic manager and a lookup service, shruggr/skein-mandala); the AMM is an
app that carries them with its own pool lookup, validator and relay; one
topic per token, registered by the owner at runtime.

## The pieces

| role | source | what it does |
|---|---|---|
| `overlay` | skein-overlay 0.7.8 (`bin/overlay.wasm`, copied) | a submission in box `amm/submit`, by message or from `/submit` (delivery only), answered to the submitter's box; `/lookup`, gossip, the listing routes; `register` / `deregister` a topic; hands every admitted BEEF to the chain app |
| `mandala-topic` | skein-mandala 0.6.0 (copied) | judges `tm_<txid>` by the BRC-162 rules; `tm_mandala` admits every deploy |
| `mandala-lookup` | skein-mandala 0.6.0 (copied) | `ls_mandala`, `ls_mandala_deploys` |
| `amm-lookup` | `programs/amm-lookup` | `ls_amm`: the live pools that pass the pool checks, per token |
| `amm-validator` | `programs/amm-validator` | the validator's three direct calls; submits by message to its own overlay |
| `amm-p2p` | `programs/amm-p2p` | the liveness beacon, the relay, the pages (`www/` from the app's tree) |
| | `src/pool.zig` | the pool library (module `pool`), over `mandala`'s parser and rules |
| | `pool/Pool.runar.go` | the contract |
| | `web/ui` → `www/` | the AMM pages; `www/mandala/` the Mandala pages |

The topic knows no application: a pool output is a token output like any
other and is admitted exactly when the BRC-162 rules admit it. Which of
those outputs are pools is the lookup's judgement, and whether one may be
spent is the validator's.

## The pool rule

A pool is a BRC-162 value output at output 0 whose lock is the compiled
Pool code, `OP_RETURN`, and the state: TokenReserve (8 bytes), LpPubKey,
ValidatorPubKey, ValidatorIdentity (33 bytes each). The BSV reserve is the
output's satoshis; the token reserve is the prefix's amount, duplicated in
state.

- **Indexed** (amm-lookup, `pool.check`): at output 0; the prefix's id and
  amount equal the AssetId and TokenReserve the code and state carry (the
  contract writes the prefix from its copies and never reads it). The id is
  32 bytes, so only a token deployed at output 0 has pools. The
  ValidatorPubKey is not checked.
- **Methods** (the contract): `Swap(validatorSig, nextValidatorPubKey,
  amountIn, bsvIn, userPkh, commissionPkh)`; `AddLiquidity(lpSig,
  validatorSig, nextLpPubKey, nextValidatorPubKey, addBsv, addTokens)`;
  `RemoveLiquidity`, the LP's alone. Fees are fixed at deploy, in basis
  points of the input, rounded up, paid in the input asset: LpFeeBps to the
  LP, ValidatorFeeBps to the validator, CommissionBps to whoever relays the
  swap. The contract builds every output; a swap carries no others.
- **Keys rotate.** Each party names its next key on every spend it signs.
  The validator's key is the BRC-42 child of its ValidatorIdentity,
  protocol `[1, "amm pool"]`, key ID the outpoint of input 0 of the
  transaction that created the pool, counterparty anyone. The overlay does
  not check it; the validator does before signing.
- **The validator signs last** (amm-validator README "What is checked"):
  the pool is ours and live, the call is the method's, the next key is our
  child, the topic would admit the transaction, the pool checks pass, the
  outputs are the contract's, every other input (and every unproven parent)
  is signed. Then it signs the pool input through the signer and submits
  the transaction to its own overlay by message — `{fn: "submit", args:
  {beef, topics: [tm_<txid>]}}` from the instance to itself, box
  `amm/submit` —
  and answers the direct call on the engine's first answer (`admitted`,
  `rejected`).

## Interfaces

**Overlay** (base URL `<handle>.<host>/amm`):

| route | what |
|---|---|
| `POST /submit` | the submission message's transport (`X-Topics` a registered topic): `200 {id}`, delivery only; no STEAK (skein-overlay 0.7.2+); the submission event is admitted into box `amm/submit` (0.7.6) |
| `POST /lookup` | BRC-24: `ls_amm` `{tokenId}`, `{tokenId, outpoint, beef?}`, `{tokenId, validatorIdentityKey}`; `ls_mandala`, `ls_mandala_deploys` (skein-mandala README) |
| `GET /listTopicManagers`, `/listLookupServiceProviders`, `/getDocumentationFor…` | the listings (each program's `metadata` / `documentation`) |
| `GET /live` | amm-p2p: the validators heard from |
| `POST /call` | amm-p2p: `{fn, args}` for the three interfaces below |
| `GET /…` (prefix `/`) | amm-p2p `serve`: the pages, `www/` of the app's own tree (skein-sdk `files.serve`) |

**Box `amm/register`** (the manifest's `"register"`, relative to the app,
shruggr/skein#128): the engine's `register {topic, program}`, `deregister
{topic}`, from the owner. skein-overlay 0.7.7+ takes them in this box only
(0.7.5–0.7.6: `amm/overlay`).

**Box `amm/submit`** (the manifest's `"submit"`, `filter: "beef"`): the
engine's `submit {beef, topics, offChainValues?}` from anyone (the
validator's own submissions among them) and `POST /submit`'s submission
event (skein-overlay 0.7.6), answered to the sender in this box; `register` / `deregister` here are refused (`bad-args`).

**Box `amm`** (a message `{fn, args}`, skein docs/APPS.md §4):

| interface | functions | from |
|---|---|---|
| `amm.swap/1` | `submit` (writes), `status`, `terms` | anyone (the owner too) |
| `amm.pool/1` | `submit` (writes), `status` | anyone |
| `amm.liquidity/1` | `submit` (writes), `status` | anyone |
| the engine's | its own `watch`, `resume` (and the libp2p routes' admits, row 3) | the instance itself, events |
| amm-p2p's | `{kind: "amm-p2p-start" \| "amm-p2p-stop"}` (the manifest's `start` / `stop`; the beacons, and on a market host the subscriptions) | the owner |

`amm.*.submit` takes the funding transaction (the wallet's `noSend`
action) and the swap, deploy or add, checks the pair, records it under
`amm/app` and relays it to the pool's validator over libp2p; the record
settles `accepted`, `refused`, `timeout` or `failed` (amm-p2p README "The
marketplace relay").

**libp2p**: `/amm-validator/1/swap`, `/addLiquidity`, `/deploy`
(amm-validator; one signed-message package per frame); the engine's
`<topic>`, `-admit`, `-proof` for each registered topic; `tm_<txid>-live`,
published by the host's beacon (below), and subscribed by a host serving a
market (below, "The market role").

**Box `amm/amm-p2p`**: the owner's `{kind: "amm-p2p-start" | "amm-p2p-stop"}`
(the Validator page), the cron provider's ticks (the fallback, `jobs`), and
admitted heartbeats (`amm-live`, on a market host: below).

**Box `amm/validate`** (0.3.2): the owner's `{fn: "validate" | "unvalidate",
args: {topic}}` (the Validator page). The row `validate` from `$owner` is the
permission: nobody else's message is admitted into the box, and amm-p2p takes
the two calls in this box only (elsewhere its step errors `NotTakenHere`),
with no sender check of its own — as the engine takes `register` in
`amm/register` only.

**Validation, per topic** (0.3.1, David 2026-10-06: beaconing is not a
role; it comes with validating a topic). The owner sets it up like a
topic's registration: `{fn: "validate", args: {topic: "tm_<txid>"}}` adds
the topic to the validated set (the record's `validated` under `amm/p2p`)
and emits its beacon; `{fn: "unvalidate", args: {topic}}` removes it and
emits `unbeacon`. Both are idempotent and answered `{topic, validating}` to
the sender's box `amm` (when the address book reaches it). They are taken in
box `amm/validate` only, which row 2 (`validate` from `$owner`) admits the
owner into (0.3.2). The market role is independent: a node may validate,
host a market, or both.

**One setting** (0.3.2, David 2026-10-06): "If I'm validating, I'm pinging,
I'm taking on new liquidity, and I'm validating." The validated set is the
one switch, and there is no other: for a topic in it the node beacons
`tm_<txid>-live` (pinging), its validator accepts an LP's liquidity —
signs an `addLiquidity` and consents to a pool `deploy` as the pool's
validator (taking on new liquidity means receiving it, not the node putting
up funds of its own) — and signs swaps. For a token whose topic is not in
the set, amm-validator refuses every direct call, `swap`, `addLiquidity` and
`deploy`, with `{ok: false, reason: "not_validating", detail}` before
checking or signing anything. It reads the set from amm-p2p's record (the
`validated` of the head `amm/p2p`) at each call; `validate` / `unvalidate`
are the only writers.

**The beacon** (shruggr/skein#126): on start amm-p2p emits, per validated
topic, `{event: "beacon", topic: "tm_<txid>-live", every: heartbeatSeconds ×
1000, body}` — the host's libp2p node publishes `body` there every `every`
ms, logging nothing per beat — and `unbeacon {topic}` for a topic it
beaconed that is not validated; on stop, `unbeacon` for each, the validated
set kept (the beacons standing: map `beacons` under `amm/p2p`). `body` is amm-poc#3's signed heartbeat
`{identityKey, peerId}`; each beat is the host's signed frame (skein
387e057, docs/MESSAGES.md "Beacons"): dag-cbor `{body, at, sender,
signature}`, `at` the beat's time, `sender` the instance's identity key,
`signature` the instance's under `[2, "metanet handles envelope"]` / `send`
/ anyone over sha2-256 of dag-cbor `{kind: "beacon", topic, body, at,
sender}`. A reader (`liveness.judge`) checks the frame's signature against
`sender` (over the topic it heard it on), `at` against its clock, then the
body: `identityKey` is `sender` and `peerId` is the GossipSub publisher. A
start that names `jobs` asks the cron provider instead (the 0.1.0 path,
kept as the fallback; it signs the same frame itself, through the signer).
The peer ID is derived: the signer's public key for `[2, "skein
instance"]`, key ID `libp2p:<handle>`, counterparty self, as an identity
multihash — the key the host's node runs (skein 387e057, signer.ts
`peerKey`, from the instance's root).

**The market role** (shruggr/skein#120, David 2026-10-06: validator
liveness by role). `config.amm.ammP2p.market` (default `false`). On a host
that serves a market, the start also emits, per served token topic (the
registered set under `amm/topics` and any declared), `{event: "subscribe",
topic: "tm_<txid>-live", program: "amm-p2p", fn: "validateLive"}`
(shruggr/skein#119), once: the kernel delivers each beat on it to
`validateLive`, which judges the host-signed frame (`liveness.judge`) and
admits an accepted one as `{kind: "amm-live", …}` into `amm/amm-p2p`;
stepped, it is applied to the validator map `live` under `amm/p2p` (a later
`at` replaces an earlier one). The relay picks a pool's validator from that
map, live within `offlineSeconds`, as amm-poc did. A start unsubscribes
(`{event: "unsubscribe", topic}`) a topic subscribed before and no longer
served, and a stop, or a start with the role off, unsubscribes every one
(the set: map `subscriptions` under `amm/p2p`). A host not serving a market
subscribes nothing; its map stays empty and its relay refuses a swap
`validator_offline`. A validator's host beacons its validated topics as
above, whatever its role.

## The manifest

`etc/app.json`: the six programs; `config.overlay` with no topics and the
lookups `ls_mandala`, `ls_mandala_deploys` (both `mandala-lookup`) and
`ls_amm` (`amm-lookup`), none with a `topics` list, so each listens to every
topic served; gossip on (the default); `config.amm` (`ammP2p`:
`heartbeatSeconds`, `offlineSeconds`, `market`; `ammValidator`, `commission`);
`provides` the three `amm.*` interfaces; `requires: ["chain/1"]`; `start` /
`stop`.

### The rows

In table order; mailbox addresses are relative to the app (shruggr/skein#128:
`""` is the box `amm`, `"x"` is `amm/x`); the kernel takes the first row of
the package's transport and address whose sender rule admits the sender:

1. `register` from `$owner` → `overlay` (register / deregister)
2. `validate` from `$owner` → `amm-p2p` (validate / unvalidate; 0.3.2)
3. `""` from `event` → `overlay`
4. `""` from `$self` → `overlay` (the engine's own watch, resume)
5. `""` from `*` → `amm-p2p` (the relay's interfaces; the owner's start / stop)
6. `amm-p2p` from `*` → `amm-p2p`
7. `submit` from `*` → `overlay`, `filter: "beef"` (submissions from anyone, by message and from `POST /submit`; skein-overlay 0.7.6)
8. http `/listTopicManagers`, `/listLookupServiceProviders`,
   `/getDocumentationForTopicManager`, `/getDocumentationForLookupServiceProvider`
   → `overlay`
9. http `/live` → `amm-p2p` `live`; `/call` → `amm-p2p` `call`
10. http `/` (prefix) → `amm-p2p` `serve`, `root: "www"`, `index: "index.html"`
11. libp2p `/amm-validator/1/swap`, `/addLiquidity`, `/deploy` →
   `amm-validator` (each refused `not_validating` for a topic outside the validated set)

then, derived by the install from `config.overlay`: http `/submit` (`filter:
beef`) and `/lookup` → `overlay` (exact paths match before the `/` prefix).

Rows 3 and 4 are the derived engine rows, listed so they come before row 5:
a `*` row admits events and the instance's own messages too, so after it
the engine would get neither.

## Not wired

- **The want-answer stream** `/skein/overlay/beef/1.0.0` (skein-overlay
  0.7.1+'s manifest row): not carried, as skein-mandala 0.6.0 does not; a
  submission paused on a parent resumes only when a later submission brings
  it.
- **`tm_<txid>-live` off a market host.** `-live` is consumed by a host
  serving a market (`ammP2p.market`, above); on any other host nothing
  subscribes it, so `/live` lists no one and the relay finds no validator.
- **Catch-up and proofs by block.** Never specified; sync is
  shruggr/skein#112's `want`. The `/amm/proofs/1.0.0` row is gone and no
  start schedules the catch-up pass; the code stays as a utility
  (`proofsByBlock`, `catchup`).
- **The subscription on register / deregister.** amm-p2p is not stepped by
  the engine's registrations (the owner's `register` goes to the engine
  alone): on a market host a topic registered after the start is subscribed
  at the next start; one deregistered is unsubscribed at the next start or
  stop. Beacons follow the validated set, not the registrations.
- **Governance per token** (#120 item 4) and the other Mandala queries:
  skein-mandala docs/MANDALA.md "Not built".
- **Run end to end.** The programs are tested natively and the pages
  against fakes; this app has not been installed on a skein.
