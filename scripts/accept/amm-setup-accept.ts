// skein-amm#5 acceptance (0.22.0) on a scratch one-key host (RocksDB, as equiv/onekey.ts sets it up): an instance
// from skein-amm's image/token, born owned (`owned: true`, #164); the host chain fed regtest headers; ONE
// event {kind: "deploy", beef} in amm/deploy (Router.admitEvent) with a deploy mined in the test → the
// instance serves tm_color_<id>, ls_color_<id>, ls_amm_<id>, beats on tm_color_<id>-live and
// ls_amm_<id>-live, answers GET /amm/token, and ls_color_<id>'s color-admission answers the deploy; a
// second deploy event is refused and changes nothing; the store replays to itself.
//
//
// It imports skein's host by relative path: copy it into a skein checkout's .scratch/ (beside src/) and run it there.
//   AMM_DIR=<skein-amm checkout> SKEIN_EQUIV_STORE=rocksdb SKEIN_KERNEL_BIN=… node --experimental-strip-types --no-warnings .scratch/amm-setup-accept.ts
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { createServer } from "node:net";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { LockingScript, MerklePath, P2PKH, PrivateKey, Transaction, UnlockingScript } from "@bsv/sdk";
import * as dagCbor from "@ipld/dag-cbor";
import * as dagJson from "@ipld/dag-json";
import { CID } from "multiformats/cid";
import { storeSpec } from "../src/runtime/index-store.ts";
import { BACKEND, routerStore } from "../kernel-zig/equiv/backend.ts";
import { FakeArcade } from "../src/host/fake-arcade.ts";
import { HostDb } from "../src/host/instances.ts";
import { Signer } from "../src/host/signer.ts";
import { Router } from "../src/host/router.ts";
import { DoorAnswered } from "../src/host/kernel.ts";
import { appHead } from "../src/host/plan.ts";

const here = dirname(fileURLToPath(import.meta.url));
const AMM = process.env.AMM_DIR!;
const kernel = process.env.SKEIN_KERNEL_BIN ?? join(here, "../kernel-zig/zig-out/bin/skein-kernel");
const home = mkdtempSync(join(tmpdir(), "amm-setup-acc-"));
let failures = 0;
const check = (ok: boolean, what: string) => { process.stdout.write(`${ok ? "ok  " : "FAIL"} ${what}\n`); if (!ok) failures++; };
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
async function until<T>(what: string, f: () => Promise<T | undefined> | T | undefined, ms = 30_000): Promise<T> {
  for (const end = Date.now() + ms; ;) {
    const v = await f().catch(() => undefined);
    if (v) return v;
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await sleep(100);
  }
}
const freePort = () => new Promise<number>((resolve, reject) => {
  const s = createServer();
  s.once("error", reject);
  s.listen(0, "127.0.0.1", () => { const p = (s.address() as { port: number }).port; s.close(() => resolve(p)); });
});

// ---------------------------------------------------------------- regtest (as equiv/onekey.ts)
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
const headers: Uint8Array[] = [mine(sha256d(REGTEST_GENESIS), sha256d(Buffer.concat([internal("cb".repeat(32)), internal("cc".repeat(32))])), 1_790_200_000)];
function mineAlone(t: Transaction): number {
  const height = headers.length + 1;
  const cb = (height * 17 % 256).toString(16).padStart(2, "0").repeat(32);
  t.merklePath = new MerklePath(height, [[{ offset: 0, hash: cb }, { offset: 1, hash: t.id("hex"), txid: true }]]);
  headers.push(mine(sha256d(headers.at(-1)!), sha256d(Buffer.concat([internal(cb), internal(t.id("hex"))])), 1_790_200_000 + height * 600));
  return height;
}

// A BRC-162 binary deploy at output 0: OP_0 <supply> OP_2DROP <payload: dag-cbor {sym, dec}> OP_DROP <p2pkh>.
const holder = new PrivateKey("3333", 16);
function deploy(seed: number, sym: string, dec: number): Transaction {
  const t = new Transaction();
  t.addInput({ sourceTXID: seed.toString(16).padStart(2, "0").repeat(32), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex("51"), sequence: 0xffffffff });
  const lock = new P2PKH().lock(holder.toPublicKey().toHash());
  const s = new LockingScript().writeOpCode(0x00).writeNumber(21_000_000).writeOpCode(0x6d).writeBin(Array.from(dagCbor.encode({ sym, dec }))).writeOpCode(0x75);
  const script = LockingScript.fromBinary([...s.toBinary(), ...lock.toBinary()]);
  t.addOutput({ lockingScript: script, satoshis: 1 });
  return t;
}

