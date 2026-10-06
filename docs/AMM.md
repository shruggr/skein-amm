# The AMM on a skein

A constant-product AMM between BSV and one Mandala token per pool, served by
a skein overlay app. Decided in shruggr/skein#120: Mandala is components (a
topic manager and a lookup service, shruggr/skein-mandala); the AMM is an
app that carries them with its own pool lookup, validator and relay; one
topic per token, registered by the owner at runtime.

## The pieces

| role | source | what it does |
|---|---|---|
| `overlay` | skein-overlay 0.9.0 (`bin/overlay.wasm`, copied) | a submission in box `amm/submit`, by message or from `/submit` (delivery only), answered to the submitter's box; `/lookup`, gossip, the listing and documentation reads; `register` / `deregister` a topic, and with it the market's liveness and the validator's beacon on `tm_<txid>-live` (below, "Market and validator"); hands every admitted BEEF to the chain app |
| `mandala-topic` | skein-mandala 0.7.1 (copied) | judges `tm_<txid>` by the BRC-162 rules; `tm_mandala` admits every deploy |
| `mandala-lookup` | skein-mandala 0.7.1 (copied) | `ls_mandala`, `ls_mandala_deploys`; its fn `tokens`, the token list, the read `/mandala/tokens` (0.6.0: the token list is a read of the components) |
| `amm-lookup` | `programs/amm-lookup` | `ls_amm`: the live pools that pass the pool checks, per token |
| `amm-validator` | `programs/amm-validator` | the validator's three direct calls, for every registered token when `config.overlay.validator` is set; submits by message to its own overlay |
| `amm-p2p` | `programs/amm-p2p` | the relay, the pages (`www/` from the app's tree); the catch-up utility, unscheduled |
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
| `GET /listTopicManagers`, `/listLookupServiceProviders`, `/getDocumentationFor…` | the listings (each program's `metadata` / `documentation`); reads since 0.5.0 |
| `GET /.live/tm_<txid>-live` | the runtime's liveness read (skein #138, no program): `[{sender, at, body, from}]` newest first, the beats within the window (`body` empty: the validator is `sender`, its peer ID `from`); 404 when the app keeps no liveness for the topic (not a market, or the topic not registered) |
| `GET /mandala/tokens` | mandala-lookup `tokens`: the token list, `{limit?, skip?}` → `[{tokenId, topic, sym, dec, icon?, txid, vout}]`; a read (0.6.0) |
| `POST /call` | amm-p2p: `{fn, args}` for the three interfaces below |
| `GET /…` (prefix `/`) | amm-p2p `serve`: the pages, `www/` of the app's own tree (skein-sdk `files.serve`); a read since 0.5.0 |

Which of these is a read and which a message route is "Rows and reads",
below. `GET /live` is gone (0.5.0; it answered 410 from 0.4.0).

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

`amm.*.submit` takes the funding transaction (the wallet's `noSend`
action) and the swap, deploy or add, and the validator the caller names —
`validator` (its identity key) and `peerId` (its libp2p peer ID, text, 0.4.0)
— checks the pair, records it under `amm/app` and relays it to that
validator: it dials `peerId` over libp2p, or, when `peerId` is its own
node's, calls its own `amm-validator` in-VM with the same package (a node
does not dial itself). It looks at no liveness. The record settles
`accepted`, `refused`, `timeout` or `failed` (amm-p2p README "The
marketplace relay").

**libp2p**: `/amm-validator/1/swap`, `/addLiquidity`, `/deploy`
(amm-validator; one signed-message package per frame); the engine's
`<topic>`, `-admit`, `-proof` for each registered topic; `tm_<txid>-live`,
beaconed by a validator's host and subscribed, without admitting anything,
by a market's liveness tool (below).

**Box `amm/amm-p2p`**: the cron provider's ticks, `{kind: "amm-p2p-tick",
job: "catchup"}` — a utility nothing schedules (0.6.0). amm-p2p takes no
start or stop: the manifest has none, and the owner's `{kind:
"amm-p2p-start" | "amm-p2p-stop"}` is refused (`BadMessage`).

**Market and validator** (0.6.0, shruggr/skein#120). David, 2026-10-06
evening: "a skein runs as a market and/or a validator by two settings in the engine's configuration (`config.overlay.market: {window}`, `config.overlay.validator: {every}`), and registering a token's topic is the one act that drives both". Two settings of the engine (skein-overlay 0.9.0), each
optional, the only role settings; this manifest sets both:

```json
"config": {"overlay": {"market": {"window": 40000}, "validator": {"every": 30000}}}
```

The engine, on `register {topic: "tm_<txid>", program: "mandala-topic"}`
(box `amm/register`, from the owner): subscribes the topic and seeds; with
`market`, emits `{event: "liveness", topic: "tm_<txid>-live", window}`; with
`validator`, `{event: "beacon", topic: "tm_<txid>-live", every, body:
<empty>}` — "the beat needs no body: the frame carries the sender's
identity key and the gossip message the peer id". `deregister` reverses
both (`unliveness`, `unbeacon`). A start or a re-read emits nothing: the
intents stand in the log.

- **The validator.** amm-validator signs swaps and takes on new liquidity
  — signs an LP's `addLiquidity`, consents to a pool `deploy` as the pool's
  validator (receiving liquidity, not the node putting up funds of its own)
  — for any token whose topic is in the engine's registered set (the head
  `amm/topics`) when `config.overlay.validator` is set; otherwise every
  `swap`, `addLiquidity` and `deploy` is refused `{ok: false, reason:
  "not_validating", detail}` before anything is checked or signed. It reads
  the setting and the set at each call. Each beat is the host's signed
  frame (docs/MESSAGES.md "Beacons"): dag-cbor `{body, at, sender,
  signature}`, `body` empty, `sender` the instance's identity key; the
  gossip message's peer is the node's peer ID, derived from the instance's
  root (`[2, "skein instance"]`, key ID `libp2p:<handle>`, counterparty
  self; skein signer.ts `peerKey`).
- **The market.** The runtime's liveness tool subscribes `tm_<txid>-live`
  without admitting its messages (no entry, nothing logged), verifies each
  beat's signature against `sender`, and keeps the beats newer than
  `window`, the latest per sender, with the node's own published beats, in
  memory; it serves them at `GET <base>/.live/tm_<txid>-live` → `[{sender,
  at, body: <base64>, from: <peer ID>}]` newest first (404 when no liveness
  is kept). The window is a margin over the beat (40 s against 30 s).
- **The page** reads that endpoint per token, takes each beat's `sender`
  (the validator) and `from` (its peer ID), shows the validators within the
  window, plans (`market/plan.ts`) only over the pools whose validator is
  there, and names the chosen one in the swap (`validator` and `peerId`).

Gone in 0.6.0: `validate` / `unvalidate` (the box `amm/validate` and its
row), amm-p2p's validated set (the record's `validated` under `amm/p2p`),
its `beacon` / `liveness` emits and their bookkeeping (the maps `beacons`,
`subscriptions`), its start / stop and the cron fallback's heartbeat, and
`config.amm.ammP2p` (`market`, `heartbeatSeconds`, `offlineSeconds`:
`validator.every` is the beat, `market.window` the offline threshold).
Per-token permissioning is a later refinement: "if we needed that kind of
split, we would split it across multiple skeins".

## The manifest

`etc/app.json`: the six programs; `config.overlay` with no topics and the
lookups `ls_mandala`, `ls_mandala_deploys` (both `mandala-lookup`) and
`ls_amm` (`amm-lookup`), none with a `topics` list, so each listens to every
topic served; gossip on (the default); `market: {window: 40000}` and
`validator: {every: 30000}` (0.6.0, above); `config.amm` (`ammValidator`,
`commission`); `provides` the three `amm.*` interfaces (each `submit` with
`peerId: "string"` beside `validator`, 0.4.0); `requires: ["chain/1"]`; no
`start` / `stop` (0.6.0).

### The rows

In table order; mailbox addresses are relative to the app (shruggr/skein#128:
`""` is the box `amm`, `"x"` is `amm/x`); the kernel takes the first row of
the package's transport and address whose sender rule admits the sender:

1. `register` from `$owner` → `overlay` (register / deregister; with them the roles' events)
2. `""` from `event` → `overlay`
3. `""` from `$self` → `overlay` (the engine's own watch, resume)
4. `""` from `*` → `amm-p2p` (the relay's interfaces)
5. `amm-p2p` from `*` → `amm-p2p` (the cron provider's ticks)
6. `submit` from `*` → `overlay`, `filter: "beef"` (submissions from anyone, by message and from `POST /submit`; skein-overlay 0.7.6)
7. http `/call` → `amm-p2p` `call` (a message route: a signed request)
8. libp2p `/amm-validator/1/swap`, `/addLiquidity`, `/deploy` →
   `amm-validator` (each refused `not_validating` without `config.overlay.validator`, or for a topic not registered)

(0.5.0 had a row `validate` from `$owner` → `amm-p2p` second; gone in 0.6.0.)

then, derived by the install from `config.overlay`: http `/submit` (`filter:
beef`) → `overlay`, and the read `/lookup` (below).

### Rows and reads

shruggr/skein#135 (two doors): a **row** (`dispatch[]`) carries a message —
an http row is a message route, which takes a signed request (BRC-104),
appends it as an entry and steps it, and answers an unsigned request 401;
a **read** (`reads[]`) is served by the host as a call of the function over
the current state, any method, signed or not, no entry, nothing logged (a
function that writes fails inside the call). A read and an http row never
share a path; exact paths match before a prefix, so `/call`, `/submit` and
`/lookup` are taken before the read `/`.

| path under `/amm/` | door | program, fn | since |
|---|---|---|---|
| `/submit` | row (http, derived from `config.overlay`) | `overlay` `submit`, `filter: beef` | |
| `/call` | row (http) | `amm-p2p` `call` | |
| `/lookup` | read (derived from `config.overlay`) | `overlay` `lookup` | skein #135 |
| `/listTopicManagers`, `/listLookupServiceProviders` | read | `overlay` `listTopicManagers`, `listLookupServiceProviders` | 0.5.0 (http rows before) |
| `/getDocumentationForTopicManager`, `/getDocumentationForLookupServiceProvider` | read | `overlay` `topicDocumentation`, `lookupDocumentation` | 0.5.0 (http rows before) |
| `/` (prefix) | read, `root: "www"`, `index: "index.html"` | `amm-p2p` `serve` | 0.5.0 (an http row before) |
| `/mandala/tokens` | read | `mandala-lookup` `tokens` | 0.6.0 |
| `/.live/tm_<txid>-live` | the runtime's liveness read (no program, skein #138) | | |
| `/live` | gone | | 0.5.0 (410 in 0.4.0) |

The boxes (`register`, `""`, `amm-p2p`, `submit`) and the libp2p
rows above are rows; they are messages, never reads. amm-p2p's `serve`
reads only (the head `amm/app` and the tree's blobs) and runs as a call
unchanged.

Rows 2 and 3 are the derived engine rows, listed so they come before row 4:
a `*` row admits events and the instance's own messages too, so after it
the engine would get neither.

## Not wired

- **The want-answer stream** `/skein/overlay/beef/1.0.0` (skein-overlay
  0.7.1+'s manifest row): not carried, as skein-mandala 0.6.0 does not; a
  submission paused on a parent resumes only when a later submission brings
  it.
- **`tm_<txid>-live` off a market host.** Liveness is kept by a host
  serving a market (`config.overlay.market`, above); on any other host its
  `.live` read answers 404 and its Swap page plans nothing.
- **Catch-up and proofs by block.** Never specified; sync is
  shruggr/skein#112's `want`. The `/amm/proofs/1.0.0` row is gone and no
  start schedules the catch-up pass; the code stays as a utility
  (`proofsByBlock`, `catchup`).
- **A setting changed after a register.** The roles' events are emitted at
  `register` / `deregister` only (the intents stand in the log): a market
  window or a beat changed by a reinstall applies to the topics registered
  after it; a topic registered before keeps its standing liveness and beacon
  (deregister and register it again). An instance upgraded from 0.5.0 keeps
  the beacons and liveness amm-p2p emitted then (keyed by app and topic, as
  the engine's are; a register of a topic registered already emits nothing,
  so deregister and register it to replace them). Their beats still carry
  0.5.0's body, which the page no longer reads.
- **Governance per token** (#120 item 4) and the other Mandala queries:
  skein-mandala docs/MANDALA.md "Not built".
- **Run end to end.** The programs are tested natively and the pages
  against fakes; this app has not been installed on a skein.
