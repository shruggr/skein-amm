# pool/regtest: the Pool contract on a real node

On-node tests of `pool/Pool.runar.go` against a bitcoin-sv regtest node. The
suite compiles the contract from source with Rúnar, deploys pools with real
transactions, spends them through Swap, Rescind and Close, mines, and reads
the chain back to check the result. `pool/pool_test.go` covers the same
scripts in the go-sdk interpreter. This suite checks them under the node's
consensus rules and mempool policy.

This is a separate Go module, and every test file has the `integration` build
tag, so the root module's `go test ./...` never builds it.

## What it proves

- Script validity under the node's consensus rules and standardness policy.
  The node is configured by Rúnar's `integration/regtest.sh`; see the policy
  list below.
- Every accepted spend's outputs are mined exactly as an independent
  expectation says (value and script, byte for byte). That covers the pool
  continuation (token prefix, code, state), payouts, fee outputs, token
  change and Rúnar sats change. Each spend consumes the previous
  continuation, and that UTXO is gone afterwards.
- Value flow. On sats-in the pool gains exactly `net`. On token-in it loses
  exactly `out`, and the payout is `out`. A funding input pays the miner
  (Close pays its fee from the swept sats). Rescind leaves the pool's sats
  unchanged.
- Each negative case is refused by the node with a consensus-level script
  failure (`mandatory-script-verify-flag-failed`). The pool UTXO is unspent
  afterwards, and the honest spend of that same UTXO is then accepted.
- Every SDK-deployed pool's code equals a build from the artifact template
  done without the SDK: constructor slots spliced, codeSepIndex slots set to
  `OP_1`, and the `codePartLenSlots` fields set to the deployed length.

## What it does not prove

- **Token conservation.** The node knows nothing about BRC-162. A token
  output is just a script with a `0x20 <assetId> <amount> OP_2DROP` prefix,
  and the "token inputs" here are ordinary 1-sat outputs carrying a
  token-prefixed P2PKH script, made by a plain wallet transaction. No token
  amount is checked on chain. The validator, which signs every spend that
  brings tokens in, and the overlays check them.
- Mainnet policy. `regtest.sh` overrides the node's defaults:
  `acceptnonstdtxn=0` (standard script flags are enforced at mempool entry),
  `minminingtxfee=0.00000001` (1 sat/kB), `maxscriptsizepolicy=0`,
  `maxtxsizepolicy=0`, `maxscriptnumlengthpolicy=0`,
  `maxstackmemoryusagepolicy`/`consensus=100000000`, and Genesis and
  Chronicle active from height 1. A miner or ARC with stricter size, fee or
  validation-time limits could refuse transactions this node takes. The
  suite pays 100 sat/kB by default (`FEE_RATE_SAT_PER_KB`).
- The validator's and the overlays' rules: affirmation checks, key
  derivation, prefix == state on admission.

## Running

You need Docker and a Rúnar checkout. By default the checkout is the sibling
`../../../runar` (next to the main skein-amm checkout); otherwise set
`RUNAR_DIR`.

```sh
cd pool/regtest
./run.sh -v                  # starts the skein-amm node if needed, runs everything, leaves it running
./run.sh -run HandSpliced -v
./run.sh --pinned -v         # compile with the Rúnar the root go.mod pins instead of the checkout
./run.sh --stop              # stop the node when done
```

Or by hand:

```sh
<runar>/integration/regtest.sh start skein-amm    # container runar-integration-regtest-skein-amm, RPC 19246
RPC_URL=http://localhost:19246 GOWORK=off go test -tags integration -count=1 -v .
```

`go.mod` replaces Rúnar with the relative path `../../../runar/...`, which
only resolves from the main checkout. `run.sh` writes `go.local.mod` with the
replaces pointing at `RUNAR_DIR` and runs with `-modfile=go.local.mod`, so it
also works from a worktree. `go.local.*` and `go.pinned.*` are generated and
ignored by git.

Environment:

