# Changelog

Released versions of skein-amm. 0.10.0 is the first entry here; the notes
on earlier versions are below as the README carried them at 0.9.1, in the
names of their time, and each release commit names what changed.

## 0.13.0 (2026-10-22)

The validator sign-off redesign (David Case, 2026-10-22; agent-env
plans/validator-signoff-2026-10-22.md §1, §2, §7, §9; docs/AMM.md).

- **Contract** (pool/Pool.runar.go): state gains `Nonce` (32 bytes) and
  `Affirmation` (the validator's DER signature), never assigned, carried by
  every continuation; no BRC-162 payload any more. `Close(lpSig)`: a plain
  signature by the LP key, outputs free. `Rescind(validatorSig)`: the pool
  written back at output 0 with ValidatorPubKey 33 zero bytes, everything
  else kept, the validator's change after it; Swap (unchanged) is impossible
  after it. The template grows from 2,657 to 3,568 bytes (a pool output from
  2,996 to 4,011).
- **The affirmation**: message v2 (`"skein-amm affirm 2"`, no termsHash, the
  nonce last; 221 bytes); the first validator key is the identity's child
  keyed by the nonce (64 hex digits); `amm.pool.affirm {pool, satoshis}` →
  `{validatorPubKey, nonce, signature}` (no `keyInput`; the validator picks a
  random nonce).
- **ls_amm**: lists a pool that attests itself (the key derived from
  identity + nonce, compared, the affirmation verified) or a Swap's
  continuation of a listed pool naming the next key (the identity's child of
  the spent outpoint); a rescinded pool, a Close's output and any other key
  are not listed.
- **amm-validator**: root's `close` → `rescind` (rescind.zig); the current
  key: a Swap's continuation by the spent outpoint, else a self-attesting
  pool by its nonce.
- **Pages**: the deploy names no input (outputs [pool], no `inputPositions`;
  the module selects the token inputs); the LP key a fresh random BRC-29
  keyID kept in the row (and in the browser until the row is filed); the
  Close pays the tokens to a new module receiving key and the sats as a
  BRC-29 payment (no `amm-swap`, no Move back); Recover takes both layouts;
  pools before 0.13.0 decode, close (their covenant outputs) and recover.
- Fixtures regenerated (`pool_deploy_own_key`, `swap_bad_next`, `rescind`,
  `close_rescinded`; `close_fee` and `close_validator` gone); the 0.12.x
  artifact and vectors kept for the pages' layout-1 path.
- Tests: Go 8 (73 with subtests), `zig build test` 63, web/ui 144, web/engine 35.

## 0.12.1 (2026-10-22)

Pages only (`web/ui`, `www/`): no contract, validator, lookup or manifest
change.

- **Recover a position** (Liquidity, src/lp/recover.ts): paste a pool
  outpoint (`<txid>.<vout>` or `<txid>_<vout>`, the deploy's or a later
  one); the pool is followed to its current output (`/spends`), read from
  `ls_color`, walked back through its BEEF to the deploy, and its LpPubKey
  checked against the wallet's LP key for the deploy's input 0 (BRC-29,
  else pre-BRC-29). A match is filed in `amm-pools` as a deploy files it
  (`poolCustomInstructions`, label `amm-pool-deploy`); no match files
  nothing. A pool already filed is left alone. Close is unchanged.

## 0.12.0 (2026-10-22)

The pages (`web/ui`, `www/`) move onto the wallet's BRC-207 1Color module
(David Case, 2026-10-21/22; docs/AMM.md "The pages and the wallet's
module"). Nothing outside the pages changed: `bin/`, the contract, the
fixtures and `www/color/` are 0.11.0's.

- **Dependencies**: `@1sat/*` 0.1.0 (published), `@bsv/sdk` 4.0.1,
  `@bsv/templates` 2.1.0, `@bsv/color-permission-module` 0.1.0 and
  `@bsv/wallet-toolbox-client` 2.15.0 through the interim `@bopen-io`
  repacks (npm aliases and `overrides`; one copy of `@bsv/sdk`). The
  template is 1sat-sdk's `OneColor` (was `Mandala`; `src/pool/oneColor.ts`).
  The local 1sat-sdk checkout alias (`ONESAT_SDK_DIR`, aff025c3) is gone.
  The `mandala <txid> <vout>` and `bsv21` baskets and their custom
  instructions are gone.
- **Your tokens**: the module's assets (`p color assets`) and balances
  (`p color balance <assetTag>`, exact); metadata from the token list, else
  `ls_color_deploys`, else a held deploy; what the active swap basket holds,
  with Move back.
- **Pool deploy**: `amm.pool.affirm`, then ONE createAction (key input at
  index 0, the affirmed pool at output 0 as a transfer to a foreign script,
  the key input's rest to a fresh module key, `randomizeOutputs: false`);
  the module funds the tokens and submits; the page submits too and files
  the pool in `amm-pools`. Gone: the funding transaction, SIGHASH_SINGLE,
  the claim, `amm.pool.submit` (`lp/deployFlow.ts`, `lp/poolRelay.ts`),
  `completePoolDeploy`, `abandonPoolDeploy`, a position's "rescinded".
- **Swaps**: tokens in, the legs spend the active swap basket `amm-swap`
  (keys `[2, "amm swap"]`, counterparty self); the legs it does not cover
  get an output each from ONE module send before the legs. Token payouts
  (and the taker's own token commission) go to a module receiving key, the
  pool's validator the sender, taken in with a receipt. Pending payouts
  keep receipts as well as BRC-29 remittances.
- **Close**: the tokens (paid by the contract to the LP key) are filed in
  `amm-swap`; Move back takes them into the wallet.
- **Move back**: every `amm-swap` output of a token, with a broadcast
  funding for the fee, to one module receiving key of the user's own;
  submitted to the token topic; taken in through the module.
- **Manifest**: baskets `amm-swap`, `amm-pools`, `1sat-deposit`; protocol
  `[2, "amm swap"]` (self) added; `bsv21`, `1sat` and `[0, "onesat"]`
  dropped; no spending authorization; no `p color` entry.

## 0.11.0 (2026-10-22)

Pool deploys move to the BRC-207 wallet module (David Case, 2026-10-21):
the LP's wallet builds, signs and broadcasts the deploy as one action and
the module submits it to the token topic, so the validator can no longer
append its claim to a delivered deploy. The claim, as a token unit paid to
the validator, is gone; the validator's consent is recorded at deploy in the
pool output itself, and delisting is the contract's Close. A contract
change: the Pool script, its artifact and every fixture are new.

- **The contract** (`pool/Pool.runar.go`): `Close(sig, bsvFee)` takes the
  LP key or the current validator key. The payout is always the LP's (the
  pool's sats less bsvFee, then every token in one BRC-162 value output). On
  the validator's path bsvFee must be 0: bsvFee > 0 checks `sig` against the
  LP key alone; bsvFee 0 checks it as one of the two keys (a 1-of-2
  `OP_CHECKMULTISIG`, so a validator's signature is never a failed
  `OP_CHECKSIG`, which NULLFAIL forbids). Swap is unchanged.
- **The affirmation** (docs/AMM.md "The deploy and the affirmation"):
  `amm.pool.affirm {pool, satoshis, keyInput}` → `{signature}` (amm-p2p,
  `writes: false`; it calls amm-validator's new fn `affirm` in-VM). The
  validator checks the proposed pool against its terms and its key, and
  signs, by the pool's validator key, sha256 of the 221-byte message
  `"skein-amm affirm 1" ‖ AssetId ‖ the three fees ‖ TokenReserve ‖ the
  pool's satoshis ‖ LpPubKey ‖ ValidatorPubKey ‖ ValidatorIdentity ‖
  termsHash`. The deploy's pool output carries the signature as its BRC-162
  payload (`<push sig> OP_DROP` after the prefix; the contract never reads
  it; continuations carry none). src/pool.zig `affirmMessage`, `affirmed`,
  `withAffirmation`; web/ui `PoolTemplate.affirmationMessage`, `affirmed`,
  `lockDeploy(args, state, affirmation)`.
- **The listing** (`ls_amm`): a deploy's pool is listed only when its
  affirmation verifies against its parameters and the ValidatorPubKey in its
  state; a Swap's continuation of a listed pool is listed; a Close (the LP's
  or the validator's) ends it. Only listed pools are indexed. The claim rule
  (`claims`, `spentClaims`, `NotListed`) is gone.
- **The validator's Close** replaces the rescind: root's `{fn: "close",
  args: {pool: "<txid>.<vout>"}}` in the box `amm/validator` (roles: root's
  `close`). amm-validator follows the pool to its newest output and has the
  instance's wallet build the Close with the pool as a caller input
  (createAction, then signAction with the Rúnar call: the pool's current
  validator key's signature, bsvFee 0, the wallet's change as Rúnar's), the
  wallet paying the fee; submits it to the token topic and answers root.
- **Gone:** `amm.pool.submit` and `amm.pool.status` (the delivered deploy,
  SIGHASH_SINGLE pairs, the unassigned unit), libp2p
  `/amm-validator/1/deploy`, the claim's filing in the wallet (`amm-claims`),
  root's `rescind`.
- **Fixtures** (`go run ./gen/vectors`): `pool_deploy` is a plain wallet
  deploy (token input and funding, ALL|FORKID) whose pool carries the
  affirmation; `pool_deploy_unaffirmed` and `pool_deploy_forged` must not
  list; `close_validator` (the validator's Close of pool 2, its own funding)
  replaces `rescind`; `pool_deploy_delivered`, `pool_deploy_wrong_key` and
  `claim_vout` are gone. Zig and the pages reproduce the affirmation and the
  validator's Close byte for byte.

**Not in this version:** the pages. The Liquidity page still builds 0.9.0's
delivered deploy and calls `amm.pool.submit`, which no longer exists; its
move to the wallet's BRC-207 module and `amm.pool.affirm` is the next build
(docs/AMM.md "Not wired"). `www/` is rebuilt for the template only.

## 0.10.2 (2026-10-21)

The Exchange's "Deploy a new token" link points at the app's own base
(`<origin>/amm/color/deploy/`) instead of `color/deploy/`, which at the
skein's root resolved to `/color/deploy/`, where the 1Color pages find no
app name in their URL and skip the overlay submit. `www/` rebuilt; nothing
else changed (`bin/`, `www/color/` as in 0.10.1).

## 0.10.1 (2026-10-21)

Re-pinned onto shruggr/skein-1color 0.10.1 (d7348cf, tree 7342c1df): its
pages, copied into `www/color/` by `scripts/color-pages.sh`, deploy through
the wallet's BRC-207 `color` module (`@1sat/actions` 0.1.0 `deploy1Color`,
the basket `p color <txid>v0`) instead of `deployMandala` and the basket
`mandala <txid> 0`. skein-1color's programs are unchanged, so `bin/` is
unchanged. The AMM's own pages (`web/ui`) are as in 0.10.0.

`web/ui/test/manifest.test.ts` follows 690e754 (the manifest asks for no
`spendingAuthorization` on connect, David Case 2026-10-21): the test still
expected it. The manifest itself is as 690e754 left it.

## 0.10.0 (2026-10-21)

Re-pinned onto shruggr/skein-1color 0.10.0 (the 1Color rename; BRC-162
retitled 1Color) and shruggr/skein-overlay 0.14.0; the image moved here from
skein (shruggr/skein#152). Decided with David Case, 2026-10-21. The
Open Exchange's flows are as in 0.9.1.

**Pins** (`build.zig.zon`): `skein_1color` v0.10.0 (3de96c4, tree
b51ad172) in place of `skein_mandala` v0.9.2; `skein_overlay` v0.14.0
(385e1b9). `bin/color-topic.wasm` and `bin/color-lookup.wasm` are
skein-1color 0.10.0's, `bin/overlay.wasm` skein-overlay 0.14.0's (the same
bytes as skein-1color 0.10.0's engine); `bin/amm-*.wasm` rebuilt (a clean
rebuild, fresh caches, gives the same bytes). The programs compiled against
0.14.0's engine sources and contracts unchanged: amm-validator's topic
judgement names no ancillary txids (0.13.0's verdict field stays empty),
and 0.14.0's beat fields and its reads `/holds`, `/anchors`, `/admitted` are
the engine's (this manifest routes none of them, as skein-1color's does
not).

**BRC-207's wire names**, no aliases:

| until 0.9.1 | 0.10.0 |
|---|---|
| `tm_mandala_<assetId>`, `tm_mandala_<txid>_0-live` | `tm_color_<assetId>`, `tm_color_<txid>_0-live` |
| `tm_mandala` (discovery) | `tm_color` |
| `ls_mandala_<assetId>` | `ls_color_<assetId>` |
| `ls_mandala`, `ls_mandala_deploys` | `ls_color`, `ls_color_deploys` (index head `<app>/ls_color`) |
| `mandala-spendability`, `mandala-admission` | `color-spendability`, `color-admission` |
| programs `mandala-topic`, `mandala-lookup` | `color-topic`, `color-lookup` |
| read `/mandala/tokens`, filter `mandala-lookup.tokens` | `/color/tokens`, `color-lookup.tokens` |
| pages `www/mandala/` (`<base>/mandala/`) | `www/color/` (`<base>/color/`) |
| Zig module `mandala` | `1color` (`@import("1color")`) |
| `scripts/mandala-pages.sh` | `scripts/color-pages.sh` (skein-1color v0.10.0's tarball, sha256 9211e792…) |

The asset id stays `<txid>_<vout>`, `_0` included. The pages say 1Color
where they said Mandala. The SDK's names are unchanged: the template is
still imported as `@1sat/templates/mandala` (`Mandala`), and 1sat-sdk's
basket `mandala <txid> <vout>` and label `mandala` are what it files.

**After upgrading a running skein from 0.9.1**: the 1Color lookup's index
head is now `<app>/ls_color` (it was `<app>/ls_mandala`), so it starts
empty; the registrations under `tm_mandala_*` / `ls_mandala_*` are not
recognised by the new programs (`color-topic` and `color-lookup` take only
`tm_color_*` / `ls_color_*`, and amm-lookup, amm-validator and amm-p2p only
`tm_color_*`). Root registers each token again under the new names (its
topic, its lookup, and `ls_amm`), and drops the old ones.

**The image** (`image/etc/apps.json`, shruggr/skein#152: skein's
`images/open-exchange` removed): the chain app (shruggr/skein-chain v0.6.0
by release tarball and tree id 13a517e9…) then this app (`..`), and root's
routes `/submit`, `/lookup` and `/`, as the old image had them (README.md
"The image").

**The pages** (`www/`) were built from the same 1sat-sdk source as 0.9.1's
(the embedded-icon draft, aff025c3): the bundle is 0.9.1's with only the
names and the words changed. web/ui gains `@ipld/dag-cbor` and `cborg` as
dependencies, deduped and mapped like `cbor2`, because the template's
sources import them and the checkout has no `node_modules`.

Tests: `zig build test` 70/70 (as 0.9.1: the names, the manifest's
`/color/tokens` route, and the old names refused); web/ui 133/133; web/engine
35/35.

## Before 0.10.0

The README's notes, as they stood at 0.9.1 (0.9.1: on skein-overlay 0.12.1
and skein-mandala 0.9.2):

0.8.1: on skein-overlay 0.12.0 and skein-mandala
0.9.1 (skein-sdk v0.11.0, skein log format 10: the BEEF envelope beside the
pointer record, shruggr/skein#146), every skein a market and a validator
from install, always (David Case, 2026-10-09: no switch; the Validator
page shows both always on), the token topic's gossip one mesh for submit
and admit messages with `-proof` and `-live` separate, each registered
topic's beat body its view digest; the pages at the origin root (`/`,
shruggr/skein#147) take `<origin>/amm` as the app's base. 0.8.0: on
skein-overlay 0.11.0 and skein-mandala
0.9.0 (BRC-207; David Case, 2026-10-08), a token's topic is
`tm_mandala_<assetId>` and its lookup `ls_mandala_<assetId>`, the asset id
`<txid>_<vout>` (a Mandala token's `tm_mandala_<txid>_0`, its liveness topic
`tm_mandala_<txid>_0-live`); 0.7.2's `tm_<txid>_0` is no topic (no alias).
Registering a token is two engine calls, its topic then its lookup (the
Token topics page, `mandala/tokens/`, skein-mandala 0.9.0's); the lookup
answers BRC-207's `mandala-spendability` and `mandala-admission`. A Mandala
deploy's icon is embedded in it (BRC-162 draft bsv-blockchain/BRCs#308:
`[mediaType, bytes]`, the pointer forms gone): the Tokens page shows it and
the deploy form embeds it (no icon output). `ls_amm` keeps its index at
`amm/ls_amm` (skein-overlay 0.11.0: one index per lookup program). 0.7.2:
on skein-mandala 0.8.2, a token's topic was `tm_<tokenId>`, `_<vout>` always: a Mandala token's `tm_<txid>_0`, its
liveness topic `tm_<txid>_0-live`; the bare `tm_<txid>` is no topic
(David, 2026-10-08: "that was the decision all along"). 0.7.1: on
skein-overlay 0.10.0 and skein-mandala 0.8.1, every token id is `<txid>_<vout>`, Mandala and legacy
BSV-21 alike, `_0` included, as BRC-162 "Token identification" writes it
(David, 2026-10-07, shruggr/skein#120; supersedes 0.6.3's bare txid).
0.7.0: skein's routes, filters and roles
(shruggr/skein#143), on skein-overlay 0.10.0 and skein-mandala 0.8.0 — the
manifest's `dispatch` and `reads` are `routes` (no senders): the box
`amm/register` is root's (`roles: {root: ["register", "market",
"validator"]}`), the reads are read routes whose filters answer (the pages:
the filter `page`, amm-p2p's `serve`), `/call` lists `kernel.brc104`, and
the box `amm` is the engine's alone, so amm-p2p's message path moved to its
own box `amm/amm-p2p` (docs/AMM.md "Routes"). 0.6.3: the pages wrote a Mandala token's id as the bare
`<txid>` (superseded by 0.7.1), show outpoints as
`<txid>.<vout>`, let every id shown be expanded and copied, and send
`POST /submit` unsigned, with plain `fetch` (David, 2026-10-08). 0.6.2: market and validator are the owner's switch, both
off in the manifest (David, 2026-10-07: "this shouldn't have been a config
in the manifest. This should be a setting that the user is configuring";
"Use it", item 4). 0.6.1: `POST /submit` is BRC-22 again (it answers the
STEAK, and the pages read it; shruggr/skein#112). Market and validator are the engine's two settings
(0.6.0, shruggr/skein#120; David, 2026-10-06 evening: "a skein runs as a market and/or a validator by two settings in the engine's configuration (`config.overlay.market: {window}`, `config.overlay.validator: {every}`), and registering a token's topic is the one act that drives both"): with
`config.overlay.market` the engine's `register` asks the runtime's liveness
tool for the token's `tm_<txid>_0-live`, with `config.overlay.validator` it
beacons it, and amm-validator signs for every registered token; amm-p2p's
`validate` / `unvalidate`, its validated set, its beacons, its liveness and
its start / stop are gone. The listings, the documentation, the pages and
the token list (`/mandala/tokens`) are reads (0.5.0, shruggr/skein#135:
docs/AMM.md "Rows and reads"). The page reads `GET
<base>/.live/tm_<txid>_0-live`, plans over the pools whose validator is there
and names that validator (identity key and peer ID) in the swap, and the
relay dials the peer it is given — or, when it is its own node, hands the
request to its own validator program in-VM. Ported from amm-poc (b-open-io/amm-poc, its
`programs/`, `pool/` and `web/`) in 0.1.0 (shruggr/skein#120).
