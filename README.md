# skein-amm

A non-custodial BSV ↔ token AMM over 1Color tokens (BRC-162), as one
[skein](https://github.com/shruggr/skein) overlay app, name `amm`. Its tree
carries the overlay engine, the 1Color components, the AMM's own programs
and its pages. Version **0.11.0** (David Case, 2026-10-21; docs/AMM.md
"The pool rule", "The deploy and the affirmation", "The listing",
"Prices"): pool deploys move to the BRC-207 wallet module, so the
validator no longer appends a claim to a delivered deploy. Before the LP's
wallet builds the deploy, the page asks this skein's validator
(`amm.pool.affirm`) to affirm the proposed pool: it checks it against its
terms (`config.amm.ammValidator`, `amm.pool.terms`; the fees are the
validator's, skein-wide) and answers its signature, by the pool's validator
key, over the pool's parameters and the hash of the terms; the deploy's
pool output carries it as its BRC-162 payload, and the deploy reaches the
overlay through the token topic like any token transaction. `ls_amm` lists
a deploy's pool only when that signature verifies, a swap's continuation of
a listed pool with it, and beats its prices (per token, per validator, its
listed pools' totals) on `ls_amm-live`. The contract is Swap and Close
(`Close(sig, bsvFee)`: the LP's with any bsvFee, or the current
validator's with bsvFee 0; everything out to the LP either way); delisting
is the validator's Close (root's `close`, the pool a caller input of the
instance's wallet, shruggr/skein#93). The pages are the Open Exchange
(Landing, Swap, Liquidity, Your tokens, Settings), at the origin root by a
root route; their deploy is not yet on the affirmation (docs/AMM.md "Not
wired"). On skein-overlay 0.14.0 and skein-1color 0.10.0
(0.10.0: the 1Color rename, BRC-207's wire names, and the image moved here
from skein, shruggr/skein#152; CHANGELOG.md). Earlier versions are in
CHANGELOG.md. Ported from amm-poc (b-open-io/amm-poc, its `programs/`,
`pool/` and `web/`) in 0.1.0 (shruggr/skein#120).

**Names** (0.10.0, skein-1color 0.10.0; BRC-207's wire names): a token's
topic is `tm_color_<assetId>` (`tm_color_<txid>_0` at output 0), its lookup
`ls_color_<assetId>`, the discovery topic `tm_color` and its lookup
`ls_color_deploys`; the index lookup is `ls_color` (head `<app>/ls_color`);
the programs are `color-topic` and `color-lookup`; the token list is the
read route `/color/tokens`; the 1Color pages are under `<base>/color/`. No
aliases: the names before 0.10.0 (`tm_mandala_*`, `ls_mandala*`,
`mandala-topic`, `mandala-lookup`, `/mandala/tokens`) are not recognised.
The SDK's own names are unchanged until 1sat-sdk renames them: the pages
import the template as `@1sat/templates/mandala` (the class `Mandala`), and
1sat-sdk files a token's outputs in the basket `mandala <txid> <vout>` under
the label `mandala`. The notes in this repo's docs that tell of earlier
versions are written in the current names; CHANGELOG.md keeps them as they
were.

## What it is

| role | file | what |
|---|---|---|
| `overlay` | `bin/overlay.wasm` | the overlay engine, skein-overlay 0.14.0's build (0.14.0: a topic's beat also carries its tip anchor; the reads `/holds`, `/anchors`, `/admitted` are the engine's, not routed by this manifest): serves the topics and lookups, keeps the registered sets (`register` / `deregister`, `registerLookup` / `deregisterLookup`, only in `amm/register`, root's) and, always (a market and a validator; `config.overlay.market` / `.validator` give the ms, absent here: 40 000 / 30 000), asks liveness of and beacons each registered token's `tm_color_<txid>_0-live` (the body its view digest) and each registered lookup's `<service>-live`, takes submissions by message (box `amm/submit`, answered to the sender's box) and by `POST /submit` (BRC-22: the STEAK) |
| `color-topic` | `bin/color-topic.wasm` | skein-1color 0.10.0's topic manager: one topic per token, `tm_color_<assetId>` (`tm_color_<txid>_0`), judged by the BRC-162 rules alone; the discovery topic `tm_color` |
| `color-lookup` | `bin/color-lookup.wasm` | skein-1color 0.10.0's lookups `ls_color` (a token's value and authority outputs, one output), `ls_color_deploys` (a token's deploy output) and each token's `ls_color_<assetId>` (BRC-207), one index; the token list (the read route `/color/tokens`, its filter `tokens`) |
| `amm-lookup` | `programs/amm-lookup` | `ls_amm`: the listed pools (an affirmed deploy's pool and its continuations, unspent) of every token topic the overlay serves, each query naming its token; its beat, the prices |
| `amm-validator` | `programs/amm-validator` | the validator, for every registered token (always): signs a taker's swap last and submits it to its own overlay by message, answering on the engine's answer (`swap`, a libp2p direct call); affirms an LP's proposed pool at its terms (`affirm`, called in-VM by amm-p2p, 0.11.0); root's `close` (box `amm/validator`): the contract's Close on the validator's path, the pool a caller input of the instance's wallet |
| `amm-p2p` | `programs/amm-p2p` | the marketplace relay (`amm.swap/1`, `amm.pool/1` with `terms`: a transaction carried to the validator the caller names), the holders' listing requests (box `amm/requests`), the reads `/requests` and `/spends`, and the pages (`www/`, served from the app's own tree) |
| | `src/pool.zig` | the Pool contract as the overlay sees it: recognising a pool, its state, the pool checks (module `pool`, over the `1color` module's parser) |
| | `pool/` | the Rúnar contract (`Pool.runar.go`) and its Go tests |
| | `gen/` | the fixture generators (`src/fixtures/`) |
| | `web/ui`, `web/engine` | the pages (the Open Exchange: Landing, Swap, Liquidity, Your tokens, Settings) and the matching engine, built into `www/` |
| | `www/color/` | skein-1color 0.10.1's pages: deploy a token (through the wallet's BRC-207 `color` module), root's token topics |
| | `image/` | this app's skein image (shruggr/skein#152): the chain app and this app installed at birth, root's three routes (below, "The image") |

docs/AMM.md has the pieces, the pool rule, the interfaces, the rows and
what is not wired.

## Build

Zig 0.16.0 (`mise.toml`), Go 1.27 for `pool/` and `gen/`, Node 22 or later
and npm for the pages.

```
zig build test     # the pool library and the three programs, natively
zig build bin      # bin/: this repo's three programs, built; overlay.wasm and color-*.wasm, copied
go test ./pool/...
scripts/www.sh     # www/: the pages from web/ui, then the 1Color pages into www/color/
```

**The overlay and 1Color artifacts are fetched, not built here.**
`build.zig.zon` names skein-overlay v0.14.0 and skein-1color v0.10.1 by tag
URL and hash; `zig build bin` copies `bin/overlay.wasm` from the
skein-overlay package and `bin/color-topic.wasm`, `bin/color-lookup.wasm`
from the skein-1color package, byte for byte. skein-1color's pages are
outside its package's `paths`, so `scripts/color-pages.sh` fetches the same
tag's tarball by URL and checks its sha256 before copying its `www/` into
`www/color/`. `bin/` and `www/` are committed: what a skein installs is the
finished tree, and nothing is built on the skein.

The pages need two local checkouts (web/ui/README.md): Rúnar at
`~/Work/bsv/runar` (`RUNAR_DIR`; d207ee8e, the commit go.mod pins) and
1sat-sdk at `~/Work/bsv/1sat-sdk` (`ONESAT_SDK_DIR`), whose
sources are bundled; the committed `www/` (0.10.0, as 0.8.1 to 0.9.1) was built with
`ONESAT_SDK_DIR` at the embedded-icon draft, b-open-io/1sat-sdk#92
(aff025c3, unpublished; web/ui/README.md "The `Mandala` template from the
local 1sat-sdk checkout") and typechecked with the tsconfig paths pointed there (the
default checkout's master, 20947539, has a different icon form and does not
typecheck against these pages), before 0.8.0 with master 183c0ce3. The
template's own imports (`@ipld/dag-cbor`, `cborg`, `cbor2`, `@bsv/sdk`) are
web/ui's dependencies, deduped (vite.config.ts) and mapped (tsconfig.json),
so the checkout needs no `node_modules`. Their tests:

```
cd web/engine && npm ci && npm test
cd web/ui && npm ci && npm test && npm run typecheck
```

Regenerate the fixtures after changing `pool/Pool.runar.go`, from the repo
root: `go run ./gen/vectors` (0.9.0: it also writes the pages' compiled
artifact, `web/ui/src/pool/pool.artifact.json`, and their copy of the
transactions, `web/ui/test/fixtures/amm-topic-vectors.json`;
`gen/addliquidity` is gone with AddLiquidity).

## Use

The base URL of the app is `https://<handle>.<host>/amm` (on a host without
wildcard DNS, `<host>/@<handle>/amm`).

1. **Install the chain app, then the AMM**, as root (skein docs/APPS.md
   §3: the management page, or `skein plan install` and `skein send`):

   ```
   skein plan install https://github.com/shruggr/skein-chain#<commit> --origin <instance> --out chain/ && skein send <instance> chain/
   skein plan install https://github.com/shruggr/skein-amm#<commit> --origin <instance> --out amm/ && skein send <instance> amm/
   ```

2. **Deploy a token** at `<base>/color/deploy/` (anyone, with their own
   wallet). The page shows the token's topic, `tm_color_<txid>_0`, and its
   lookup, `ls_color_<txid>_0`.
3. **Register the token** at `<base>/color/tokens/` (root). The page
   sends root's messages to the app's box `register` (`amm/register`,
   shruggr/skein#128), which the route `{address: "register", handler:
   "overlay.register"}` takes to the engine, gated by `roles: {root:
   ["register", "registerLookup", "deregisterLookup", "market",
   "validator"]}` (shruggr/skein#143: any other key's message is recorded
   and runs nothing): its topic, then its lookup (BRC-207); deregistering
   reverses both:

   ```
   box:  amm/register
   body: {"fn": "register", "args": {"topic": "tm_color_<txid>_0", "program": "color-topic"}}
   body: {"fn": "registerLookup", "args": {"service": "ls_color_<txid>_0", "program": "color-lookup", "topics": ["tm_color_<txid>_0"]}}
   body: {"fn": "deregisterLookup", "args": {"service": "ls_color_<txid>_0"}}
   body: {"fn": "deregister", "args": {"topic": "tm_color_<txid>_0"}}
   ```

   `tm_color` (the discovery topic) is registered by `register` alone.
   The Settings page (0.9.0) sends the same, and registers `ls_amm` too —
   `{"fn": "registerLookup", "args": {"service": "ls_amm", "program":
   "amm-lookup"}}`, idempotent — so the AMM lookup's beat, the prices, is
   declared (skein-overlay 0.12: a registered lookup beats on
   `<service>-live` and keeps liveness on it).
4. **Market and validator, always** (0.8.1, skein-overlay 0.12.0; David
   Case, 2026-10-09: "every skein is marketplace AND validator from
   install, always"): no switch (0.6.2's `{fn: "market" | "validator"}`
   and the Token topics page's two switches are gone). The register above
   is the one act that drives both: the engine's `register` also emits
   `{event: "liveness", topic: "tm_color_<txid>_0-live", window}` — the
   runtime's liveness tool subscribes the topic without admitting its
   messages, verifies each beat and keeps the ones within the window in
   memory, served at `GET <base>/.live/tm_color_<txid>_0-live` →
   `[{sender, at, body, from}]` (404 when no liveness is kept; skein
   docs/MESSAGES.md "Liveness (#138)") — and `{event: "beacon", topic:
   "tm_color_<txid>_0-live", every, body}`: the host's node publishes a
   signed beat every `every` ms — the frame carries the instance's identity
   key (`sender`), the gossip message its peer ID (`from`), the body the
   topic's view digest `{view: {count, digest}}`, re-declared when it
   changes — and amm-validator signs swaps and affirms an LP's proposed
   pool for every registered token, refusing any other `not_validating`. A deregister
   reverses both (`unliveness`, `unbeacon`). The ms are
   `config.overlay.market: {window}` / `config.overlay.validator: {every}`
   (e.g. at install, `--config '{"overlay": {"market": {"window": 40000},
   "validator": {"every": 30000}}}'`), else 40 000 / 30 000; this manifest
   sets neither. `config.amm.ammP2p.market`, `heartbeatSeconds` and
   `offlineSeconds` are gone (`validator.every` is the beat,
   `market.window` the offline threshold), and so are `validate` /
   `unvalidate` (the box `amm/validate`), the validated set and amm-p2p's
   start / stop.

   **The validator named** (0.4.0): the Swap page reads each token's
   `.live` read, takes each beat's `sender` (the validator) and `from` (its
   peer ID), plans only over the pools whose validator beat within the window, and sends
   `amm.swap.submit` with that validator's `peerId` beside its `validator`
   key (a deploy names this skein's own validator, `amm.pool.terms`, and
   asks it `amm.pool.affirm`). The relay looks at no liveness: it dials `peerId`, or, when
   `peerId` is its own node's, calls its own `amm-validator` in-VM with the
   same package, as the front door calls the route's handler for a frame.
5. **The terms** (0.9.0, David Case 2026-10-09: "Fees are the
   validator's"): `config.amm.ammValidator` `{lpFeeBps, validatorFeeBps,
   commissionBps}` (here 30, 5, 0; at install, `--config`). Every pool this
   skein hosts carries them exactly: an LP posting here accepts them, and
   the validator affirms no pool whose fees differ (`fees_unacceptable`).
   The pages read them with `amm.pool.terms`.
6. **The pages** at `<base>/` (and at the origin's root, below), served
   from the app's own tree (`www/`): the Open Exchange — Landing (the
   hosted tokens, their prices from the beats), Swap, Liquidity (deploy a
   position at the terms; close one, `bsvFee` the miner fee left from the
   pool), Your tokens ("Add to this exchange": a listing request), Settings
   (root: register and deregister a token, the requests); the 1Color pages
   at `<base>/color/`. They read `<base>/listTopicManagers`,
   `<base>/color/tokens`, `<base>/.live/<topic>` (the beats),
   `<base>/lookup` (`ls_amm`, `ls_color`), `<base>/requests`,
   `<base>/spends?outpoint=`, and call `<base>/call` (the relay),
   `<base>/submit` (a Close) and `<origin>/sendMessage` (a listing request,
   root's registers). Settings is shown to every connected wallet (`isRoot`
   is a stub: skein has no read of a key's grants); the skein refuses a
   non-root's register messages.

   **At the origin's root** (the landing at `/`, shruggr/skein#147): root
   adds a root route to the same filter (skein-overlay README "Root
   routes"; the pages then take `<origin>/amm` as the app's base, 0.8.1):

   ```
   skein routes add --transport http --prefix --filters amm.page --settings '{"root":"www","index":"index.html"}' / <origin>
   ```

7. **Close by the validator** (0.11.0, in place of 0.9.0's rescind): the
   validator delists a pool by the contract's Close — root's message in
   `amm/validator`:

   ```
   box:  amm/validator
   body: {"fn": "close", "args": {"pool": "<txid>.<vout>"}}
   ```

   `pool` is any of the pool's outputs (its deploy's `<txid>.0` too):
   amm-validator follows it to the newest, has the instance's wallet build
   the Close with the pool as a caller input (createAction with the pool's
   unlocking script to come, the contract's two outputs to the LP key; then
   signAction with the Rúnar call, `sig` the pool's current validator key's,
   bsvFee 0, the wallet paying the fee and keeping its change;
   shruggr/skein#93), submits it to the overlay and answers root `{fn:
   "close", request, result}`. The LP gets every sat and every token, as on
   its own Close; `ls_amm` lists the pool no more.

**Submit and look up** (BRC-22, synchronous: skein-overlay 0.9.1; by message instead, answered later to the submitter's box; BRC-24):

```
POST <base>/submit                     → 200, the STEAK (BRC-22; 503 + Retry-After while undecided)
X-Topics: ["tm_color_<txid>_0"]
Content-Type: application/octet-stream
<BEEF>

POST <base>/lookup
{"service": "ls_amm", "query": {"tokenId": "<txid>"}}                                        (listed pools only, 0.9.0)
{"service": "ls_amm", "query": {"tokenId": "<txid>", "outpoint": "<txid>_0", "beef": true}}
{"service": "ls_amm", "query": {"tokenId": "<txid>", "validatorIdentityKey": "<hex>"}}
{"service": "ls_color", "query": {"tokenId": "<txid>"}}
{"service": "ls_color_deploys", "query": {"tokenId": "<txid>"}}
{"service": "ls_color_<txid>_0", "query": {"type": "color-spendability", "version": 1, "assetId": "<txid>_0", "topic": "tm_color_<txid>_0", "outpoints": ["<txid>.1"]}}
```

At the skein's origin (the @bsv/sdk clients' form), once root adds the two
root routes (skein-overlay README "Root routes"):

```
skein routes add --transport http --filters kernel.beef --fn submit /submit amm.overlay <origin>
skein routes add --transport http --filters amm.lookup /lookup <origin>
```

## The image

`image/` is this app's skein image (shruggr/skein#152: an image carries no
apps; the Open Exchange image moved here from skein's `images/open-exchange`):

```
skein-host add <h> --image <this repo>/image
```

Its `etc/apps.json` installs, at birth, the chain app (shruggr/skein-chain
v0.6.0 by its release tarball, checked by its tree's git id; this app
requires `chain/1`, and the chain app has no start message, so a genesis can
carry it) and then this app (`..`, the repo root), and adds root's three
routes, as the old image did: `POST /submit` (`kernel.beef`, the engine's
`submit`), `/lookup` (the filter `amm.lookup`) and `/` (the pages, the
filter `amm.page`, `www/`, `index.html`). The skein is born unclaimed; its
first claim owns it. Nothing is registered: root registers each token as
above.

On skein main ac20aea, `skein-host add --image <dir>` boots a directory as
a whole system tree (it needs `etc/dispatch.json` and the boundary
programs), so `image/` as committed (only `etc/apps.json`, as
shruggr/skein-collection's `image/`) is refused there with
"etc/dispatch.json: missing"; booted over skein's `images/default` (its
files copied beside this `etc/apps.json`) it makes the genesis above.

## Versions

| | |
|---|---|
| this app | 0.11.0 |
| skein-overlay | v0.14.0 (385e1b9) by tag URL and hash (`build.zig.zon`): the engine in `bin/` (byte-identical to the tag's and to skein-1color 0.10.0's), the modules `topic` (0.13: a verdict's `ancillary_txids`, empty here), `lookup` (0.12: a lookup's beat, `Spec.beat`, `Service.beat`), `sk`, and its engine sources for amm-validator and amm-p2p |
| skein-1color | v0.10.1 (d7348cf; shruggr/skein-mandala until 0.9.2) by tag URL and hash: `bin/color-*.wasm` (the same bytes as 0.10.0's), the module `1color`; its pages by the tag's tarball and sha256 (`scripts/color-pages.sh`) |
| skein-sdk | v0.11.0, through skein-overlay (`files` serves the pages; `sk.peerAt` finds the host's providers) |
| skein | the wallet program's caller inputs (shruggr/skein#93, main 2231398: the validator's Close, createAction / signAction); log format 10, the BEEF envelope beside the pointer record (shruggr/skein#146); routes-roles e3f50fe (format 9: routes, filters, roles, shruggr/skein#143); before, main f45c887 (the liveness tool and `GET /<app>/.live/<topic>`, docs/MESSAGES.md "Liveness (#138)"; the beacon's signed frame, "Beacons"; the node's key from the instance root, signer.ts `peerKey`) |
| requires | `chain/1` (shruggr/skein-chain; the image installs v0.6.0) |
| Rúnar | d207ee8e (go.mod; the pages' runar-sdk) |
| the pages | `@1sat/actions` 0.0.228, `@1sat/connect` 0.0.99, `@1sat/react` 0.0.97, `@1sat/templates` 0.0.39 (the `Mandala` template from b-open-io/1sat-sdk#92, aff025c3, unpublished; before 0.8.0, 183c0ce3), `@bsv/sdk` 2.8.10, `@ipld/dag-cbor` 10.0.2 and `cborg` 6.1.4 for the template (`web/ui/package-lock.json`) |

## Contributing

Work is tracked in shruggr/skein; start at issue
[#31](https://github.com/shruggr/skein/issues/31), this repo's issue is
[#120](https://github.com/shruggr/skein/issues/120). MIT, as skein.
