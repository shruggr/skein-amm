# pool/regtest: the Pool contract on a real node

On-node tests of `pool/Pool.runar.go` against a bitcoin-sv regtest node. The
suite compiles the contract from source with Rúnar, deploys pools with real
transactions, spends them through Swap, Rescind and Close, mines, and reads
the chain back to check the result. `pool/pool_test.go` covers the same
scripts in the go-sdk interpreter. This suite checks them under the node's
consensus rules and mempool policy.

This is a separate Go module, and every test file has the `integration` build
tag, so the root module's `go test ./...` never builds it.

## The pool on chain

```
0x20 <assetId:32> <push tokenReserve> OP_2DROP || code || OP_RETURN || state
```

The state is 243 bytes, and every field is fixed-width:

| field | bytes |
|---|---|
| TokenReserve (LE) | 8 |
| LpPubKey, ValidatorPubKey, ValidatorIdentity | 33 each |
| Nonce | 32 |
| LpSatsPkh, LpTokenPkh | 20 each |
| AffirmationR, AffirmationS | 32 each |

The code is the artifact template with its 13 constructor slots filled: the
asset id and the three fee rates. The artifact has no `codeSepIndexSlots` and
no `codePartLenSlots`, on either compiler.

Unlocking scripts:

- **Swap:** `_codePart`, the signature, 7 args, `allPrevouts`,
  `_changePKH`, `_changeAmount`, `txPreimage`, method index 0.
- **Rescind:** `_codePart`, the signature, `allPrevouts`, `_changePKH`,
  `_changeAmount`, `txPreimage`, method index 2.
- **Close:** the LP's signature, `txPreimage`, method index 1. It takes no
  `_codePart`.

`allPrevouts` is the transaction's input outpoints, 36 bytes each, in order.

## What it proves

- Script validity under the node's consensus rules and standardness policy.
  The node is configured by Rúnar's `integration/regtest.sh`; see the policy
  list below.
- Every accepted spend's outputs are mined exactly as an independent
  expectation says (value and script, byte for byte). That covers the pool
  continuation (token prefix, code, state), payouts, fee outputs, token
  change and Rúnar sats change. Each spend consumes the previous
  continuation, and that UTXO is gone afterwards.
- AffirmationR and AffirmationS from a real ECDSA signature are carried byte
  for byte through Swap in both directions and through Rescind.
- Value flow. On sats-in the pool gains exactly `net`. On token-in it loses
  exactly `out`, and the payout is `out`. A funding input pays the miner
  (Close pays its fee from the swept sats). Rescind leaves the pool's sats
  unchanged.
- Each negative case is refused by the node with a consensus-level script
  failure (`mandatory-script-verify-flag-failed`). Before the broadcast, the
  go-sdk interpreter must fail exactly the expected pool input, and the test
  log names the opcodes it stopped at. Afterwards the pool UTXO is unspent,
  and the honest spend of that same UTXO is accepted.
- The template spliced without the SDK (constructor slots only) is byte for
  byte the SDK's code, for fee sets {0,0,0}, {17,0,0} and {30,5,10}, and it
  spends.

## What it does not prove

- **Token conservation.** The node knows nothing about BRC-162. A token
  output is just a script with a `0x20 <assetId> <amount> OP_2DROP` prefix,
  and the "token inputs" here are ordinary 1-sat outputs carrying a
  token-prefixed P2PKH script, made by a plain wallet transaction. No token
  amount is checked on chain. The validator, which signs every spend that
  brings tokens in, and the overlays check them.
