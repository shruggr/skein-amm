# skein-amm

A non-custodial BSV ↔ token AMM over Mandala tokens (BRC-162), as one
[skein](https://github.com/shruggr/skein) overlay app, name `amm`. Its tree
carries the overlay engine, the Mandala components, the AMM's own programs
and its pages. Version **0.3.1**: on skein-overlay 0.7.7 (skein-sdk 0.7.1)
and skein-mandala 0.5.2; a host serving a market subscribes the beacons
(0.3.0, shruggr/skein#120), and a node beacons only the topics its owner
has set up validation for, per topic (0.3.1, David 2026-10-06). Ported from amm-poc (b-open-io/amm-poc, its
`programs/`, `pool/` and `web/`) in 0.1.0 (shruggr/skein#120).

## What it is

| role | file | what |
|---|---|---|
| `overlay` | `bin/overlay.wasm` | the overlay engine, skein-overlay 0.7.7's build: serves the topics, keeps the registered set (`register` / `deregister`, only in `amm/register`), takes submissions by message and from `POST /submit` (box `amm/submit`) and answers them to the sender's box |
| `mandala-topic` | `bin/mandala-topic.wasm` | skein-mandala 0.5.2's topic manager: one topic per token, `tm_<txid>`, judged by the BRC-162 rules alone; the discovery topic `tm_mandala_deploys` |
| `mandala-lookup` | `bin/mandala-lookup.wasm` | skein-mandala 0.5.2's lookups `ls_mandala` (a token's value and authority outputs, one output) and `ls_mandala_deploys` (a token's deploy output) |
| `amm-lookup` | `programs/amm-lookup` | `ls_amm`: the live pools of every token topic the overlay serves, each query naming its token |
| `amm-validator` | `programs/amm-validator` | the validator: checks a taker's or an LP's transaction against the pool, signs the pool input last, submits it to its own overlay by message and answers on the engine's answer (`swap`, `addLiquidity`, `deploy`, libp2p direct calls) |
| `amm-p2p` | `programs/amm-p2p` | the validator liveness beacon (and, on a market host, the beacons' subscription and the validator map), the marketplace relay (`amm.swap/1`, `amm.pool/1`, `amm.liquidity/1`: a transaction carried to its validator), and the pages (`www/`, served from the app's own tree) |
| | `src/pool.zig` | the Pool contract as the overlay sees it: recognising a pool, its state, the pool checks (module `pool`, over the `mandala` parser) |
| | `pool/` | the Rúnar contract (`Pool.runar.go`) and its Go tests |
| | `gen/` | the fixture generators (`src/fixtures/`) |
| | `web/ui`, `web/engine` | the pages (validator, LP, swap, tokens) and the matching engine, built into `www/` |
| | `www/mandala/` | skein-mandala 0.5.2's pages: deploy a token, the owner's token topics |

docs/AMM.md has the pieces, the pool rule, the interfaces, the rows and
what is not wired.

## Build

Zig 0.16.0 (`mise.toml`), Go 1.27 for `pool/` and `gen/`, Node 22 or later
and npm for the pages.

```
zig build test     # the pool library and the three programs, natively
zig build bin      # bin/: this repo's three programs, built; overlay.wasm and mandala-*.wasm, copied
go test ./pool/...
scripts/www.sh     # www/: the pages from web/ui, then the Mandala pages into www/mandala/
```

**The overlay and Mandala artifacts are fetched, not built here.**
`build.zig.zon` names skein-overlay v0.7.7 and skein-mandala v0.5.2 by tag
URL and hash; `zig build bin` copies `bin/overlay.wasm` from the
skein-overlay package and `bin/mandala-topic.wasm`, `bin/mandala-lookup.wasm`
from the skein-mandala package, byte for byte. skein-mandala's pages are
outside its package's `paths`, so `scripts/mandala-pages.sh` fetches the same
tag's tarball by URL and checks its sha256 before copying its `www/` into
`www/mandala/`. `bin/` and `www/` are committed: what a skein installs is the
finished tree, and nothing is built on the skein.

The pages need two local checkouts (web/ui/README.md): Rúnar at
`~/Work/bsv/runar` (`RUNAR_DIR`; d207ee8e, the commit go.mod pins) and
1sat-sdk at `~/Work/bsv/1sat-sdk` (`ONESAT_SDK_DIR`; master 183c0ce3), whose
sources are bundled. Their tests:

```
cd web/engine && npm ci && npm test
cd web/ui && npm ci && npm test && npm run typecheck
```

Regenerate the fixtures after changing `pool/Pool.runar.go`, from the repo
root: `go run ./gen/vectors`, then `go run ./gen/addliquidity`.

## Use

The base URL of the app is `https://<handle>.<host>/amm` (on a host without
wildcard DNS, `<host>/@<handle>/amm`).

1. **Install the chain app, then the AMM**, as the owner (skein docs/APPS.md
   §3: the management page, or `skein plan install` and `skein send`):

   ```
   skein plan install https://github.com/shruggr/skein-chain#<commit> --origin <instance> --out chain/ && skein send <instance> chain/
   skein plan install https://github.com/shruggr/skein-amm#<commit> --origin <instance> --out amm/ && skein send <instance> amm/
   ```

2. **Deploy a token** at `<base>/mandala/deploy/` (anyone, with their own
   wallet). The page shows the token's topic, `tm_<txid>`.
3. **Register its topic** at `<base>/mandala/tokens/` (the owner). The page
   sends the owner's message to the app's box `register` (`amm/register`,
   shruggr/skein#128), which the row `{address: "register", sender: "$owner",
   program: "overlay"}` takes to the engine:

   ```
   box:  amm/register
   body: {"fn": "register", "args": {"topic": "tm_<txid>", "program": "mandala-topic"}}
   body: {"fn": "deregister", "args": {"topic": "tm_<txid>"}}
   ```

   `tm_mandala_deploys` (the discovery topic) is registered the same way.
4. **Validate topics, per topic** (0.3.1): beaconing is not a role; a node
   beacons `tm_<txid>-live` for the topics it validates. The owner sets that
   up per topic, like a registration, in box `amm/amm-p2p` (the Validator
   page's Validate / Stop validating):

   ```
   box:  amm/amm-p2p
   body: {"fn": "validate", "args": {"topic": "tm_<txid>"}}
   body: {"fn": "unvalidate", "args": {"topic": "tm_<txid>"}}
   ```

   Each is idempotent and answered `{topic, validating}`; `validate` adds the
   topic to the validated set (`validated` in the head `amm/p2p`) and asks the
   host for its beacon, `unvalidate` removes it and ends the beacon. The row
   (`amm-p2p` from `*`) admits anyone; amm-p2p acts only on the owner's
   (`NotTheOwner` errors the step otherwise).

   **Start the beacon**: the manifest's `start`, `{kind: "amm-p2p-start"}`
   in box `amm` (or the Validator page's Start, box `amm/amm-p2p`), reaches
   amm-p2p, which asks the host for one beacon per validated topic; the stop
   ends them and keeps the set, so the next start beacons it again.

   **The market role** (`config.amm.ammP2p.market`, default `false`): a host
   serving a market sets it, and the same start also subscribes each served
   token's `tm_<txid>-live` to amm-p2p's `validateLive` (shruggr/skein#119),
   which keeps the validator map the relay picks from; a later start
   subscribes a newly registered topic and unsubscribes a deregistered one,
   and the stop unsubscribes all. Without it the map stays empty and the relay
   refuses a swap `validator_offline`. The Validator page's Start sends the same
   start; it does not show the role.
5. **The pages** at `<base>/`, served from the app's own tree (`www/`):
   Tokens (the wallet's tokens), Pools (create a pool, add and remove
   liquidity), Swap, Validator (this instance as a validator); the Mandala
   pages at `<base>/mandala/`. They call `<base>/lookup` (`ls_amm`,
   `ls_mandala`), `<base>/live`, `<base>/call` (the relay) and
   `<base>/submit`.

**Submit and look up** (skein-overlay 0.7.2+: a submission is a message, answered later to the submitter's box; BRC-24):

```
POST <base>/submit                     → 200 {id}: delivery only (the request record's CID), no STEAK
X-Topics: ["tm_<txid>"]
Content-Type: application/octet-stream
<BEEF>

POST <base>/lookup
{"service": "ls_amm", "query": {"tokenId": "<txid>"}}
{"service": "ls_amm", "query": {"tokenId": "<txid>", "outpoint": "<txid>_0", "beef": true}}
{"service": "ls_amm", "query": {"tokenId": "<txid>", "validatorIdentityKey": "<hex>"}}
{"service": "ls_mandala", "query": {"tokenId": "<txid>"}}
{"service": "ls_mandala_deploys", "query": {"tokenId": "<txid>"}}
```

## Versions

| | |
|---|---|
| this app | 0.3.1 |
| skein-overlay | v0.7.7 (9e30a64) by tag URL and hash (`build.zig.zon`): the engine in `bin/`, the modules `topic`, `lookup`, `sk`, and its engine sources for amm-validator and amm-p2p |
| skein-mandala | v0.5.2 (cd9ae27) by tag URL and hash: `bin/mandala-*.wasm`, the module `mandala`; its pages by the tag's tarball and sha256 (`scripts/mandala-pages.sh`) |
| skein-sdk | v0.7.1, through skein-overlay (`files` serves the pages; `sk.peerAt` finds the host's providers) |
| skein | main 387e057 (the beacon's signed frame, docs/MESSAGES.md "Beacons"; the node's key from the instance root, signer.ts `peerKey`) |
| requires | `chain/1` (shruggr/skein-chain v0.3.2) |
| Rúnar | d207ee8e (go.mod; the pages' runar-sdk) |
| the pages | `@1sat/actions` 0.0.228, `@1sat/connect` 0.0.99, `@1sat/react` 0.0.97, `@1sat/templates` 0.0.39 (Mandala from 1sat-sdk 183c0ce3), `@bsv/sdk` 2.8.10 (`web/ui/package-lock.json`) |

## Contributing

Work is tracked in shruggr/skein; start at issue
[#31](https://github.com/shruggr/skein/issues/31), this repo's issue is
[#120](https://github.com/shruggr/skein/issues/120). MIT, as skein.
