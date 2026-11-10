// The token app end to end on a real kernel (skein-amm 0.49.0): ONE token instance born from an image
// that installs, at birth and with their approvals, the chain app (skein-chain 0.10.0), the signer app
// (skein-signer 0.1.0), the wallet app (skein-wallet 0.1.0, as `wallet`) and this app (`amm`, its
// manifest's `calls` granted in the genesis). The instance's identity is the fixtures' validator
// identity (gen/vectors: key 0x7f…), so the validator fees of the fixtures' Swaps are ours.
//
//   1. the deploy event (amm-setup): the token's topic and lookups registered, the deploy seeded;
//   2. pool_deploy, swap_bsv_in (a buy), swap_tokens_in (a sell) submitted (POST /submit, the fixtures as
//      signed by gen/vectors);
//   3. the recognizer's events, as the KERNEL indexed them (GET /@<h>/events/…): `amm/pool/<poolId>`,
//      `amm/output/<op>`, `amm/hop` — so a record the kernel refuses to emit (an event's `kind` / `app`)
//      fails here;
//   4. the validator fee outputs filed in the wallet app (its reads /wallet/outputs, /wallet/balance):
//      the buy's under "bsv", the sell's under "tokens";
//   5. the token fee tally (GET /@<h>/amm/fees/tokens);
//   6. the store replays to itself exactly.
//
//   SKEIN_DIR=<skein checkout at 0.7.0 (440e704), node_modules, its kernel built: kernel-zig zig-out/web (the wasm host) and zig-out/bin (the replay)>
//   CHAIN_DIR=<skein-chain v0.10.0>  SIGNER_DIR=<skein-signer v0.1.0>  WALLET_DIR=<skein-wallet v0.1.0>
//   SKEIN_TEST_STORE=sqlite  node --experimental-strip-types --no-warnings scripts/e2e/token-instance.ts
//
// (the app trees as their tags carry them: `git archive <tag> | tar x -C <dir>`; this repo's bin/ built: `zig build bin`).

import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const AMM = join(here, "../..");
const env = (k: string) => { const v = process.env[k]; if (!v) throw new Error(`set ${k}`); return v; };
const SKEIN = env("SKEIN_DIR"), CHAIN_DIR = env("CHAIN_DIR"), SIGNER_DIR = env("SIGNER_DIR"), WALLET_DIR = env("WALLET_DIR");
const imp = async <T>(p: string): Promise<T> => await import(pathToFileURL(join(SKEIN, p)).href) as T;
/** A package's ESM entry as skein's own modules import it (its exports' `import`), so the classes are the same. */
const esm = (pkg: string) => {
  const pj = join(SKEIN, "node_modules", pkg, "package.json");
  const x = (JSON.parse(readFileSync(pj, "utf8")) as { exports: Record<string, { import: string | { default: string } }> }).exports["."]!.import;
  return pathToFileURL(join(dirname(pj), typeof x === "string" ? x : x.default)).href;
};
const { MerklePath, PrivateKey, Transaction } = await import(esm("@bsv/sdk")) as typeof import("@bsv/sdk");
type Tx = InstanceType<typeof Transaction>;
const { FakeArcade } = await imp<{ FakeArcade: { start(): Promise<{ url: string; eventsUrl: string; close(): Promise<void> }> } }>("src/host/fake-arcade.ts");
const { testHost, until } = await imp<{ testHost: (...a: unknown[]) => Promise<any>; until: <T>(w: string, f: () => Promise<T | undefined>, ms?: number) => Promise<T> }>("src/host/testhost.ts");
const { storeSpec } = await imp<{ storeSpec: (row: unknown) => string }>("src/runtime/index-store.ts");

let failures = 0;
const check = (ok: boolean, what: string) => { process.stdout.write(`${ok ? "ok  " : "FAIL"} ${what}\n`); if (!ok) failures++; };

// ---------------------------------------------------------------- the fixtures (src/fixtures/vectors.zig)
const vectors = readFileSync(join(AMM, "src/fixtures/vectors.zig"), "utf8");
const vec = (name: string) => { const m = new RegExp(`pub const ${name} = "([0-9a-f]*)";`).exec(vectors); if (!m) throw new Error(`no vector ${name}`); return m[1]!; };
const names = ["fund", "token_deploy", "pool_deploy", "swap_bsv_in", "swap_tokens_in"] as const;
const txs = new Map<string, Tx>();
for (const n of names) txs.set(n, Transaction.fromHex(vec(n)));
const byId = new Map<string, Tx>([...txs.values()].map((t) => [t.id("hex"), t]));
// Each input's source linked (an Atomic BEEF carries the ancestry), back to a root: a transaction spending one no fixture is.
for (const t of txs.values()) for (const i of t.inputs) i.sourceTransaction = byId.get(i.sourceTXID ?? "") ?? i.sourceTransaction;
const roots = [...txs.values()].filter((t) => t.inputs.some((i) => !byId.has(i.sourceTXID ?? "")));

