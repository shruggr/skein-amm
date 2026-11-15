# pool/pair/regtest: the PairPool contract on a real node

On-node tests of `pool/pair/PairPool.runar.go` and `pool/pair/Vault.runar.go`
against a bitcoin-sv regtest node. The suite compiles both contracts from
source with Rúnar, deploys pairs with real transactions, spends them through
Swap, Rescind and Close, mines, and reads the chain back to check the
result. `pool/pair/pair_test.go` covers the same scripts in the go-sdk
interpreter. This suite checks them under the node's consensus rules and
mempool policy.

This is a separate Go module, and every test file has the `integration` build
tag, so the root module's `go test ./...` never builds it.

A pair is two outputs of one transaction, always spent and rewritten
together: output 0 the stateful pool (token A's prefix, the code, the
state), output 1 the keyless vault (token B's prefix, the vault code). A
deploy is one funder transaction with the pool at output 0 and the vault at
output 1. Every spend here has the pool at input 0 and the vault at input 1,
unless a negative case moves them.

The pool's state is fixed-width, 251 bytes: ReserveA and ReserveB (8 bytes
each, little-endian), LpPubKey, ValidatorPubKey and ValidatorIdentity (33
each), Nonce (32), LpFeeAPkh and LpFeeBPkh (20 each), and the validator's
affirmation as its signature's r and s, AffirmationR and AffirmationS (32
each). With no variable-length field the artifact has no `codePartLenSlots`:
the deployed code is the template with its constructor args spliced in and
nothing else. Swap and Rescind, which write a continuation, take Rúnar's
`_codePart` witness; Close does not.