// ---------------------------------------------------------------- the host
const shared = new PrivateKey("0e0e0e", 16);
const one = Signer.oneKey(shared);
const SHARED = shared.toPublicKey().toString();
const owner = new PrivateKey("0f0f0f", 16).toPublicKey().toString();
const homeO = join(home, "o");
mkdirSync(homeO, { recursive: true });
const dbO = new HostDb(join(homeO, "host.db"));
const arcade = await FakeArcade.start();
const port = await freePort();
const lines: string[] = [];
const rO = new Router({
  owner: SHARED, idleMs: 0, answerWaitMs: 8000, libp2pDiscoveryMs: 300, genesis: { defaults: { walletNetwork: "regtest" } },
  arc: { url: arcade.url, token: "t", events: arcade.eventsUrl }, arcRetry: { min: 500, max: 2000 },
  db: dbO, home: homeO, ...routerStore(homeO), kernel: { command: kernel, env: { SKEIN_HOME: homeO } },
  walletFor: (row) => one.wallet(row.handle), providerKeyFor: (n) => one.providerKey(n), peerKeyFor: (x) => one.peerKey(x),
  claimer: one.wallet(""), imagesDir: join(AMM, "image"),
  libp2p: { listen: [`/ip4/127.0.0.1/tcp/${port}`], bootstrap: [], dht: "off", relays: [], mdns: false },
  log: (s, l) => { lines.push(`[${s}] ${l}`); if (process.env.VERBOSE) process.stdout.write(`  | [${s}] ${l}\n`); },
});
const stores = new Map<string, string>();
const keep = (h: string) => { const r = dbO.get(h); if (r) stores.set(storeSpec(r), h); };
const feed = async (h: string, from = 1) => { for (let i = from; i <= headers.length; i++) await rO.chainHeaders(h, "chain", [headers[i - 1]!]); await rO.settled(); };
const get = async (path: string) => { const r = await fetch(`http://127.0.0.1:${rO.port}${path}`); return { status: r.status, text: await r.text() }; };
const getJson = async (path: string) => { const r = await get(path); return r.status === 200 ? JSON.parse(r.text) : undefined; };
const H = "tok";

