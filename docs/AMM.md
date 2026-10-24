# The AMM on a skein

A constant-product AMM between BSV and one 1Color token per pool, served by
a skein overlay app. Decided in shruggr/skein#120: 1Color is components (a
topic manager and a lookup service, shruggr/skein-1color); the AMM is an
app that carries them with its own pool lookup, validator and relay; one
topic per token, registered by root at runtime.

## The pieces

| role | source | what it does |
|---|---|---|
| `overlay` | skein-overlay 0.16.0 (`bin/overlay.wasm`, copied; 0.16.0: the beat's `terms` from the head `amm/terms`) | a submission by message in box `amm/submit`, answered to the submitter's box, or by `POST /submit` (BRC-22, the STEAK); `/lookup`, gossip, the listing and documentation reads; `register` / `deregister` a topic (and `registerLookup` / `deregisterLookup` its lookup), and with it the market's liveness and the validator's beacon on `tm_color_<txid>_0-live` (below, "Market and validator"), and each registered lookup's beacon and liveness on `<service>-live`; hands every admitted BEEF to the chain app |
| `color-topic` | skein-1color 0.10.0 (copied) | judges `tm_color_<txid>_0` (`tm_color_<assetId>`, `_<vout>` always) by the BRC-162 rules; `tm_color` admits every deploy |
| `color-lookup` | skein-1color 0.10.0 (copied) | `ls_color`, `ls_color_deploys`, each token's `ls_color_<assetId>` (BRC-207: `color-spendability`, `color-admission`), one index; its fn `tokens`, the token list, the read route `/color/tokens` (0.6.0: the token list is a read of the components) |
| `amm-lookup` | `programs/amm-lookup` | `ls_amm`: the listed pools (below, "The listing"), per token; its beat, the prices ("Prices") |
| `amm-validator` | `programs/amm-validator` | the validator, for every registered token (always, 0.8.1): `swap` (a libp2p direct call; it submits by message to its own overlay), `affirm` (0.11.0; 0.13.0: it picks the nonce; called in-VM by `amm.pool.affirm`), root's `rescind` (0.13.0; 0.11.0–0.12.x: `close`) |
| `amm-p2p` | `programs/amm-p2p` | the relay (`amm.swap/1`; `amm.pool/1`: `terms`, `affirm`), the holders' listing requests, the reads `/requests` and `/spends`, the pages (`www/` from the app's tree); the catch-up utility, unscheduled |
| | `src/pool.zig` | the pool library (module `pool`), over `1color`'s parser and rules |
| | `pool/Pool.runar.go` | the contract |
| | `web/ui` → `www/` | the AMM pages; `www/color/` the 1Color pages |

The topic knows no application: a pool output is a token output like any
other and is admitted exactly when the BRC-162 rules admit it. Which of
those outputs are pools is the lookup's judgement, and whether one may be
spent is the validator's.

## The pool rule

A pool is a BRC-162 value output at output 0 whose lock is the compiled
Pool code, `OP_RETURN`, and the state (0.13.0, layout 2; David Case
2026-10-22, agent-env plans/validator-signoff-2026-10-22.md §9):