// ---------------------------------------------------------------- regtest: each root mined alone
const sha256d = (b: Uint8Array) => createHash("sha256").update(createHash("sha256").update(b).digest()).digest();
const internal = (displayHex: string) => Buffer.from(displayHex, "hex").reverse();
const REGTEST_GENESIS = Buffer.from("0100000000000000000000000000000000000000000000000000000000000000000000003ba3edfd7a7b12b27ac72c3e67768f617fc81bc3888a51323a9fb8aa4b1e5e4adae5494dffff7f2002000000", "hex");
const TARGET = 0x7fffffn << BigInt(8 * (0x20 - 3));
function mine(prev: Uint8Array, merkleRoot: Uint8Array, time: number): Uint8Array {
  const h = Buffer.alloc(80);
  h.writeInt32LE(1, 0);
  Buffer.from(prev).copy(h, 4);
  Buffer.from(merkleRoot).copy(h, 36);
  h.writeUInt32LE(time, 68);
  h.writeUInt32LE(0x207fffff, 72);
  for (let nonce = 0; ; nonce++) {
    h.writeUInt32LE(nonce, 76);
    if (BigInt("0x" + Buffer.from(sha256d(h)).reverse().toString("hex")) <= TARGET) return new Uint8Array(h);
  }
}
const headers: Uint8Array[] = [];
let prev = sha256d(REGTEST_GENESIS);
roots.forEach((t, i) => {
  const id = t.id("hex");
  t.merklePath = new MerklePath(i + 1, [[{ offset: 0, hash: id, txid: true }]]);
  const h = mine(prev, internal(id), 1_790_300_000 + i * 600);
  headers.push(h);
  prev = sha256d(h);
});

// ---------------------------------------------------------------- the image: the four apps at birth
const work = mkdtempSync(join(tmpdir(), "skein-amm-e2e-"));
const images = join(work, "images");
mkdirSync(join(images, "token-e2e", "etc"), { recursive: true });
const tokenImage = JSON.parse(readFileSync(join(AMM, "image/token/etc/apps.json"), "utf8")) as { routes?: unknown[] };
writeFileSync(join(images, "token-e2e", "etc/apps.json"), JSON.stringify({ install: [CHAIN_DIR, SIGNER_DIR, WALLET_DIR, AMM], routes: tokenImage.routes ?? [] }, null, 2));

// The instance's key: the fixtures' validator identity (gen/vectors key(0x7f)).
const H = "tok";
const identityKey = PrivateKey.fromHex("7f".repeat(32));
const IDENTITY = identityKey.toPublicKey().toString();
check(IDENTITY === /pub const identity = "([0-9a-f]+)";/.exec(vectors)![1], `the instance's identity is the fixtures' validator identity (${IDENTITY.slice(0, 12)}…)`);
mkdirSync(join(work, "home"), { recursive: true });
const keys = new Map<string, InstanceType<typeof PrivateKey>>([[H, identityKey]]);
const keyOf = (h: string) => { let k = keys.get(h); if (!k) { k = PrivateKey.fromRandom(); keys.set(h, k); } return k; };