try {
  await rO.listen(0);
  const dA = deploy(0x41, "AAA", 5);
  const hA = mineAlone(dA);
  const dB = deploy(0x42, "BBB", 2);
  mineAlone(dB);
  const A = `${dA.id("hex")}_0`;
  const tmA = `tm_color_${A}`, lsA = `ls_color_${A}`, ammA = `ls_amm_${A}`;

  // ------------------------------------------------ the instance: from image/token, born owned
  const made = await rO.createInstance(H, owner, { image: "token", owned: true });
  keep(H);
  const k = (await rO.hydrate(H)).kernel;
  const appRec = await k.store.get(await k.call("head", appHead("amm")) as CID) as { version?: string; programs?: Record<string, unknown> };
  check(appRec.version === "0.22.0" && "amm-setup" in (appRec.programs ?? {}), `(image/token) amm ${appRec.version}, programs: ${Object.keys(appRec.programs ?? {}).join(", ")}`);
  const grants = await k.store.get(await k.call("head", "grants") as CID) as { roles?: Record<string, Uint8Array[]> };
  check((grants.roles?.root ?? []).map((x) => Buffer.from(x).toString("hex")).join() === owner, "born owned: root is the owner's key (no claim)");
  check((await k.dispatch()).rows.some((r) => r.transport === "event" && r.address === "amm/deploy" && JSON.stringify(r.filters) === '["kernel.beef"]'), "the event route amm/deploy names kernel.beef");
  await feed(H);
  check(((await k.call("head", "amm/token")) as CID | null) === null, "no amm/token before the event");
  check((await get(`/@${H}/amm/token`)).status === 404, "GET /amm/token: 404 before the event");
  const lt0 = await getJson(`/@${H}/amm/listTopicManagers`);
  check(!!lt0 && !(tmA in lt0), `before the event the instance serves no tm_color_<A> (${Object.keys(lt0 ?? {}).join(", ")})`);

  // ------------------------------------------------ the one event
  const t0 = Date.now();
  const entry = await rO.admitEvent(H, "amm/deploy", { kind: "deploy", beef: Uint8Array.from(dA.toAtomicBEEF()) });
  await rO.settled();
  const e = await k.store.get(entry) as { event: CID };
  const rec = await k.store.get(e.event) as { kind?: string; beef?: { form?: string; beef?: CID } };
  check(rec.kind === "deploy" && !!rec.beef?.beef, `the event admitted, its record the door's envelope (${JSON.stringify(rec.beef ? { form: rec.beef.form } : rec)})`);
  const tokCid = await k.call("head", "amm/token") as CID | null;
  const tok = tokCid ? await k.store.get(tokCid) as Record<string, unknown> : undefined;
  process.stdout.write(`  amm/token ${dagJson.stringify(tok ?? null)}\n`);
  check(tok?.tokenId === A && tok?.topic === tmA && tok?.lookup === lsA && tok?.ammLookup === ammA && tok?.sym === "AAA" && tok?.dec === 5 && tok?.txid === dA.id("hex") && tok?.vout === 0, "the head amm/token: {tokenId, topic, lookup, ammLookup, sym, dec, txid, vout}");
  const tokenRead = await get(`/@${H}/amm/token`);
  check(tokenRead.status === 200 && JSON.parse(tokenRead.text).tokenId === A, `GET /amm/token answers (${tokenRead.status} ${tokenRead.text.slice(0, 160)})`);
  const lt = await getJson(`/@${H}/amm/listTopicManagers`);
  check(!!lt && tmA in lt, `listTopicManagers: ${Object.keys(lt ?? {}).join(", ")}`);
  const ll = await getJson(`/@${H}/amm/listLookupServiceProviders`);
  check(!!ll && lsA in ll && ammA in ll, `listLookupServiceProviders: ${Object.keys(ll ?? {}).join(", ")}`);
  const admitted = await until("the deploy admitted under tm_color_<A>", async () => {
    const adm = await (rO as unknown as { overlayRead: (h: string, app: string, fn: string, q: Record<string, string>) => Promise<{ blocks: Array<{ txids: string[] }> }> }).overlayRead(H, "amm", "admitted", { topic: tmA, fromHeight: String(hA), toHeight: String(hA) });
    return adm.blocks.some((b) => b.txids.includes(dA.id("hex"))) ? adm : undefined;
  }, 20_000).catch(() => undefined);
  check(!!admitted, `tm_color_<A>'s admitted (height ${hA}) holds the deploy`);
  const lk = await fetch(`http://127.0.0.1:${rO.port}/@${H}/amm/lookup`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ service: lsA, query: { type: "color-admission", version: 1, assetId: A, topic: tmA, outpoints: [`${dA.id("hex")}.0`] } }) });
  const lkb = await lk.json() as { type?: string; outputs?: unknown[] };
  check(lk.status === 200 && lkb.type === "output-list" && (lkb.outputs?.length ?? 0) === 1, `ls_color_<A> color-admission answers the deploy (${lk.status} ${JSON.stringify(lkb).slice(0, 120)})`);
  const liveT = await until("tm_color_<A>-live", async () => { const x = await getJson(`/@${H}/amm/.live/${tmA}-live`) as unknown[] | undefined; return x?.length ? x : undefined; }, 60_000).catch(() => undefined);
  check(!!liveT, `beats on tm_color_<A>-live (${liveT?.length ?? 0} beat(s) seen, ${Date.now() - t0} ms after the event)`);
  const liveL = await until("ls_amm_<A>-live", async () => { const x = await getJson(`/@${H}/amm/.live/${ammA}-live`) as unknown[] | undefined; return x?.length ? x : undefined; }, 60_000).catch(() => undefined);
  check(!!liveL, `beats on ls_amm_<A>-live (${liveL?.length ?? 0})`);

  // ------------------------------------------------ a second deploy event: refused, nothing changes
  const heads0 = JSON.stringify([await k.call("head", "amm/token"), await k.call("head", "amm/topics"), await k.call("head", "amm/lookups")].map(String));
  let second = "admitted";
  try { await rO.admitEvent(H, "amm/deploy", { kind: "deploy", beef: Uint8Array.from(dB.toAtomicBEEF()) }); } catch (x) { second = x instanceof DoorAnswered ? `door: ${x.answer.reason}` : (x as Error).message; }
  await rO.settled();
  const heads1 = JSON.stringify([await k.call("head", "amm/token"), await k.call("head", "amm/topics"), await k.call("head", "amm/lookups")].map(String));
  check(heads0 === heads1, `a second deploy event (B): ${second}; amm/token, amm/topics, amm/lookups unchanged`);
  const refusal = lines.filter((l) => /refused: this instance has its token/.test(l));
  const lt2 = await getJson(`/@${H}/amm/listTopicManagers`);
  check(!(`tm_color_${dB.id("hex")}_0` in (lt2 ?? {})) && JSON.parse((await get(`/@${H}/amm/token`)).text).tokenId === A, `…B not served, /amm/token still A (refusal logged: ${refusal.length ? refusal[0]!.slice(0, 160) : "not in the host log; the step's result record holds it"})`);
  void made;
} catch (x) {
  check(false, `threw: ${(x as Error).stack}`);
} finally {
  await rO.stop();
  await arcade.close();
  dbO.close();
  for (const [store, h] of stores) {
    const rp = spawnSync("node", ["--experimental-strip-types", "--no-warnings", join(here, "../kernel-zig/equiv/replays.ts"), store], { encoding: "utf8", env: { ...process.env, SKEIN_KERNEL_BIN: kernel } });
    if (rp.status !== 0) process.stdout.write(rp.stdout.slice(-2000) + rp.stderr.slice(-2000));
    check(rp.status === 0 && /identical .*the source store reproduced exactly/.test(rp.stdout), `${h}: the store replays to itself exactly after the processes stopped`);
  }
}
if (failures && process.env.KEEP_LOG) writeFileSync(join(tmpdir(), "amm-setup-accept.log"), lines.join("\n"));
if (process.env.KEEP) process.stdout.write(`kept ${home}\n`); else rmSync(home, { recursive: true, force: true });
process.stdout.write(failures ? `amm setup acceptance (${BACKEND}): ${failures} FAILED\n` : `amm setup acceptance (${BACKEND}): all ok\n`);
process.exit(failures ? 1 : 0);