The pool does not carry the vault's code: it carries `VaultCodeHash`
(sha256 of the vault script, a readonly constructor arg), and Swap and
Rescind take the vault code as their last declared argument (after
`allPrevouts`, before Rúnar's `_changePKH`/`_changeAmount`), check it against
the hash and build output 1 from it. Close takes no vault code. The
validator's signature (Swap, Rescind) and the LP's (Close) must be
SIGHASH_ALL|FORKID.

Swap alone also checks its inputs and the deploy: the two asset ids differ
and no fee rate is negative (`requireSwappable`; a pair deployed wrong can
still be rescinded and closed), the next validator key starts 02 or 03,
`userPkh`, `commissionPkh` and `tokenChangePkh` are 20 bytes each (whether
used or not), and `tokenChange` is at most 2^64−1 (BRC-162's largest amount) and written into its output as
a minimal number whatever push it came in. The asset ids are Rúnar's fixed
32-byte type.

## What it proves

- Script validity of both contract inputs, the pool and the vault, under the
  node's consensus rules and standardness policy. The node is configured by
  Rúnar's `integration/regtest.sh`; see the policy list below.
- Every accepted spend's outputs are mined exactly as an independent
  expectation says (value and script, byte for byte). That covers the pool
  continuation (token-A prefix, code, state with both reserves and the
  affirmation carried byte for byte), the vault
  continuation (token-B prefix, vault code), the payout, the fee outputs,
  token change and Rúnar sats change. Each spend consumes the previous pair:
  input 0 spends the previous `txid:0`, input 1 the previous `txid:1`, and
  both UTXOs are gone afterwards.
- Value flow. The pool continuation holds exactly the spent pool's sats, also
  for a pool deployed with 5,000 sats. The vault stays at 1 sat. A funding
  input pays the miner: the contract inputs' sats all go to the contract
  outputs (or, on Close, to the LP's outputs), and the funding input carries
  exactly the fee plus the 1-sat outputs nothing else covers.
- Each negative case is refused by the node with a consensus-level script
  failure (`mandatory-script-verify-flag-failed`). Before the broadcast, the
  go-sdk interpreter has to fail exactly the contract inputs the case names
  (for example, only the vault at input 2), so a harness defect cannot pass
  as a rejection. After the rejection, the pool and vault UTXOs of every pair
  involved are still unspent, and the honest spend of that pair is then
  accepted.
- Every SDK-deployed pool's code equals a build from the artifact template
  done without the SDK: the constructor slots (assetIdA, assetIdB,
  VaultCodeHash, the three fee rates) replaced by the args' pushes, and
  nothing else. A pair deployed from such spliced bytes spends through every
  method at fee rates {0, 0, 0}, {17, 0, 0} and {30, 5, 10}. The SDK's vault
  script equals the vault artifact's script (it has no slots), and the
  suite takes `VaultCodeHash` as sha256 of it.
- The new rules refuse what they are meant to, and only them: a wrong vault
  code fails at the `OP_SHA256 <hash> OP_EQUALVERIFY` compare, and a
  signature of another sighash type fails at the compare of its last byte
  with `0x41`. For Swap and Rescind the interpreter trace also shows the
  signer's own `OP_CHECKSIGVERIFY` succeeding first, so the signature is
  valid and only the type rule refuses it. In Close the type check runs
  before the LP's signature check; there the signature's validity rests on
  the harness verifying it against its type's digest.
- Swap's input and deploy checks refuse what they are meant to, each traced
  to its own check: the 20-byte compares of the three addresses (the
  commission address with and without a commission output), the 02/03 compare
  of the next key's first byte, the `<= 2^64−1` compare of `tokenChange`, the
  compare of the two compiled-in asset ids, and the `LpFeeBps >= 0` compare.
  A pair of one token with itself is still rescinded and closed, and a pair
  with a negative fee rate still closed. A next key of `02` and 32 arbitrary
  bytes is accepted (the type byte is all Swap checks); nobody can sign for
  it, so that pair can then neither Swap nor Rescind, and the LP closes it.

## What it does not prove

- **Token conservation.** The node knows nothing about BRC-162. A token
  output is just a script with a `0x20 <assetId> <amount> OP_2DROP` prefix,
  and the "token inputs" here are ordinary 1-sat outputs carrying a
  token-prefixed P2PKH script, made by a plain wallet transaction. No token
  amount is checked on chain, in either direction: the pair's script sees no
  token input at all. The validator, which signs every Swap, and the overlays
  check them.
- Mainnet policy. `regtest.sh` overrides the node's defaults:
  `acceptnonstdtxn=0` (standard script flags are enforced at mempool entry),
  `minminingtxfee=0.00000001` (1 sat/kB), `maxscriptsizepolicy=0`,
  `maxtxsizepolicy=0`, `maxscriptnumlengthpolicy=0`,
  `maxstackmemoryusagepolicy`/`consensus=100000000`, and Genesis and
  Chronicle active from height 1. A miner or ARC with stricter size, fee or
  validation-time limits could refuse transactions this node takes. The
  suite pays 100 sat/kB by default (`FEE_RATE_SAT_PER_KB`).
- The validator's and the overlays' rules: affirmation checks, key
  derivation, prefix == state on admission, `VaultCodeHash` being the
  canonical vault's.
- The validator's duties the contract cannot enforce. `TestTrustModel_ChangePKHCarriesAnExtraOutput`
  shows one on the node: Rúnar does not check the length of `_changePKH`
  (icellan/runar#213) and serializes the change output as
  `<amount:8> 19 76 a9 14 <_changePKH> 88 ac`, so a `_changePKH` of
  `<20-byte hash> 88 ac <value:8> <varint len> <script minus its last two
  bytes>` commits the transaction to the change output and one more output
  after it. The node accepts and mines that Swap; nothing in its standardness
  policy objects. Only the validator's refusal to sign stops it.
- The 32-byte `nextValidatorPubKey` case builds its continuation with the
  short key in state. That shows the node refuses this spend, not that no
  continuation could pass.
- That a non-minimal `tokenChange` push is written minimally, on this node.
  The script does it (the go-sdk interpreter, under consensus rules, passes
  such a Swap whose change output carries the minimal number), but the node
  refuses the transaction first, on policy:
  `non-mandatory-script-verify-flag (Non-minimally encoded script number)`
  (MINIMALDATA, enforced at mempool entry because `regtest.sh` sets
  `acceptnonstdtxn=0`). `TestStrictTokenChangeNonMinimalPush` keeps that as an
  honest spend refused by policy, then shows the same change pushed minimally
  is accepted and mined as the minimal push. Only a miner that takes
  non-standard transactions would see the script's re-encoding at work.
- That a Close with a stray `_codePart` push (as Close took while the state
  was variable-length) is refused on consensus. It is not: the push stays on
  the stack under the result, which consensus allows after Genesis; the node
  refuses it on policy, `non-mandatory-script-verify-flag (Script did not
  clean its stack)` (CLEANSTACK). `TestCloseWithStrayCodePart` asserts exactly
  that, then the LP's Close without the push is accepted.

## Running

You need Docker and a Rúnar checkout at main 6bd5e29f4 or later (Rúnar v1;
the contract's header says why). `RUNAR_DIR` names it; the default is
`../../../../runar`, the sibling of the skein-amm checkout this directory is
in (from a worktree under `.worktrees/` that path points elsewhere). `run.sh`
checks the checkout contains 6bd5e29f4 and stops with a message if it does
not, rather than compile with an older Rúnar: a sibling checkout that is
not kept up to date is the usual way that happens.

```sh
cd pool/pair/regtest
./run.sh -v                       # starts the skein-pair node if needed, runs everything, leaves it running
./run.sh -run Strict -v
./run.sh --pinned -v              # compile with the Rúnar the root go.mod pins (older than v1) instead of the checkout
FEE_RATE_SAT_PER_KB=1 ./run.sh -v # the node's floor
./run.sh --stop                   # run everything, then stop the node
```

Or by hand:

```sh
<runar>/integration/regtest.sh start skein-pair   # container runar-integration-regtest-skein-pair, RPC 19741
RPC_URL=http://localhost:19741 GOWORK=off go test -tags integration -count=1 -v .
<runar>/integration/regtest.sh stop skein-pair
<runar>/integration/regtest.sh clean skein-pair   # removes <runar>/integration/regtest-data/skein-pair
```

`go.mod` replaces Rúnar with the relative path `../../../../runar/...`, which
only resolves from the main checkout. `run.sh` writes `go.local.mod` with the
replaces pointing at `RUNAR_DIR` and runs with `-modfile=go.local.mod`, so it
also works from a worktree. `go.local.*` and `go.pinned.*` are generated and
ignored by git.

Environment:

| var | default | |
|---|---|---|
| `RUNAR_DIR` | `../../../../runar` | Rúnar checkout (`regtest.sh`, compiler, SDK) |
| `INSTANCE` | `skein-pair` | regtest.sh instance; `default` = the unnamed one on 18332 |
| `RPC_URL` | `http://localhost:19741` | node RPC (user/pass `RPC_USER`/`RPC_PASS`, default bitcoin/bitcoin) |
| `FEE_RATE_SAT_PER_KB` | `100` | miner fee every transaction pays, deploys included |
| `POOL_SOURCE` | `../PairPool.runar.go` | the pool contract to compile |
| `VAULT_SOURCE` | `../Vault.runar.go` | the vault contract to compile |

The tests are independent: each makes its own keys and asset ids, funds a
fresh key from the node wallet, and deploys its own pairs. You can rerun them
against a long-lived node. `TestMain` mines to height 300 on a fresh chain.
The suite serializes its own mining, because two `generatetoaddress` calls
racing on one tip fork the chain. When the run ends, it prints every
transaction it sent, with size, the unlocking-script sizes of the first pool
and vault inputs, fee and the node's verdict, then the accepted/rejected
counts.

## Sizes

At Rúnar main (6bd5e29f4), fees {30, 5, 10}; the fee is at the default
100 sat/kB (at 1 sat/kB: 6, 17, 17, 17 and 7 sats). The
unlock columns are the pool's and the vault's unlocking scripts.

| | tx bytes | pool unlock | vault unlock | fee |
|---|---|---|---|---|
| deploy (pool 4670-byte script, code 4380; vault 553-byte script) | 5436 | | | 544 |
| Swap | 16774 | 10037 | 821 | 1678 |
| Swap with token change and Rúnar sats change | 16881 | 10040 | 821 | 1689 |
| Rescind (with Rúnar change) | 16209 | 9901 | 785 | 1622 |
| Close | 6148 | 4975 | 785 | 616 |

Swap's and Rescind's pool unlock carries `_codePart` (the code), the
preimage (whose scriptCode is the pool script again) and the vault code (518
bytes pushed). Close's carries only the signature, `allPrevouts` and the
preimage.

## The cases

| | test |
|---|---|
| deploy {30, 5, 10} + Close round trip | `TestDeployAndClose` |
| fee pair {30, 5, 10}: A-in, B-in, a Swap with token change and Rúnar sats change, 10 chained swaps alternating direction with validator key rotation, Rescind (with Rúnar change), Close sweeping both tokens to token-prefixed P2PKH | `TestLifecycleFeePair` |
| zero-fee pair {0, 0, 0}: both directions (3 outputs each), Close | `TestZeroFeePair` |
| Close on a fresh pair; Close after swaps without Rescind | `TestCloseFreshPair`, `TestCloseAfterSwapsWithoutRescind` |
| the affirmation a real signature's r and s: deploy, Swap, Rescind, Close, each mined pool script ending in exactly r \|\| s | `TestRealAffirmation` |
| a pool deployed with 5,000 sats keeps them through Swaps and Rescind; Close sweeps them | `TestPairWithMoreSats` |
| Swap refused (the pool fails, the vault passes): payout +1 (both directions), vault one token short, vault replaced by a token-B P2PKH, vault missing, pool/vault outputs swapped, wrong validator key, validator fee missing, extra output, ReserveB in state one more than the vault's prefix, 32-byte next key | `TestSwapRejected` |
| Swap refused: continuation 1 sat short of a 5,000-sat pool, the sat routed to Rúnar change | `TestSwapRejected_PoolSatSkimmed` |
| pairing, for each of Swap, Rescind and Close: no vault input (pool fails); a foreign pair's vault at input 1 (both fail); its vault at input 2 (both fail); the pool at input 1, the vault at 0 (both fail) | `TestPairingRejected` |
| vault theft: the vault alone; after a plain funding input; another pair's pool honestly Closed at 0 with its vault at 1 and the victim vault at 2 (only the victim vault fails); another pair's pool Closed with the victim vault at 1 in place of its own (both fail); the victim's honest Close with the vault's preimage ALL\|ANYONECANPAY (only the vault fails) | `TestVaultTheftRejected` |
| Close refused: validator-signed, random key | `TestCloseRejected` |
| Rescind refused: LP-signed; second Rescind; Swap after Rescind | `TestRescindRejected` |
| wrong `vaultCode` for Swap and for Rescind (the vault's code with its last byte flipped; a stranger's P2PKH), each with output 1 built from it and with output 1 the real vault (the pool fails) | `TestVaultCodeArgumentRejected` |
| Swap, Rescind and Close each signed with a valid NONE\|FORKID and a valid ALL\|ANYONECANPAY\|FORKID signature (the pool fails at the `0x41` compare; the trace is checked) | `TestSignerSighashTypeRejected` |
| Swap refused by its input checks (the pool fails): `userPkh` of 19 and of 21 bytes (payout written as the contract would); `commissionPkh` of 19 bytes with a commission and on a zero-fee pair without one; `tokenChangePkh` of 21 bytes with a token change; next key of 33 zero bytes, `04` + 32 bytes, `07` + 32 bytes; `tokenChange` 2^64 with its output; `tokenChange` 17 pushed as `02 11 00` with the output carrying that push | `TestStrictSwapRejected` |
| `tokenChange` 17 pushed as `02 11 00`, the output minimal: refused by the node's policy only (both inputs pass on consensus rules); then pushed minimally: accepted, mined as `01 11` | `TestStrictTokenChangeNonMinimalPush` |
| next key `02` + 32 arbitrary bytes accepted; then a Swap and a Rescind refused; Close accepted | `TestStrictNextKeyTypeByteOnly` |
| a pair with `AssetIdA == AssetIdB`: Swap refused both ways; Rescind and Close accepted | `TestStrictSameToken` |
| a pair with fee rates {−30, 0, 0}: Swap refused (outputs as the script's arithmetic gives: no fee output, net 40,119 for 40,000 in); Close accepted | `TestStrictNegativeFeeRate` |
| `tokenChange` 2^63 and 2^64−1 (BRC-162's largest amount), pushed minimally: accepted, the change output mined with that push | `TestStrictTokenChangeLargeAmounts` |
| a continuation with AffirmationR or AffirmationS one byte off (Swap; Rescind): refused (the pool fails) | `TestAffirmationCarriedRejected` |
| a Close with a stray `_codePart` push: both inputs pass on consensus rules, the node refuses it on policy (CLEANSTACK); the Close without it accepted | `TestCloseWithStrayCodePart` |
| trust model: a 54-byte `_changePKH` carrying an extra output after the change: accepted and mined (the validator must refuse to sign it) | `TestTrustModel_ChangePKHCarriesAnExtraOutput` |
| the template with its constructor args spliced, without the SDK, at fees {0, 0, 0}, {17, 0, 0}, {30, 5, 10}: the SDK's bytes, and it spends through every method | `TestSplicedTemplateEqualsSDK` |
