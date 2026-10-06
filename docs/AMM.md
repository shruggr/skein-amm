# The AMM on a skein

A constant-product AMM between BSV and one Mandala token per pool, served by
a skein overlay app. Decided in shruggr/skein#120: Mandala is components (a
topic manager and a lookup service, shruggr/skein-mandala); the AMM is an
app that carries them with its own pool lookup, validator and relay; one
topic per token, registered by the owner at runtime.

## The pieces

| role | source | what it does |
|---|---|---|
| `overlay` | skein-overlay 0.7.4 (`bin/overlay.wasm`, copied) | a submission by message (or `/submit`, delivery only), answered to the submitter's box; `/lookup`, gossip, the listing routes; `register` / `deregister` a topic; hands every admitted BEEF to the chain app |
| `mandala-topic` | skein-mandala 0.5.0 (copied) | judges `tm_<txid>` by the BRC-162 rules; `tm_mandala_deploys` admits every deploy |
| `mandala-lookup` | skein-mandala 0.5.0 (copied) | `ls_mandala`, `ls_mandala_deploys` |
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
  {beef, topics: [tm_<txid>]}}` from the instance to itself, box `amm` —
  and answers the direct call on the engine's first answer (`admitted`,
  `rejected`).

## Interfaces

**Overlay** (base URL `<handle>.<host>/amm`):

| route | what |
|---|---|
| `POST /submit` | the submission message's transport (`X-Topics` a registered topic): `200 {id}`, delivery only; no STEAK (skein-overlay 0.7.2+) |
| `POST /lookup` | BRC-24: `ls_amm` `{tokenId}`, `{tokenId, outpoint, beef?}`, `{tokenId, validatorIdentityKey}`; `ls_mandala`, `ls_mandala_deploys` (skein-mandala README) |
| `GET /listTopicManagers`, `/listLookupServiceProviders`, `/getDocumentationFor…` | the listings (each program's `metadata` / `documentation`) |
| `GET /live` | amm-p2p: the validators heard from |
| `POST /call` | amm-p2p: `{fn, args}` for the three interfaces below |
| `GET /…` (prefix `/`) | amm-p2p `serve`: the pages, `www/` of the app's own tree (skein-sdk `files.serve`) |

**Box `amm/overlay`** (the manifest's `"overlay"`, relative to the app,
shruggr/skein#128): the engine's `register {topic, program}`, `deregister
{topic}`, from the owner.

**Box `amm`** (a message `{fn, args}`, skein docs/APPS.md §4):

| interface | functions | from |
|---|---|---|
| `amm.swap/1` | `submit` (writes), `status`, `terms` | anyone (the owner too) |
| `amm.pool/1` | `submit` (writes), `status` | anyone |
| `amm.liquidity/1` | `submit` (writes), `status` | anyone |
| the engine's | `submit {beef, topics}` (and its own `watch`, `resume`) | the instance itself (the validator) |
| amm-p2p's | `{kind: "amm-p2p-start" \| "amm-p2p-stop"}` (the manifest's `start` / `stop`) | the owner |

`amm.*.submit` takes the funding transaction (the wallet's `noSend`
action) and the swap, deploy or add, checks the pair, records it under
`amm/app` and relays it to the pool's validator over libp2p; the record
settles `accepted`, `refused`, `timeout` or `failed` (amm-p2p README "The
marketplace relay").

**libp2p**: `/amm-validator/1/swap`, `/addLiquidity`, `/deploy`
(amm-validator; one signed-message package per frame); the engine's
`<topic>`, `-admit`, `-proof` for each registered topic; `tm_<txid>-live`,
published by the host's beacon (below), never subscribed by the node.

**Box `amm/amm-p2p`**: the owner's `{kind: "amm-p2p-start" | "amm-p2p-stop"}`
(the Validator page), the cron provider's ticks (the fallback, `jobs`), and
admitted heartbeats (none are, below).

**The beacon** (shruggr/skein#126): on start amm-p2p emits, per served token
topic, `{event: "beacon", topic: "tm_<txid>-live", every: heartbeatSeconds ×
1000, body}` — the host's libp2p node publishes `body` there every `every`
ms, logging nothing per beat — and `unbeacon {topic}` for a topic it
beaconed that is no longer served; on stop, `unbeacon` for each (the set
under `amm/p2p`, map `beacons`). `body` is amm-poc#3's signed heartbeat
`{identityKey, peerId, sig, at}`. A start that names `jobs` asks the cron
provider instead (the 0.1.0 path, kept as the fallback). The peer ID is
derived: the signer's public key for `[2, "skein instance"]`, key ID
`libp2p:<handle>`, counterparty self, as an identity multihash.

## The manifest

`etc/app.json`: the six programs; `config.overlay` with no topics and the
lookups `ls_mandala`, `ls_mandala_deploys` (both `mandala-lookup`) and
`ls_amm` (`amm-lookup`), none with a `topics` list, so each listens to every
topic served; gossip on (the default); `config.amm` (`ammP2p`:
`heartbeatSeconds`, `offlineSeconds`; `ammValidator`, `commission`);
`provides` the three `amm.*` interfaces; `requires: ["chain/1"]`; `start` /
`stop`.

### The rows

In table order; mailbox addresses are relative to the app (shruggr/skein#128:
`""` is the box `amm`, `"x"` is `amm/x`); the kernel takes the first row of
the package's transport and address whose sender rule admits the sender:

1. `overlay` from `$owner` → `overlay` (register / deregister)
2. `""` from `event` → `overlay`
3. `""` from `$self` → `overlay` (the validator's submissions; the engine's own watch, resume)
4. `""` from `*` → `amm-p2p` (the relay's interfaces; the owner's start / stop)
5. `amm-p2p` from `*` → `amm-p2p`
6. http `/listTopicManagers`, `/listLookupServiceProviders`,
   `/getDocumentationForTopicManager`, `/getDocumentationForLookupServiceProvider`
   → `overlay`
7. http `/live` → `amm-p2p` `live`; `/call` → `amm-p2p` `call`
8. http `/` (prefix) → `amm-p2p` `serve`, `root: "www"`, `index: "index.html"`
9. libp2p `/amm-validator/1/swap`, `/addLiquidity`, `/deploy` →
   `amm-validator`

then, derived by the install from `config.overlay`: http `/submit` (`filter:
beef`) and `/lookup` → `overlay` (exact paths match before the `/` prefix).

Rows 2 and 3 are the derived engine rows, listed so they come before row 4:
a `*` row admits events and the instance's own messages too, so after it
the engine would get neither.

## Not wired

- **The engine's submit row from anyone.** skein-overlay 0.7.2+ asks for
  `{"address": "", "sender": "*", "program": "overlay", "filter": "beef"}`
  (a submission by message from anyone, into the app's box). Row 4 has that
  key (transport, address, prefix, sender) for amm-p2p, and only one row
  may. The engine takes `submit` in any box routed to it, but a `*` row on
  another box (`overlay`) opens `register` / `deregister` there to anyone
  too (skein-overlay src/topics.zig `mayRegister`: only the app's own box
  limits them to the instance). Not decided: the manifest has neither. What
  submits still works: `POST /submit` (delivery only), gossip, and the
  validator's own submissions (row 3).
- **The want-answer stream** `/skein/overlay/beef/1.0.0` (skein-overlay
  0.7.1+'s manifest row): not carried, as skein-mandala 0.5.0 does not; a
  submission paused on a parent resumes only when a later submission brings
  it.
- **The beacon's freshness.** The host re-sends the same body every beat,
  so its `at` and signature are the start's; a receiver's `liveness.judge`
  ignores a body older than the offline threshold. How a beat carries
  freshness is not decided.
- **The peer ID.** The host derives the node's key from the router's master
  secret (skein src/host/signer.ts `peerKey`), not from the instance's root
  key the signer holds, so the derived peer ID is not the node's yet.
- **`tm_<txid>-live` subscription.** The skein node never subscribes `-live`
  (matchmaking is the client's): `validateLive` and the last-seen map stay,
  routed by nothing, so `/live` lists no one and the relay finds no
  validator's peer from it.
- **Catch-up and proofs by block.** Never specified; sync is
  shruggr/skein#112's `want`. The `/amm/proofs/1.0.0` row is gone and no
  start schedules the catch-up pass; the code stays as a utility
  (`proofsByBlock`, `catchup`).
- **The beacon on register / deregister.** amm-p2p is not told of the
  engine's registrations: a topic registered after the start is beaconed at
  the next start; one deregistered is unbeaconed at the next start or stop.
- **Governance per token** (#120 item 4) and the other Mandala queries:
  skein-mandala docs/MANDALA.md "Not built".
- **Run end to end.** The programs are tested natively and the pages
  against fakes; this app has not been installed on a skein.
