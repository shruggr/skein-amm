# Changelog

Released versions of skein-amm. 0.10.0 is the first entry here; the notes
on earlier versions are below as the README carried them at 0.9.1, in the
names of their time, and each release commit names what changed.

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
