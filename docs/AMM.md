# The AMM on a skein

A constant-product AMM between BSV and one Mandala token per pool, served by
a skein overlay app. Decided in shruggr/skein#120: Mandala is components (a
topic manager and a lookup service, shruggr/skein-mandala); the AMM is an
app that carries them with its own pool lookup, validator and relay; one
topic per token, registered by the owner at runtime.

## The pieces

| role | source | what it does |
|---|---|---|
| `overlay` | skein-overlay 0.6.0 (`bin/overlay.wasm`, copied) | `/submit`, `/lookup`, gossip, the listing routes; `register` / `deregister` a topic; hands every admitted BEEF to the chain app |
| `mandala-topic` | skein-mandala 0.4.0 (copied) | judges `tm_<txid>` by the BRC-162 rules; `tm_mandala_deploys` admits every deploy |
| `mandala-lookup` | skein-mandala 0.4.0 (copied) | `ls_mandala`, `ls_mandala_deploys` |
| `amm-lookup` | `programs/amm-lookup` | `ls_amm`: the live pools that pass the pool checks, per token |
| `amm-validator` | `programs/amm-validator` | the validator's three direct calls |
| `amm-p2p` | `programs/amm-p2p` | liveness, proofs by block, the relay |
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
  the transaction to its own overlay.

## Interfaces

**Overlay** (base URL `<handle>.<host>/amm`):

| route | what |
|---|---|
| `POST /submit` | BRC-22, `X-Topics` a registered topic |
| `POST /lookup` | BRC-24: `ls_amm` `{tokenId}`, `{tokenId, outpoint, beef?}`, `{tokenId, validatorIdentityKey}`; `ls_mandala`, `ls_mandala_deploys` (skein-mandala README) |
| `GET /listTopicManagers`, `/listLookupServiceProviders`, `/getDocumentationFor…` | the listings (each program's `metadata` / `documentation`) |
| `GET /live` | amm-p2p: the validators heard from |
| `POST /call` | amm-p2p: `{fn, args}` for the three interfaces below |

**Box `amm`** (a message `{fn, args}`, skein docs/APPS.md §4):

| interface | functions | from |
|---|---|---|
| the engine's | `register {topic, program}`, `deregister {topic}` | the owner |
| `amm.swap/1` | `submit` (writes), `status`, `terms` | anyone else |
| `amm.pool/1` | `submit` (writes), `status` | anyone else |
| `amm.liquidity/1` | `submit` (writes), `status` | anyone else |

`amm.*.submit` takes the funding transaction (the wallet's `noSend`
action) and the swap, deploy or add, checks the pair, records it under
`amm/app` and relays it to the pool's validator over libp2p; the record
settles `accepted`, `refused`, `timeout` or `failed` (amm-p2p README "The
marketplace relay").

**libp2p**: `/amm-validator/1/swap`, `/addLiquidity`, `/deploy`
(amm-validator; one signed-message package per frame); `/amm/proofs/1.0.0`
(amm-p2p); the engine's `<topic>`, `-admit`, `-proof` for each registered
topic; `tm_<txid>-live` (heartbeats, not routed: below).

**Box `amm-p2p`**: the owner's `{kind: "amm-p2p-start" | "amm-p2p-stop"}`,
the cron provider's ticks, admitted heartbeats.

## The manifest

`etc/app.json`: the six programs; `config.overlay` with no topics and the
lookups `ls_mandala`, `ls_mandala_deploys` (both `mandala-lookup`) and
`ls_amm` (`amm-lookup`), none with a `topics` list, so each listens to every
topic served; gossip on (the default); `config.amm` (`ammP2p`,
`ammValidator`, `commission`); `provides` the three `amm.*` interfaces;
`requires: ["chain/1"]`; `start` / `stop`.

### The rows

In table order; the kernel takes the first row of the package's transport
and address whose sender rule admits the sender:

1. `amm` from `$owner` → `overlay` (register / deregister)
2. `amm` from `event` → `overlay`
3. `amm` from `$self` → `overlay`
4. `amm` from `*` → `amm-p2p` (the relay's interfaces)
5. `amm-p2p` from `*` → `amm-p2p`
6. http `/listTopicManagers`, `/listLookupServiceProviders`,
   `/getDocumentationForTopicManager`, `/getDocumentationForLookupServiceProvider`
   → `overlay`
7. http `/live` → `amm-p2p` `live`; `/call` → `amm-p2p` `call`
8. libp2p `/amm/proofs/1.0.0` → `amm-p2p` `proofsByBlock`
9. libp2p `/amm-validator/1/swap`, `/addLiquidity`, `/deploy` →
   `amm-validator`

then, derived by the install from `config.overlay`: http `/submit` (`filter:
beef`) and `/lookup` → `overlay`.

Rows 2 and 3 are the derived engine rows, listed so they come before row 4:
a `*` row admits events and the instance's own messages too, so after it
the engine would get neither (the gossip it admits, its own watch).

Row 1 before row 4 means every message from the owner in box `amm` reaches
the engine, which takes `register` / `deregister` and refuses anything
else from a sender that is not the instance (`NotFromThisInstance`). So the
manifest's `start` / `stop` (sent into box `amm`) do not reach amm-p2p, and
neither do the owner's own `amm.*` messages; the owner starts amm-p2p in box
`amm-p2p` (row 5).

## Not built

- **Serving `www/`.** skein-static 0.2.1 serves files from the `main`
  head's tree only (its src/main.zig: `sk.head(a, "main")`), not from an
  app's own tree, and no handler does. The manifest has no static rows.
- **Validator liveness per token.** The heartbeat is published on
  `tm_<txid>-live` for each served token topic and `validateLive` judges
  it, but nothing routes `tm_<txid>-live` to it: amm-poc had one libp2p row
  per token (`tm_{{TXID}}-live`), templated per instance, and the engine's
  `register` subscribes only `<topic>`, `-admit` and `-proof`. Not decided.
- **The peer ID.** A program cannot learn its instance's libp2p peer ID
  (the step's input has `self.identity` only); amm-poc templated
  `ammP2p.peerId`. Without it, `amm-p2p-start` schedules no heartbeat.
- **Recording a fetched proof.** amm-p2p's catch-up applied each BUMP it
  fetched to the chain core under the head `wallet`. The chain state is the
  chain app's alone, so the pass now errors (`CatchupCannotRecordProofs`)
  when a reply proves a held unproven transaction.
- **Starting amm-p2p by the manifest's `start`** (The rows, above).
- **Governance per token** (#120 item 4) and the other Mandala queries:
  skein-mandala docs/MANDALA.md "Not built".
- **Run end to end.** The programs are tested natively and the pages
  against fakes; this app has not been installed on a skein. amm-poc's runs
  (its deploy/README.md) were on skein-overlay 0.2.0.