| var | default | |
|---|---|---|
| `RUNAR_DIR` | `../../../runar` | Rúnar checkout (`regtest.sh`, compiler, SDK) |
| `INSTANCE` | `skein-amm` | regtest.sh instance; `default` = the unnamed one on 18332 |
| `RPC_URL` | `http://localhost:19246` | node RPC (user/pass `RPC_USER`/`RPC_PASS`, default bitcoin/bitcoin) |
| `FEE_RATE_SAT_PER_KB` | `100` | miner fee every spend pays |
| `POOL_SOURCE` | `../Pool.runar.go` | the contract to compile |

The tests are independent: each makes its own keys, funds a fresh key from
the node wallet, and deploys its own pools. You can rerun them against a
long-lived node. `TestMain` mines to height 300 on a fresh chain. The suite
serializes its own mining, because two `generatetoaddress` calls racing on one
tip fork the chain.
When the run ends, it prints every transaction it sent, with size,
unlocking-script size, fee and the node's verdict.

## The cases

| | test |
|---|---|
| deploy + Close round trip | `TestDeployAndClose` |
| fee pool {30, 5, 10}: sats-in, token-in with token input and token change, Rúnar sats change, 10 chained swaps alternating direction with key rotation, Rescind, Close | `TestLifecycleFeePool` |
| zero-fee pool {0, 0, 0}: both directions, Close | `TestZeroFeePool` |
| Close on a fresh pool; Close after swaps without Rescind | `TestCloseFreshPool`, `TestCloseAfterSwapsWithoutRescind` |
| pool at input index 1: Swap both ways, Rescind, Close | `TestPoolAtInputIndex1` |
| affirmation 0, 71, 72 bytes: deploy, Swap, Rescind, Close | `TestAffirmationSizes` |
| Swap refused: payout +1, continuation -1 sat, wrong signer, validator fee missing, extra output, LP fee to the LP key, prefix/state token mismatch, 32-byte next key (its continuation carries the short key in state; this shows the node refuses that spend, not that no continuation could pass) | `TestSwapRejected` |
| Close refused: validator-signed, random key | `TestCloseRejected` |
| Rescind refused: LP-signed, key kept; second Rescind; Swap after Rescind | `TestRescindRejected` |
| hand-spliced template (fund lock) | `TestHandSplicedTemplate_FundLock` |

### The hand-spliced template

The project carries the artifact template outside the SDK.
`gen/vectors`' `writeArtifact` writes it to `src/fixtures/pool_artifact.zig`
with the codeSepIndex slots set to `OP_1`, and `src/pool.zig`'s `parse`
matches locks against it segment by segment around the constructor slots.
(`gen/vectors` builds its own pool code through the SDK.) Both predate Rúnar's
`codePartLenSlots` (N-051). At Rúnar HEAD, Pool's code has three
`verify_code_part_len` fields (`04 <len:4 LE> OP_BIN2NUM OP_NUMEQUAL
OP_VERIFY`). The template holds a compile-time lower bound in them (the
template's own length), and only the SDK writes the deployed length. The
checked-in fixture is still the pinned Rúnar's 3773-byte template.

`TestHandSplicedTemplate_FundLock` builds pool code from the template the way
a non-SDK builder would: constructor slots spliced, codeSepIndex slots set to
`OP_1`, length fields left alone. It deploys one pool per method, under fee
sets {30, 5, 10} and {0, 0, 0}, and tries two spends on each:

- the honest spend;
- `_codePart` truncated to the pinned length. Rúnar authenticates
  `_codePart` as `61ab || scriptCode[0:SIZE-2]`, so at the pinned SIZE this
  is the only `_codePart` that can pass.

If the node refuses both for Swap, Rescind and Close, the deposit cannot be
recovered: the test passes and logs `FUNDS LOCKED`. If any of them is
accepted, the test fails. The last subtest writes the real code length into
the three fields, checks the result equals the SDK's bytes exactly, and spends
that pool through Swap, Rescind and Close.

With a Rúnar whose artifact has no `codePartLenSlots` (the pinned one), the
test instead asserts the hand splice equals the SDK's code, and that it
spends.