const arcade = await FakeArcade.start();
const afters: Array<() => unknown> = [];
const ownerKey = PrivateKey.fromRandom();
const h = await testHost({ after: (f: () => unknown) => afters.push(f) }, {
  ownerKey, genesis: { defaults: { walletNetwork: "regtest" } }, arc: { url: arcade.url, token: "the-amm-e2e-token", events: arcade.eventsUrl }, arcRetry: { min: 300, max: 1000 },
  reuse: { home: join(work, "home"), keyOf },
});
h.router.o.imagesDir = images;
let store = "";
try {
  await h.router.start();
  const made = await h.router.createInstance(H, h.ownerId, { image: "token-e2e", owned: true });
  check(made.identity === IDENTITY, `the instance ${H} born from the image, its identity the fixtures' (${made.identity.slice(0, 12)}…)`);
  store = storeSpec(h.db.get(H)!);
  const k = async () => (await h.router.hydrate(H)).kernel;
  const record = async (name: string) => {
    const kk = await k();
    const root = await kk.call("head", name) as unknown;
    return root ? await kk.store.get(root) as Record<string, unknown> : undefined;
  };
  const grants = await record("grants") as { calls?: Record<string, Record<string, string[]>>; services?: Record<string, string[]> };
  const manifest = JSON.parse(readFileSync(join(AMM, "etc/app.json"), "utf8")) as { version: string; calls: Record<string, string[]> };
  check(JSON.stringify(grants.calls?.amm) === JSON.stringify(Object.fromEntries(Object.entries(manifest.calls).map(([c, n]) => [c, [...n].sort()]))), `approvals at birth: amm calls ${JSON.stringify(grants.calls?.amm)}`);
  check(JSON.stringify(grants.services?.signer) === '["signer"]', `the signer app holds the signer service (${JSON.stringify(grants.services)})`);
  for (const a of ["chain", "signer", "wallet", "amm"]) check(!!(await record(`${a}/app`)), `${a} installed at birth`);
  check((await record("amm/app"))?.version === manifest.version, `amm ${String((await record("amm/app"))?.version)}`);

  // ------------------------------------------------ the chain: each root mined alone
  await h.router.chainHeaders(H, "chain", headers);
  await h.router.settled();

  const events = async (name: string) => {
    const res = await fetch(`${h.base}/@${H}/events/${name}`);
    return res.status === 200 ? (await res.json() as Array<{ name: string; record: Record<string, unknown> }>).map((x) => x.record) : [];
  };
  const getJson = async (path: string) => { const r = await fetch(`${h.base}/@${H}/${path}`); return { status: r.status, body: r.status === 200 ? await r.json() as any : await r.text() }; };

  // ------------------------------------------------ 1. the deploy event
  const deploy = txs.get("token_deploy")!;
  const tokenId = `${deploy.id("hex")}_0`;
  const topic = `tm_color_${tokenId}`;
  await h.router.admitEvent(H, "amm/deploy", { kind: "deploy", beef: Uint8Array.from(deploy.toAtomicBEEF()) });
  await h.router.settled();
  const tok = await until("amm/token", async () => (await record("amm/token")) as Record<string, unknown> | undefined, 30_000).catch(() => undefined);
  check(tok?.tokenId === tokenId, `the deploy event: amm/token ${String(tok?.tokenId)}`);
  const deployVerdict = await until("the deploy admitted", async () => (await events(`amm/topic/tx/${deploy.id("hex")}`)).some((r) => r.verdict === "admitted") || undefined, 30_000).then(() => true, () => false);
  check(deployVerdict, "the seed went in as the engine's own submission: amm/topic/tx/<deploy> admitted");

  // ------------------------------------------------ 2. the pool, a buy, a sell
  const submit = async (name: string) => {
    const t = txs.get(name)!;
    const r = await fetch(`${h.base}/@${H}/submit`, { method: "POST", headers: { "content-type": "application/octet-stream", "x-topics": JSON.stringify([topic]) }, body: Uint8Array.from(t.toAtomicBEEF()) });
    const text = await r.text();
    await h.router.settled();
    const admitted = await until(`${name} admitted`, async () => (await events(`amm/topic/tx/${t.id("hex")}`)).some((x) => x.verdict === "admitted") || undefined, 30_000).then(() => true, () => false);
    check(admitted, `${name} submitted (${r.status} ${text.slice(0, 120)}): amm/topic/tx admitted`);
    return t.id("hex");
  };
  const pd = await submit("pool_deploy");
  const buy = await submit("swap_bsv_in");
  const sell = await submit("swap_tokens_in");
  const poolId = `${pd}_0`;

  // ------------------------------------------------ 3. the recognizer's events, as the kernel indexed them
  const hops = await events(`amm/pool/${poolId}`);
  process.stdout.write(`  amm/pool/${poolId.slice(0, 12)}…: ${hops.map((r) => `${String(r.type)}@${String(r.txid).slice(0, 8)} swaps=${String(r.swaps)}`).join(" · ")}\n`);
  check(hops.map((r) => r.type).join(",") === "deploy,swap,swap", `amm/pool/<poolId>: deploy, swap, swap (${hops.map((r) => r.type).join(",")})`);
  const sellRow = (await events(`amm/output/${sell}.0`)).at(-1);
  check(sellRow?.type === "swap" && sellRow?.side === "sell" && sellRow?.poolId === poolId && sellRow?.keyId === `${buy}_0` && typeof sellRow?.feeVouts === "object", `amm/output/<sell>.0: the sell's hop row (side ${String(sellRow?.side)}, keyId ${String(sellRow?.keyId).slice(0, 12)}…, feeVouts ${JSON.stringify(sellRow?.feeVouts)})`);
  const buyRow = (await events(`amm/output/${buy}.0`)).at(-1);
  check(buyRow?.type === "swap" && buyRow?.side === "buy" && buyRow?.keyId === `${pd}_0`, `amm/output/<buy>.0: the buy's hop row (keyId ${String(buyRow?.keyId).slice(0, 12)}…)`);
  const all = await events("amm/hop");
  check(all.map((r) => r.type).join(",") === "deploy,swap,swap" && all.every((r) => r.tokenId === tokenId), `amm/hop: the instance's three rows in order (${all.map((r) => `${String(r.type)}`).join(",")})`);
  const live = await fetch(`${h.base}/@${H}/lookup`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ service: `ls_amm_${tokenId}`, query: { tokenId } }) });
  const liveBody = await live.json() as { result?: Array<{ outpoint: string }> };
  check(live.status === 200 && liveBody.result?.length === 1 && liveBody.result[0]!.outpoint === `${sell}_0`, `ls_amm_<id> {tokenId}: the one live pool at the sell's output (${live.status} ${JSON.stringify(liveBody).slice(0, 120)})`);

  // ------------------------------------------------ 4. the validator fees filed in the wallet
  const vBuy = Number((buyRow?.feeVouts as Record<string, number> | undefined)?.validator);
  const vSell = Number((sellRow?.feeVouts as Record<string, number> | undefined)?.validator);
  const bsv = await getJson("wallet/outputs?basket=bsv");
  const tokens = await getJson("wallet/outputs?basket=tokens");
  process.stdout.write(`  wallet bsv: ${JSON.stringify(bsv.body)}\n  wallet tokens: ${JSON.stringify(tokens.body)}\n`);
  const bOut = (bsv.body?.outputs ?? []) as Array<{ outpoint: string; satoshis: number; protocol: unknown; keyID: string; counterparty: string }>;
  const tOut = (tokens.body?.outputs ?? []) as typeof bOut;
  check(bOut.length === 1 && bOut[0]!.outpoint === `${buy}.${vBuy}` && JSON.stringify(bOut[0]!.protocol) === '[1,"amm pool"]' && bOut[0]!.counterparty === "anyone" && bOut[0]!.satoshis === Number(buyRow?.feeValidator), `the buy's validator fee filed under "bsv": ${bOut[0]?.outpoint.slice(0, 12)}… ${bOut[0]?.satoshis} sats, keyID ${bOut[0]?.keyID.slice(0, 12)}…`);
  check(tOut.length === 1 && tOut[0]!.outpoint === `${sell}.${vSell}` && tOut[0]!.keyID === `${pd}_0`, `the sell's validator fee filed under "tokens": ${tOut[0]?.outpoint.slice(0, 12)}…, keyID ${tOut[0]?.keyID.slice(0, 12)}… (the grandparent)`);
  const bal = await getJson("wallet/balance?basket=bsv");
  check(bal.body?.satoshis === Number(buyRow?.feeValidator), `/wallet/balance?basket=bsv: ${JSON.stringify(bal.body)}`);

  // ------------------------------------------------ 5. the token fee tally
  const tally = await getJson("amm/fees/tokens");
  check(tally.status === 200 && tally.body?.tokenId === tokenId && tally.body?.units === Number(sellRow?.feeValidator), `GET /amm/fees/tokens: ${JSON.stringify(tally.body)} (the sell's validator fee ${String(sellRow?.feeValidator)})`);
} catch (e) {
  check(false, `threw: ${(e as Error).stack}`);
} finally {
  for (const f of afters.reverse()) await f();
  await arcade.close();
  if (store) {
    const rp = spawnSync("node", ["--experimental-strip-types", "--no-warnings", join(SKEIN, "kernel-zig/equiv/replays.ts"), store], { encoding: "utf8", cwd: SKEIN });
    process.stdout.write(rp.stdout);
    if (rp.status !== 0) process.stdout.write(rp.stderr);
    check(rp.status === 0 && /identical .*the source store reproduced exactly/.test(rp.stdout), "the instance's store replays to itself exactly");
  }
}
process.stdout.write(failures ? `token instance e2e: ${failures} FAILED\n` : "token instance e2e: all ok\n");
process.exit(failures ? 1 : 0);