- **The validator's rules.** A Swap whose `_changePKH` is longer than 20
  bytes and carries an extra output behind the change is accepted by the
  script and by this node (`TestTrustModelLongChangePKH`, icellan/runar#213).
  Refusing it is the validator's job. So are affirmation checks, key
  derivation, and prefix == state on admission.
- Mainnet policy. `regtest.sh` overrides the node's defaults:
  `acceptnonstdtxn=0` (standard script flags are enforced at mempool entry),
  `minminingtxfee=0.00000001` (1 sat/kB), `maxscriptsizepolicy=0`,
  `maxtxsizepolicy=0`, `maxscriptnumlengthpolicy=0`,
  `maxstackmemoryusagepolicy`/`consensus=100000000`, and Genesis and
  Chronicle active from height 1. A miner or ARC with stricter size, fee or
  validation-time limits could refuse transactions this node takes. The
  suite pays 100 sat/kB by default (`FEE_RATE_SAT_PER_KB`).

## Running

You need Docker and a Rúnar checkout that contains commit `6bd5e29f4`.
`run.sh` refuses a checkout without it, so it cannot build against a stale
clone. The default checkout is `../../../runar`, the directory next to the
skein-amm checkout. If that one is not current, set `RUNAR_DIR`.

```sh
cd pool/regtest
RUNAR_DIR=/path/to/runar ./run.sh -v          # starts the skein-amm node if needed, runs everything, leaves it running
RUNAR_DIR=/path/to/runar ./run.sh -run Strict -v
RUNAR_DIR=/path/to/runar ./run.sh --pinned -v # compile with the Rúnar the root go.mod pins instead of the checkout
RUNAR_DIR=/path/to/runar ./run.sh --stop -run XXX   # stop the node when done
/path/to/runar/integration/regtest.sh clean skein-amm   # and remove its chain data
```

Or by hand:

```sh
<runar>/integration/regtest.sh start skein-amm    # container runar-integration-regtest-skein-amm, RPC 19246
RPC_URL=http://localhost:19246 GOWORK=off go test -tags integration -count=1 -v .
```

`go.mod` replaces Rúnar with the relative path `../../../runar/...`. `run.sh`
writes `go.local.mod` with the replaces pointing at `RUNAR_DIR` and runs with
`-modfile=go.local.mod`, so it works from a worktree and with any checkout.
`go.local.*` and `go.pinned.*` are generated and ignored by git.

Environment:

| var | default | |
|---|---|---|
| `RUNAR_DIR` | `../../../runar` | Rúnar checkout (`regtest.sh`, compiler, SDK); must contain `6bd5e29f4` |
| `INSTANCE` | `skein-amm` | regtest.sh instance; `default` = the unnamed one on 18332 |
| `RPC_URL` | `http://localhost:19246` | node RPC (user/pass `RPC_USER`/`RPC_PASS`, default bitcoin/bitcoin) |
| `FEE_RATE_SAT_PER_KB` | `100` | miner fee every spend pays (deploys and funding transactions pay a flat 500 sats) |
| `POOL_SOURCE` | `../Pool.runar.go` | the contract to compile |

The tests are independent: each makes its own keys, funds a fresh key from
the node wallet, and deploys its own pools. You can rerun them against a
long-lived node. `TestMain` mines to height 300 on a fresh chain. The suite
serializes its own mining, because two `generatetoaddress` calls racing on one
tip fork the chain. When the run ends, it prints every transaction it sent,
with size, unlocking-script size, fee and the node's verdict.

## The cases

### Accepted

| | test |
|---|---|
| deploy + Close round trip | `TestDeployAndClose` |
| fee pool {30, 5, 10}: sats-in, token-in with token input and token change, Rúnar sats change, 10 chained swaps alternating direction with key rotation, Rescind, Close | `TestLifecycleFeePool` |
| zero-fee pool {0, 0, 0}: both directions, Close | `TestZeroFeePool` |
| Close on a fresh pool; Close after swaps without Rescind | `TestCloseFreshPool`, `TestCloseAfterSwapsWithoutRescind` |
| real affirmation r/s carried through Swap (both ways) and Rescind | `TestAffirmationCarried` |
| template splice equals the SDK's code and spends: {0,0,0}, {17,0,0}, {30,5,10} | `TestTemplateSpliceEqualsSDK` |
| next key `02` + 32 arbitrary bytes accepted; then Swap and Rescind refused (no one holds that key), Close accepted | `TestStrictNextKeyTypeByteOnly` |
| tokenChange 2^63 (a nine-byte push) with its output | `TestStrictTokenChange2Pow63` |
| `_changePKH` of 54 bytes carrying an extra output behind the change (the trust-model case: accepted) | `TestTrustModelLongChangePKH` |

### Refused

| | test |
|---|---|
| Swap: payout +1, continuation -1 sat, wrong signer, validator fee missing, extra output, LP fee to the LP key, prefix/state token mismatch, 32-byte next key, AffirmationR or AffirmationS changed by one byte in the continuation | `TestSwapRejected` |
| Swap: `userPkh` 19 and 21 bytes, `commissionPkh` 19 bytes with and without a commission, `tokenChangePkh` 21 bytes, next key of 33 zero bytes, `04`+32, `07`+32, tokenChange 2^64 with its output | `TestStrictSwapRejected` |
| Swap on a pool deployed with LpFeeBps -30, with the outputs the script's arithmetic gives; Close accepted | `TestStrictNegativeFeeRate` |
| tokenChange 17 pushed as `02 11 00`, its output minimal: refused by **policy** (MINIMALDATA, `non-mandatory-script-verify-flag`), though consensus rules pass it; the minimal push is accepted | `TestStrictTokenChangeNonMinimalPush` |
| Close: validator-signed, random key | `TestCloseRejected` |
| Rescind: LP-signed, key kept; second Rescind; Swap after Rescind | `TestRescindRejected` |
| each method signed with a valid NONE\|FORKID and a valid ALL\|ANYONECANPAY\|FORKID signature | `TestSighashTypeRejected` |
| Swap and Rescind with the pool at input 1 (Close at input 1 accepted) | `TestPoolAtInputIndex1` |
| Swap and Rescind with `allPrevouts` one byte off, and truncated | `TestAllPrevoutsRejected` |
| two byte-identical pools in one transaction, both Swapping (and both Rescinding) against one output list, the second pool's sats leaving through the change: input 0 passes, input 1 fails | `TestTwinPoolsRejected` |

For the 32-byte next key, the continuation carries the short key in state.
That shows the node refuses this spend, not that no continuation could pass.

## Sizes

Measured at Rúnar `6bd5e29f4`, at 100 sat/kB:

| tx | bytes | unlocking script | fee |
|---|---|---|---|
| deploy | 4,382–4,386 | – | 500 (flat) |
| Swap, sats-in | 12,939–13,084 | ~8,480 | ~1,305 |
| Swap, token-in | 13,086–13,378 | ~8,516 | ~1,328 |
| Rescind | 12,765–12,807 | ~8,377 | ~1,278 |
| Close | 4,534–4,538 | ~4,378 | ~455 |

The pool script is 4,198 bytes (code 3,916). With the pinned Rúnar it is
4,158 bytes (code 3,876), and each transaction is about 100 bytes smaller.
The table was measured with a pool script 15 bytes shorter.
At 1 sat/kB every spend is also accepted (Swap 14 sats, Close 5).
