# skein-amm

A non-custodial BSV ↔ token AMM over Mandala tokens (BRC-162), as one
[skein](https://github.com/shruggr/skein) overlay app, name `amm`. Its tree
carries the overlay engine, the Mandala components, the AMM's own programs
and its pages. Version **0.1.0**. Ported from amm-poc (b-open-io/amm-poc, its
`programs/`, `pool/` and `web/`) onto skein-overlay 0.6.0 and skein-mandala
0.4.0 (shruggr/skein#120).

## What it is

| role | file | what |
|---|---|---|
| `overlay` | `bin/overlay.wasm` | the overlay engine, skein-overlay 0.6.0's build: serves the topics, keeps the registered set (`register` / `deregister`) |
| `mandala-topic` | `bin/mandala-topic.wasm` | skein-mandala 0.4.0's topic manager: one topic per token, `tm_<txid>`, judged by the BRC-162 rules alone; the discovery topic `tm_mandala_deploys` |
| `mandala-lookup` | `bin/mandala-lookup.wasm` | skein-mandala 0.4.0's lookups `ls_mandala` (a token's value and authority outputs, one output) and `ls_mandala_deploys` (a token's deploy output) |
| `amm-lookup` | `programs/amm-lookup` | `ls_amm`: the live pools of every token topic the overlay serves, each query naming its token |
| `amm-validator` | `programs/amm-validator` | the validator: checks a taker's or an LP's transaction against the pool, signs the pool input last, submits it to its own overlay (`swap`, `addLiquidity`, `deploy`, libp2p direct calls) |
| `amm-p2p` | `programs/amm-p2p` | validator liveness, proofs by block, and the marketplace relay (`amm.swap/1`, `amm.pool/1`, `amm.liquidity/1`: a transaction carried to its validator) |
| | `src/pool.zig` | the Pool contract as the overlay sees it: recognising a pool, its state, the pool checks (module `pool`, over the `mandala` parser) |
| | `pool/` | the Rúnar contract (`Pool.runar.go`) and its Go tests |
| | `gen/` | the fixture generators (`src/fixtures/`) |
| | `web/ui`, `web/engine` | the pages (validator, LP, swap, tokens) and the matching engine, built into `www/` |
| | `www/mandala/` | skein-mandala 0.4.0's pages: deploy a token, the owner's token topics |

docs/AMM.md has the pieces, the pool rule, the interfaces and what is not
built.

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
`build.zig.zon` names skein-overlay v0.6.0 and skein-mandala v0.4.0 by tag
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
   sends the owner's message to box `amm`, which the row `{address: "amm",
   sender: "$owner", program: "overlay"}` takes to the engine:

   ```
   box:  amm
   body: {"fn": "register", "args": {"topic": "tm_<txid>", "program": "mandala-topic"}}
   body: {"fn": "deregister", "args": {"topic": "tm_<txid>"}}
   ```

   `tm_mandala_deploys` (the discovery topic) is registered the same way.
4. **Start amm-p2p**: the owner sends `{kind: "amm-p2p-start"}` to box
   `amm-p2p` (the Validator page's Start button). The manifest's `start`
   goes into box `amm`, where the owner's row is the engine's (docs/AMM.md
   "The rows"), so it does not reach amm-p2p.
5. **The pages** at `<base>/`: Tokens (the wallet's tokens), Pools (create a
   pool, add and remove liquidity), Swap, Validator (this instance as a
   validator). They call `<base>/lookup` (`ls_amm`, `ls_mandala`),
   `<base>/live` and `<base>/call` (the relay). Serving `www/` from the app's
   own tree is not built (docs/AMM.md "Not built"): until it is, the pages run
   from a dev server pointed at the base URL (web/ui/README.md).

**Submit and look up** (BRC-22, BRC-24):

```
POST <base>/submit
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
| this app | 0.1.0 |
| skein-overlay | v0.6.0 by tag URL and hash (`build.zig.zon`): the engine in `bin/`, the modules `topic`, `lookup`, `sk`, and its engine sources for amm-validator and amm-p2p |
| skein-mandala | v0.4.0 by tag URL and hash: `bin/mandala-*.wasm`, the module `mandala`; its pages by the tag's tarball and sha256 (`scripts/mandala-pages.sh`) |
| skein-sdk | v0.5.1, through skein-overlay |
| skein | main 15852f4 (`checkManifest` accepts `etc/app.json`) |
| requires | `chain/1` (shruggr/skein-chain) |
| Rúnar | d207ee8e (go.mod; the pages' runar-sdk) |
| the pages | `@1sat/actions` 0.0.228, `@1sat/connect` 0.0.99, `@1sat/react` 0.0.97, `@1sat/templates` 0.0.39 (Mandala from 1sat-sdk 183c0ce3), `@bsv/sdk` 2.8.10 (`web/ui/package-lock.json`) |

## Contributing

Work is tracked in shruggr/skein; start at issue
[#31](https://github.com/shruggr/skein/issues/31), this repo's issue is
[#120](https://github.com/shruggr/skein/issues/120). MIT, as skein.