| bytes | state field |
|---|---|
| 8 | TokenReserve (little-endian sign-magnitude, Rúnar's bigint) |
| 33 | LpPubKey |
| 33 | ValidatorPubKey (33 zero bytes once rescinded) |
| 33 | ValidatorIdentity |
| 32 | Nonce (Rúnar's fixed 32-byte type, raw) |
| 1 + n | Affirmation: one length byte, then the validator's DER signature (n ≤ 72; `OP_0` when empty) |

The BSV reserve is the output's satoshis; the token reserve is the prefix's
amount, duplicated in state. There is no payload (0.11.0–0.12.x carried the
affirmation as the deploy's BRC-162 payload). Nonce and Affirmation are
never assigned, so Rúnar carries them into every continuation: every pool
output attests itself (below, "The deploy and the affirmation"). The
compiled template is 3,568 bytes (0.12.x: 2,657), a pool output 4,011 bytes
with its args and a 71-byte DER (0.12.x: 2,996): the variable-length
Affirmation costs each method a state parser, and Rescind its own copy of
the continuation writer. A pool deployed before 0.13.0 (layout 1: the first
four state fields, `Close(sig, bsvFee)`) is still decoded, closed and
recovered by the pages; amm-lookup and amm-validator know layout 2 only
(below, "Not wired").

- **Indexed** (amm-lookup, `pool.check`): at output 0; the prefix's id and
  amount equal the AssetId and TokenReserve the code and state carry (the
  contract writes the prefix from its copies and never reads it). The id is
  32 bytes, so only a token deployed at output 0 has pools.
- **Methods** (the contract; 0.13.0, David Case 2026-10-22, three):
  `Swap(validatorSig, nextValidatorPubKey, amountIn, bsvIn, userPkh,
  commissionPkh)`, the validator's, unchanged; `Close(lpSig)`, the LP's
  alone: a plain `OP_CHECKSIG` against the LP key, the **outputs free** (no
  output covenant: the LP spends the LP's own funds, its wallet decides
  where they go; the method writes no state and adds no output, so Rúnar
  checks no hashOutputs and takes no change parameters); `Rescind(
  validatorSig)`, the validator's: `OP_CHECKSIG` against the current
  ValidatorPubKey, exactly one continuation at output 0 — the same sats,
  TokenReserve, LpPubKey, ValidatorIdentity, Nonce and Affirmation — with
  ValidatorPubKey 33 zero bytes, then Rúnar's change output (the validator
  pays the fee from an input of its own; nothing is taken from the pool).
  After a Rescind no Swap can be signed (no key matches the zero key) and no
  second Rescind; the LP closes the pool as any. Fees are fixed at deploy,
  in basis points of the input, rounded up, paid in the input asset:
  LpFeeBps to the LP, ValidatorFeeBps to the validator, CommissionBps to
  whoever relays the swap — the validator's terms. A swap carries only the
  contract's outputs. (0.11.0–0.12.x: `Close(sig, bsvFee)`, the LP's or
  the validator's, everything to the LP key.)
- **Keys rotate.** The validator's key is the BRC-42 child of its
  ValidatorIdentity, protocol `[1, "amm pool"]`, counterparty anyone: a
  pool's first key (its deploy's) keyed by the Nonce, as 64 lowercase hex
  digits; each Swap's next key keyed by the pool outpoint that Swap spends,
  `<txid>_<vout>` (0.9.0's convention, kept). The LP's key is the pool's
  for its life. amm-lookup checks both derivations (below, "The listing");
  the validator derives its key before signing (`currentKeyId`).
- **The validator signs last** (amm-validator README "What is checked"):
  the pool is ours, live and not rescinded, the call is the method's, the
  next key is our child of the spent outpoint, the topic would admit the
  transaction, the pool checks pass, the outputs are the contract's, every
  other input (and every unproven parent) is signed. Then it signs the pool
  input through the signer and submits the transaction to its own overlay
  by message — `{fn: "submit", args: {beef, topics: [tm_color_<txid>_0]}}`
  from the instance to itself, box `amm/submit` — and answers the direct
  call on the engine's first answer (`admitted`, `rejected`).

## The deploy and the affirmation (0.13.0)

David Case, 2026-10-09: "Fees are the validator's"; 2026-10-21: pool
deploys move to the BRC-207 wallet module; 2026-10-22: the validator's
nonce, the attestation in state, no input named by the page.

- **The terms** (0.15.0, shruggr/skein-amm#2; David Case 2026-10-23:
  "The configs should not be published in the image. This will be a
  dynamic, competitive marketplace."). The validator's fees are a runtime
  record root sets, skein-wide: root's message `{fn: "terms", args:
  {lpFeeBps, validatorFeeBps, commissionBps}}` (each an integer, 0 to
  10 000) in the box `amm/validator` sets them, `{fn: "terms", args:
  null}` clears them, gated by `roles: {root: [..., "terms"]}`.
  amm-validator keeps them as the root of the head `amm/terms` (the record
  `{lpFeeBps, validatorFeeBps, commissionBps}`, or `null` once cleared;
  src/terms.zig), answers root `{fn: "terms", request, result}` (or
  `error: {code: "bad-args", message}`, nothing written), and sends the
  instance itself `{fn: "rebeat"}` in box `amm`: the engine (skein-overlay
  0.16.0) re-declares every registered topic's beat, whose `terms` is the
  record. Terms set: this skein is a validator. Unset: a market only —
  `affirm` and the swap signing are refused `not_validating`, "no terms:
  this skein validates nothing", `amm.pool.terms` answers "no validator
  here", and the beat carries no `terms`. No fee numbers ship in the
  manifest or the image (`config.amm.ammValidator`, 0.9.0 to 0.14.2, is
  gone, with no fallback). An LP posting here accepts the terms: the page
  has no fee inputs and reads them with `amm.pool.terms`. The three fees
  are the terms (no termsHash).
- **The affirmation.** Before the LP's wallet builds the deploy, the page
  asks this skein's validator, `amm.pool.affirm {pool, satoshis}` (below,
  "Interfaces"): `pool` the pool output's locking script as the deploy will
  carry it, its ValidatorPubKey, Nonce and Affirmation still to come (the
  page sends 33 zero bytes, 32 zero bytes and an empty Affirmation; the
  validator ignores those three), `satoshis` its BSV reserve. amm-validator
  checks it — a pool (the Pool code; prefix id and amount the asset id and
  TokenReserve the code and state carry; no payload), its
  ValidatorIdentity ours, its token's topic registered, its fees our terms
  exactly (`fees_unacceptable`, the field and the term in `detail`), its
  reserves positive — then picks a random 32-byte nonce, derives its key
  from it, has the signer agree (`wrong_validator_key`), and signs the
  message below. It answers `{validatorPubKey, nonce, signature}`. Nothing
  is recorded or submitted.
- **The message v2** (src/pool.zig `affirmMessage`, web/ui
  `PoolTemplate.affirmationMessage`, gen/vectors `affirmMessage`; 221
  bytes):

  | bytes | field |
  |---|---|
  | 18 | `"skein-amm affirm 2"`, ASCII: the tag (a domain separator, the layout's version) |
  | 32 | AssetId: the bytes the pool script pushes (the token's deploy txid, internal byte order) |
  | 8 × 3 | LpFeeBps, ValidatorFeeBps, CommissionBps: int64, little-endian |
  | 8 | TokenReserve: uint64, little-endian |
  | 8 | the pool output's satoshis (the BSV reserve): uint64, little-endian |
  | 33 × 3 | LpPubKey, ValidatorPubKey, ValidatorIdentity: compressed |
  | 32 | Nonce |

  The signature is ECDSA by the ValidatorPubKey over sha256(message), DER
  (the signer signs that digest as is: BRC-100 `createSignature`
  `hashToDirectlySign`; `@bsv/sdk`'s `PublicKey.verify(message, sig)`
  hashes the message itself). ValidatorPubKey = the anyone-child of
  ValidatorIdentity, `[1, "amm pool"]`, key ID the nonce's 64 hex digits.
- **The deploy.** The page checks the answer (the key is the validator's
  child of the nonce; the pool with the three in its state attests itself)
  and writes ValidatorPubKey, Nonce and Affirmation into the pool's state.
  The LP's wallet builds, signs and broadcasts the deploy as one
  createAction naming **no input**: outputs [the pool], the `p color plan`
  a transfer with no `inputPositions` and no owned outputs; the BRC-207
  module selects the token inputs and adds the token change, and supplies
  the input BEEF of its own selected inputs (agent-env
  plans/validator-signoff-2026-10-22.md §10: `@bopen-io/color-permission-module`
  0.1.1, the module paging its asset basket; not the toolbox). The module
  submits to the token topic `tm_color_<assetId>`; the page submits it too.
  The validator's part at deploy time ends with the affirmation. The pages'
  side is below, "The pages and the wallet's module".

## The pages and the wallet's module (0.12.0; 0.13.0)

David Case, 2026-10-21/22; 0.13.0, 2026-10-22 (deploy, Close, Recover). The pages (`web/ui`, the Open Exchange) hold the
user's tokens in the wallet's BRC-207 `color` module (`@bsv/color-permission-module`;
Yours, branch `1color`, registers it, its overlay on testnet this skein,
`https://swap.skein.nexus`). The module's names are the page's
(`src/wallet/color.ts`): the asset id `<txid>_<vout>`, its tag
`<txid>v<vout>`; the reads `p color assets` and `p color balance <tag>`; a
send is a `createAction` with a `p color plan` (`ColorPlan.toLabels`) and
`randomizeOutputs: false`, the module selecting token inputs, adding token
change, asking the user, signing and submitting; a receive is an output
locked to a module receiving key, `[2, "p color <tag> <prefixHex>
<suffixHex>"]`, keyID `color`, counterparty the sender, taken in with
`internalizeAction` (`basket insertion` into `p color <tag>`, the receipt
`{version: 1, assetId, derivationPrefix, derivationSuffix,
senderIdentityKey}`) once the overlay admits it. The exchange's own
baskets: `amm-pools` (the LP's position rows) and `amm-swap` (the active
swap basket, keys `[2, "amm swap"]`, counterparty self, a fresh keyID per
output; customInstructions `{assetId, amount, protocolID, keyID,
counterparty}`).

- **Your tokens.** The assets `p color assets` lists, each balance `p color
  balance <tag>` (an exact decimal string); symbol, decimals and icon from
  the token list (`/color/tokens`), else `ls_color_deploys {tokenId}`, else
  a deploy the wallet holds. Beside it, what `amm-swap` holds, with Move
  back.
- **Deploy a position** (0.13.0). The LP key: a BRC-29 key to self under a
  fresh random keyID (derivationPrefix base64("amm-lp"), derivationSuffix
  16 random bytes), kept in this browser (localStorage,
  `PendingDeployStore`) until the row is filed. `amm.pool.terms`;
  `amm.pool.affirm {pool, satoshis}` → `{validatorPubKey, nonce,
  signature}`, checked and written into the pool's state; one
  `createAction`: outputs [the pool] (a transfer to a foreign script), no
  inputs, no `inputPositions`, `randomizeOutputs: false`; the module selects
  the token inputs and adds the change. The wallet funds the sats (one spend
  prompt, one module prompt). The page submits the deploy to
  `tm_color_<assetId>` too, then files output 0 in `amm-pools`
  (customInstructions `{id, op: "amm-pool", sym, dec, protocolID, keyID,
  counterparty, amm: {role, validatorIdentity, lpFeeBps, validatorFeeBps,
  commissionBps, nonce}}`). (0.12.x: the page named a key input, input 0,
  and keyed the LP key by it.)
- **Swap, sats in.** As before (`1sat-deposit` funding, the leg built here,
  the relay). The token payout is locked to a module receiving key whose
  sender is the pool's ValidatorIdentity; accepted, the page takes it in
  through the module with its receipt.
- **Swap, tokens in.** Each leg spends `amm-swap` outputs carrying exactly
  its amount in (the contract takes no token change). The legs those do
  not cover get one output each, of exactly their amount, from ONE module
  send before the legs (a transfer out to `amm swap` keys; one module
  prompt; the page submits it to the token topic too and files the outputs
  in `amm-swap`). The legs are built and signed as before
  (`createSignature` under each output's key), relayed, and their inputs
  relinquished from `amm-swap` once accepted; an output a failed leg did
  not spend stays there for a later swap.
- **Recover a position** (0.12.1, `src/lp/recover.ts`). A deploy whose
  `amm-pools` row was never filed (the deploy broadcast, the filing did
  not happen) is not in the positions list, so it cannot be closed there.
  Liquidity's Recover takes the pool's outpoint (`<txid>.<vout>` or
  `<txid>_<vout>`, the deploy's or a later continuation), follows it to its
  current output (`GET <base>/spends`), reads that output's BEEF from
  `ls_color {txid, outputIndex}` and decodes the pool. A pool before
  0.13.0: walks the BEEF back to the deploy (the transaction whose pool
  output carries the affirmation as its payload) and derives the LP key
  from the deploy's input 0 (`lpKeyId`, else the pre-BRC-29
  `legacyLpKeyId`). A pool from 0.13.0 on: its LP keyID is random and on no
  chain, so the keyIDs this browser kept for deploys whose row was not
  filed are tried; none: "not kept in this browser", nothing filed. Each
  candidate is checked with `getPublicKey` against the pool's LpPubKey. On a match it files the
  current output in `amm-pools` with `internalizeAction` (basket
  insertion, tag `amm-pool`, the deploy's `poolCustomInstructions`, label
  `amm-pool-deploy`); otherwise it files nothing ("This pool's LP key is
  not this wallet's."). A pool the wallet already files is left alone.
  The row is then an ordinary position: Close as below.
- **Close** (0.13.0: `Close(lpSig)`, outputs free). The page's own Close
  (`bsvFee`: the miner fee left from the pool, or 0 for a funding output the
  wallet broadcasts): the sats less `bsvFee` to the LP key, a BRC-29 wallet
  payment to self; every token, one sat, to a NEW module receiving key of
  the user's own (`newReceivingKey`, sender the user's identity), taken in
  through the module with its receipt (a second `internalizeAction`; kept in
  pending payouts if the module does not take it at once). No `amm-swap`,
  no Move back. A pool before 0.13.0 closes as 0.12.x did (the contract's
  outputs, the tokens to the LP key, filed in `amm-swap`, Move back). A
  rescinded pool is shown as such and closes as any.
- **Move back** (Your tokens). Every `amm-swap` output of the token, and a
  funding output the wallet broadcasts for the fee, spent by an app-built
  transaction to one module receiving key of the user's own (sender: the
  user's identity), submitted to `tm_color_<assetId>`, taken in through the
  module, the spent outputs relinquished. A swap that got stuck, and a
  pre-0.13.0 position's closed tokens, come back this way.
- **Pending payouts** (`wallet/pendingPayouts.ts`) keep both kinds: a
  BRC-29 remittance or a module receipt, retried with the final
  transaction's BEEF.
- **Manifest** (`www/manifest.json`, `web/ui/public/manifest.json`):
  baskets `amm-swap`, `amm-pools`, `1sat-deposit`; protocols BRC-29 to self,
  `[2, "amm swap"]` to self, identity, AuthFetch's two, the action labels;
  no spending authorization; no `p color` entry (the module asks for those).
  0.13.0 adds nothing (the deploy, Close and Recover use the labels
  `amm-pool-deploy` and `amm-close` as before).

## The listing (0.13.0)

`ls_amm` (programs/amm-lookup `listedAt`) lists a pool output when

- **(a) it attests itself** (src/pool.zig `affirmed`): not rescinded; its
  ValidatorPubKey equals the ValidatorIdentity's child keyed by its Nonce,
  derived by the lookup (not trusted); and its Affirmation verifies over
  message v2 under that key — a deploy; or
- **(b) it is a Swap's continuation of a listed pool** (`nextKeyOf`): the
  transaction spends a pool of this token in `pools` (`spent` runs after
  `admitted`, so it is still there) with an unlocking script whose last
  push selects Swap (`methodOf`), the output is at 0, not rescinded, carries
  the same ValidatorIdentity, and its ValidatorPubKey is the identity's
  child keyed by that spent outpoint (David Case 2026-10-22: the lookup
  checks the next key).

Nothing else is listed: a rescinded pool (ValidatorPubKey 33 zero bytes:
Rescind's continuation), a pool-shaped output a Close wrote (Close's
outputs are free; it is no Swap), a continuation naming another next key,
a deploy under a key of the LP's own (v0.11.0's defect: its signature
verified but the key was not derived). Only listed pools are indexed, and
every query answers listed pools only. A listed pool leaves the listing
when its output is spent with no listed continuation: the LP's Close, or
the validator's Rescind.

**The validator's Rescind** (0.13.0; it replaces 0.11.0's validator Close,
which replaced 0.9.0's rescind of the claim): root's message `{fn:
"rescind", args: {pool: "<txid>.<vout>"}}` in the box `amm/validator`,
gated by `roles: {root: ["rescind"]}`. amm-validator follows `pool` (any of
the pool's outputs, its deploy's among them) to its newest output (refused
when it is closed, or rescinded already) and has the instance's wallet
build the Rescind with the pool as a caller input (shruggr/skein#93):
`createAction` with the pool (`unlockingScriptLength` the longest the call
can be, its transaction's BEEF), the contract's one output (the pool's
sats, its script with the ValidatorPubKey zeroed), label `amm-rescind`,
`noSend` — the wallet adds its funding input and its change after it — a
draft; it writes the pool input's Rúnar call of Rescind over the draft
(`validatorSig` the pool's current validator key's, through the signer,
over the draft's BIP-143 preimage; the wallet's change as Rúnar's
`_changePKH` / `_changeAmount`); `signAction {reference, spends}`; then
submits it to its overlay for the token topic (the topic admits the
rescinded pool; `ls_amm` drops it) and answers root `{fn: "rescind",
request, result | error}`. Each wallet call is a thread of the genesis
wallet program (amm-validator `rescind.zig`).

**The LP's view** comes from the chain state by outpoint, not the listing:
the read `GET <base>/spends?outpoint=<txid>.<vout>` (amm-p2p, the chain
app's `spent` as the overlay reads it) follows the pool from its deploy to
its current output (each spender's output 0 while it is a pool — a
Rescind's included; `closed` once a Close spent it). The current output's BEEF comes from `ls_color
{txid, outputIndex}`.

## Prices (0.9.0)

David Case, 2026-10-09: "Pricing lives in the AMM LOOKUP's own beacon";
"beats are libp2p service discovery". `ls_amm`, registered (skein-overlay
0.12: a registered lookup beats on `<service>-live` and keeps liveness on
it), beats on `ls_amm-live` a body (dag-cbor) carrying, per token, this
skein's per-validator totals of its listed pools:

```
{tokens: {"<assetId>": {"<validator identity, hex>": {sats, tokens, pools}}}}
```

re-declared by a hook whenever it changes (the lookup keeps the last body's
record, `beat`; `Service.beat`) and answered by fn `beat` at the beacon's
declaration. The page reads `GET <base>/.live/ls_amm-live` (each live
skein's latest beat) and combines them: a token's price is Σsats / Σtokens
across the live validators (a validator is live when its own beat is held,
on `ls_amm-live` or the token's topic); one report per validator (its own
beat's, else the newest naming it). Polled every `REFRESH_MS`. A token no
beat reports is priced from this skein's own listing and liveness. The
topic manager's beat carries no prices.

## Interfaces

**Overlay** (base URL `<handle>.<host>/amm`):

| route | what |
|---|---|
| `POST /submit` | BRC-22 (`X-Topics` a registered topic): the request waits on the submission and answers the STEAK; 400 for a BEEF that does not verify or a rejected transaction; 503 + Retry-After while undecided (skein-overlay 0.9.1; 0.7.3–0.9.0 answered `{id}`) |
| `POST /lookup` | BRC-24: `ls_amm` `{tokenId}`, `{tokenId, outpoint, beef?}`, `{tokenId, validatorIdentityKey}` (listed pools only); `ls_color`, `ls_color_deploys` (skein-1color README) |
| `GET /requests` | amm-p2p `requests` (0.9.0): the holders' listing requests not yet registered, newest first, `[{tokenId, from, at}]`; a read |
| `GET /spends?outpoint=<txid>.<vout>` | amm-p2p `spends` (0.9.0): the chain state of an outpoint, a pool followed to its current output, `{outpoint, spentBy?, current?, hops, closed}`; a read |
| `GET /listTopicManagers`, `/listLookupServiceProviders`, `/getDocumentationFor…` | the listings (each program's `metadata` / `documentation`); reads since 0.5.0 |
| `GET /.live/tm_color_<txid>_0-live` | the runtime's liveness read (skein #138, no program): `[{sender, at, body, from}]` newest first, the beats within the window (`body` the topic's view digest; the validator is `sender`, its peer ID `from`); 404 when the app keeps no liveness for the topic (the topic not registered) |
| `GET /color/tokens` | color-lookup `tokens`: the token list, `{limit?, skip?}` → `[{tokenId, topic, sym, dec, icon?, txid, vout}]` (`icon` a 1Color deploy's embedded image as a data URL, skein-1color 0.9.0); a read (0.6.0) |
| `POST /call` | amm-p2p: `{fn, args}` for the two interfaces below |
| `GET /…` (prefix `/`) | amm-p2p `serve`: the pages, `www/` of the app's own tree (skein-sdk `files.serve`); a read since 0.5.0 |

A token id in a query is any form (`<txid>`, `<txid>_<vout>`, `<txid>.<vout>`); the pages write every token id as `<txid>_<vout>`, 1Color and legacy BSV-21 alike, `_0` included (BRC-162 "Token identification"; 0.7.1, David 2026-10-07), and show outpoints as `<txid>.<vout>` (0.6.3, David 2026-10-08), while `ls_amm`'s `outpoint` and the relay's take `<txid>_<vout>`.

Which of these is a read and which a route with a handler is "Read routes",
below. `GET /live` is gone (0.5.0; it answered 410 from 0.4.0).

**Box `amm/register`** (the manifest's `"register"`, relative to the app,
shruggr/skein#128): the engine's `register {topic, program}`, `deregister
{topic}`, `registerLookup {service, program, topics?}`, `deregisterLookup
{service}` (skein-overlay 0.11.0: a token is its topic `tm_color_<assetId>`
and its lookup `ls_color_<assetId>`, BRC-207), from root (`roles`, 0.7.0). skein-overlay 0.7.7+ takes them in this box only
(0.7.5–0.7.6: `amm/overlay`).

**Box `amm/submit`** (the manifest's `"submit"`, `kernel.beef`): the
engine's `submit {beef, topics, offChainValues?}` from anyone (the
validator's own submissions among them) and `POST /submit`'s submission
event (skein-overlay 0.7.6), answered to the sender in this box; `register` / `deregister` here are refused (`bad-args`).

**Box `amm/amm-p2p`** (a message `{fn, args}`, skein docs/APPS.md §4; the
box `amm` before 0.7.0), and the route `/call` (a signed request):

| interface | functions | from |
|---|---|---|
| `amm.swap/1` | `submit` (writes), `status`, `terms` | anyone (root too) |
| `amm.pool/1` | `terms` (0.9.0: `{validator, peerId?, lpFeeBps, validatorFeeBps, commissionBps}`, this skein's validator and its terms; 0.15.0: the record `amm/terms`, or the error "no validator here" when none is set), `affirm` (0.13.0: `{pool: bytes, satoshis: int}` → `{validatorPubKey: bytes, nonce: bytes, signature: bytes}`, or the refusal `<reason>[: <detail>]`; no writes; 0.11.0–0.12.x: `{pool, satoshis, keyInput}` → `{signature}`) | anyone |

`amm.liquidity/1` is gone (0.9.0, with AddLiquidity); `amm.pool.submit` and
`amm.pool.status` (0.9.0's delivered deploy) are gone (0.11.0).

**Box `amm/validator`** (0.11.0): root's `{fn: "rescind", args: {pool:
"<txid>.<vout>"}}` (0.13.0; `close` before) → amm-validator (above, "The
validator's Rescind"), answered `{fn: "rescind", request, result | error}`
to the sender in this box.

**Box `amm/requests`** (0.9.0): a holder's listing request, `{fn:
"request", args: {tokenId}}` from anyone → amm-p2p, recorded under the head
`amm/requests` (the latest per token); nothing else runs. Settings reads
them with `GET /requests`; registering the token settles its request.

**Box `amm`** (derived): the engine's own `watch`, `resume`, `wait` (from
the instance itself) and, as an event route, the libp2p routes' admits.

`amm.swap.submit` takes the funding transaction (the wallet's `noSend`
action) and the swap, and the validator the caller names —
`validator` (its identity key) and `peerId` (its libp2p peer ID, text, 0.4.0)
— checks the pair, records it under `amm/app` and relays it to that
validator: it dials `peerId` over libp2p, or, when `peerId` is its own
node's, calls its own `amm-validator` in-VM with the same package (a node
does not dial itself). It looks at no liveness. The record settles
`accepted`, `refused`, `timeout` or `failed` (amm-p2p README "The
marketplace relay").

`amm.pool.affirm` is not relayed: amm-p2p packages the request (box
`affirm`, from this instance to itself) and calls its own amm-validator's
fn `affirm` in-VM, which answers at once.

**libp2p**: `/amm-validator/1/swap` (0.9.0: `/addLiquidity` gone; 0.11.0:
`/deploy` gone) (amm-validator; one signed-message package per frame); the engine's
`<topic>` (submit and admit messages, one mesh) and `-proof` for each
registered topic (skein-overlay 0.12.0; `-admit` before); `tm_color_<txid>_0-live`,
beaconed by a validator's host and subscribed, without admitting anything,
by a market's liveness tool (below); `ls_amm-live`, the AMM lookup's beat
(the prices, above), once root registers `ls_amm`; `/skein/overlay/sync/1.0.0`,
the overlay sync protocol, answered by the engine's filter `sync` (0.14.0);
`/skein/overlay/beef/1.0.0`, the engine's receive row, a catch-up's fetched
Atomic BEEFs (0.14.1).

**Box `amm/amm-p2p`** (also): the cron provider's ticks, `{kind: "amm-p2p-tick",
job: "catchup"}` — a utility nothing schedules (0.6.0). amm-p2p takes no
start or stop: the manifest has none, and the owner's `{kind:
"amm-p2p-start" | "amm-p2p-stop"}` is refused (`BadMessage`).

**Market and validator, always** (0.6.0, shruggr/skein#120; 0.8.1,
skein-overlay 0.12.0). David, 2026-10-09: "every skein is marketplace AND
validator from install, always". Two roles of the engine, never off; the
owner's switch of 0.6.2 (skein-overlay 0.9.2, `{fn: "market" |
"validator"}` in `amm/register`, kept in `amm/topics`) and the Token topics
page's Market and Validator switches are gone. `config.overlay.market:
{window}` / `.validator: {every}` give the ms (this manifest sets neither:
40 000 / 30 000). (0.8.1's Validator page is gone with the 0.9.0 pages.)

The engine, on `register {topic: "tm_color_<txid>_0", program: "color-topic"}`
(box `amm/register`, from root): subscribes the topic and seeds; emits
`{event: "liveness", topic: "tm_color_<txid>_0-live", window}` and
`{event: "beacon", topic: "tm_color_<txid>_0-live", every, body}` — the
frame carries the sender's identity key and the gossip message the peer
id; the body is the topic's view digest `{view: {count, digest}}`
(skein-overlay 0.12.0), re-declared when it changes. `deregister` reverses
both (`unliveness`, `unbeacon`). A start or a re-read emits nothing: the
intents stand in the log.

- **The validator.** amm-validator signs swaps and affirms an LP's
  proposed pool at its terms (above, "The deploy and the affirmation")
  — for any token whose topic is in the engine's registered set (the head
  `amm/topics`; always, 0.8.1); otherwise every
  `swap` and `affirm` is refused `{ok: false, reason:
  "not_validating", detail}` before anything is checked or signed. It reads
  the set at each call. Each beat is the host's signed
  frame (docs/MESSAGES.md "Beacons"): dag-cbor `{body, at, sender,
  signature}`, `body` the topic's view digest, `sender` the instance's identity key; the
  gossip message's peer is the node's peer ID, derived from the instance's
  root (`[2, "skein instance"]`, key ID `libp2p:<handle>`, counterparty
  self; skein signer.ts `peerKey`).
- **The market.** The runtime's liveness tool subscribes `tm_color_<txid>_0-live`
  without admitting its messages (no entry, nothing logged), verifies each
  beat's signature against `sender`, and keeps the beats newer than
  `window`, the latest per sender, with the node's own published beats, in
  memory; it serves them at `GET <base>/.live/tm_color_<txid>_0-live` → `[{sender,
  at, body: <base64>, from: <peer ID>}]` newest first (404 when no liveness
  is kept). The window is a margin over the beat (40 s against 30 s).
- **The page** reads that endpoint per token, takes each beat's `sender`
  (the validator) and `from` (its peer ID), plans (`market/plan.ts`) only
  over the pools whose validator is there, and names the chosen one in the
  swap (`validator` and `peerId`); the prices combine the `ls_amm-live`
  beats over the live validators (above, "Prices").

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
lookups `ls_color`, `ls_color_deploys` (both `color-lookup`) and
`ls_amm` (`amm-lookup`), none with a `topics` list, so each listens to every
topic served; gossip on (the default); no `market` or `validator` (0.8.1:
both always on, at the defaults 40 000 / 30 000 ms; 0.6.0–0.6.1 set
`market: {window: 40000}` and `validator: {every: 30000}`, 0.6.2–0.8.0 had
root's switch); `config.amm` (`commission`; 0.9.0 to 0.14.2 also
`ammValidator`, the validator's terms, and before 0.9.0 the bounds
`minValidatorFeeBps`, `maxLpFeeBps`, `maxCommissionBps`: 0.15.0 moved the
terms to the runtime record `amm/terms`, "The terms" above); `provides` the two `amm.*` interfaces (`amm.swap.submit` with
`peerId: "string"` beside `validator`, 0.4.0; `amm.pool.terms`, 0.9.0;
`amm.pool.affirm`, 0.11.0, in place of `amm.pool.submit` / `status`;
0.13.0: `{pool, satoshis}` → `{validatorPubKey, nonce, signature}`);
`requires: ["chain/1"]`; no `start` / `stop` (0.6.0). No
`config.overlay.terms` (skein-overlay 0.16.0 no longer reads it: the beat's
`terms` is the record `amm/terms`); `roles.root` `register`,
`registerLookup`, `deregisterLookup`, `rescind` and (0.15.0) `terms`.

### Routes

shruggr/skein#143 (0.7.0): a route names a transport, an address, its
filters and a handler; no sender. Mailbox addresses are relative to the app
(shruggr/skein#128: `"x"` is the box `amm/x`); one route per box. Who may
run a function is `roles`: `{"root": ["register", "registerLookup", "deregisterLookup", "rescind"]}`.

1. `register` → `overlay.register` (register / deregister and
   registerLookup / deregisterLookup; root's: any other key's message is recorded and runs nothing)
2. `submit`, `kernel.beef` → `overlay.submit` (submissions by message from
   anyone whose BEEF validates; the validator's own among them; skein-overlay 0.7.6)
3. `amm-p2p` → `amm-p2p` (the relay's interfaces by message, `{fn, args}`,
   answered to the sender in that box; the cron provider's ticks). Before
   0.7.0 the relay's message path was the app's box `amm` (a row from `*`
   after the engine's `event` and `$self` rows); since #143 a box has one
   route, and `amm` is the engine's (its own watch, resume and wait), so the
   relay took its own box.
4. `validator` → `amm-validator` (0.13.0: root's `rescind`, and the steps
   of its thread; 0.11.0–0.12.x: `close`; 0.9.0–0.10.2: the claim's `rescind`)
5. `requests` → `amm-p2p` (0.9.0: a holder's listing request, anyone)
6. http `/call`, `kernel.brc104` → `amm-p2p.call` (a signed request; the
   caller is its key)
7. libp2p `/amm-validator/1/swap` → `amm-validator.swap` (refused
   `not_validating` for a topic not registered; `/addLiquidity` gone, 0.9.0;
   `/deploy` gone, 0.11.0: `affirm` is called in-VM only)
8. libp2p `/skein/overlay/sync/1.0.0`, no handler, the filter `sync` →
   `overlay.sync` (0.14.0, skein-overlay 0.15.0, shruggr/skein#154): a
   peer's catch-up frames (the locator, the admitted lists by height
   range, a transaction's Atomic BEEF), answered by the engine through the
   front door, a read, nothing logged
9. libp2p `/skein/overlay/beef/1.0.0`, `kernel.beef` → `overlay.submit`
   (0.14.1, as in skein-overlay's own manifest): the host hands each
   Atomic BEEF a catch-up fetches to this row, as from the serving peer;
   without it the puller refused every one ("no route for
   libp2p:/skein/overlay/beef/1.0.0")
10. the read routes, below

then, derived by the install from `config.overlay`: the box `amm` as an
`event` route and a `mailbox` route → `overlay` (gossiped submissions,
peers' admits; the engine's own watch, resume, wait), http `/submit`
(`kernel.beef`) → `overlay.submit`, and the read route `/lookup` (the
filter `lookup`, `overlay.lookup`).

### Read routes

An http route with no handler is a **read**: its filters run over the
request and the last one answers (`{answer: {status, type, body}}`), any
method, signed or not, **no entry, nothing logged** (a function that
writes fails inside the call). Exact paths match before a prefix, so
`/call`, `/submit` and `/lookup` are taken before the read `/`.

| path under `/amm/` | route | filter → program, fn | since |
|---|---|---|---|
| `/submit` | handler (derived), `kernel.beef` | → `overlay.submit` | |
| `/call` | handler, `kernel.brc104` | → `amm-p2p.call` | |
| `/lookup` | read (derived) | `lookup` → `overlay.lookup` | skein #135 |
| `/listTopicManagers`, `/listLookupServiceProviders` | read | `listTopicManagers`, `listLookupServiceProviders` → `overlay.<the same>` | 0.5.0 (http rows before) |
| `/getDocumentationForTopicManager`, `/getDocumentationForLookupServiceProvider` | read | `topicDocumentation`, `lookupDocumentation` → `overlay.<the same>` | 0.5.0 (http rows before) |
| `/` (prefix) | read, `root: "www"`, `index: "index.html"` | `page` → `amm-p2p.serve` | 0.5.0 (an http row before) |
| `/color/tokens` | read | `tokens` → `color-lookup.tokens` | 0.6.0 |
| `/requests` | read | `requests` → `amm-p2p.requests` | 0.9.0 |
| `/spends` | read | `spends` → `amm-p2p.spends` | 0.9.0 |
| `/.live/tm_color_<txid>_0-live`, `/.live/ls_amm-live` | the runtime's liveness read (no program, skein #138) | | |
| `/live` | gone | | 0.5.0 (410 in 0.4.0) |

amm-p2p's `serve` reads only (the head `amm/app` and the tree's blobs);
called as a filter it answers `{answer: <the page>}` (0.7.0).

**At the origin's root** (0.9.0: the landing at `/`, shruggr/skein#147):
root adds a root route to the same filter (skein-overlay README "Root
routes": root's own route, no `app`, outside `/<app>/`); the pages served
there take `<origin>/amm` as the app's base (0.8.1). No skein change is
needed:

```
skein routes add --transport http --prefix --filters amm.page --settings '{"root":"www","index":"index.html"}' / <origin>
```

## Not wired

- **Pools deployed before 0.13.0, on chain.** amm-lookup and
  amm-validator know layout 2 only: a layout-1 pool still listed stays in
  `pools` until it is spent (the spend drops it), its Swap continuations
  are not listed, and the validator does not sign its swaps (`not_a_pool`)
  or rescind it. The pages close and recover it (the contract's own Close,
  the LP key from its deploy's input 0).
- **Recovering a 0.13.0 pool from another browser.** Its LP keyID is random
  and kept only in its row (and in the deploying browser until the row is
  filed): a deploy whose filing failed can be recovered only in that
  browser. The validator can still Rescind it.

- **The want-answer stream** `/skein/overlay/beef/1.0.0` (skein-overlay
  0.7.1+'s manifest row): not carried, as skein-1color 0.6.0 does not; a
  submission paused on a parent resumes only when a later submission brings
  it.
- **`tm_color_<txid>_0-live` for an unregistered token.** Liveness is
  kept for a registered topic (every skein a market, 0.8.1); for a token
  not registered here the `.live` read answers 404 and its Swap page plans
  nothing.
- **Catch-up and proofs by block.** Never specified; sync is
  shruggr/skein#112's `want`. The `/amm/proofs/1.0.0` row is gone and no
  start schedules the catch-up pass; the code stays as a utility
  (`proofsByBlock`, `catchup`).
- **A setting changed after a register.** The roles' events are emitted at
  `register` / `deregister` only (the
  intents stand in the log; root's switch is gone, 0.8.1): a market window or a beat changed by a
  reinstall applies to the topics registered after it; a topic registered
  before keeps its standing liveness and beacon (deregister and register it
  again). An instance upgraded from 0.5.0 keeps
  the beacons and liveness amm-p2p emitted then (keyed by app and topic, as
  the engine's are; a register of a topic registered already emits nothing,
  so deregister and register it to replace them). Their beats still carry
  0.5.0's body, which the page no longer reads.
- **Governance per token** (#120 item 4) and the other 1Color queries:
  skein-1color docs/1COLOR.md "Not built".
- **`ls_amm` beats once registered.** skein-overlay 0.12 declares a
  lookup's beacon (and keeps liveness on `<service>-live`) at
  `registerLookup`, not for a service the manifest declares; `ls_amm` is
  declared (so `/lookup` answers it from install) and root registers it too
  (the Settings page's register sends `registerLookup ls_amm`,
  idempotent). Until then there are no prices beats, and the pages price a
  token from this skein's own listing.
- **Root, as the page sees it.** No read tells a page a key's grants
  (shruggr/skein#143): `isRoot` is a stub, every connected wallet sees
  Settings, and the skein refuses a non-root's register messages.
- **Pending payouts.** A payout the wallet did not take in at once (sats or
  tokens, or a Move back not yet admitted) is kept in the browser
  (`wallet/pendingPayouts.ts`); 0.8.1's Pending payouts page, which retried
  them, is gone with the old pages, and the Open Exchange pages have no
  place for it.
- **Spends the chain app records of a transaction never submitted to the
  topic** (a pool spent by a transaction that bypassed the overlay): the
  listing does not fold them out (skein-overlay 0.12.1 "Beats"); the LP's
  page reads them (`/spends`).
- **Run end to end.** The programs are tested natively and the pages
  against fakes. 0.13.0's deploy depends on the BRC-207 module supplying the
  input BEEF of the token inputs it selects (color-permission-module 0.1.1,
  in progress), not tested here against a live wallet.
