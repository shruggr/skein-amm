//! The validator's Rescind (skein-amm 0.13.0, David Case 2026-10-22: it replaces 0.11.0's
//! validator Close, which replaced 0.9.0's rescind of the claim). Root's message `{fn:
//! "rescind", args: {pool: "<txid>.<vout>"}}` in the box `amm/validator` (main.zig): the pool
//! (that outpoint, or the newest continuation it leads to) is spent by the contract's
//! `Rescind(validatorSig)` — the signature the pool's current validator key's — which writes it
//! back at output 0 unchanged but for its ValidatorPubKey, cleared to 33 zero bytes: no Swap can
//! be signed for it again, amm-lookup delists it, and the LP closes it as any pool. The validator
//! pays the miner fee from the instance's wallet; nothing is taken from the pool.
//!
//! The pool is a caller input of the instance's wallet (shruggr/skein#93): `createAction` with
//! the pool as an input whose unlocking script is the validator's (only its length now, the
//! longest the call can be), the pool's source in `inputBEEF`, the contract's one output (the
//! rescinded pool) and `noSend` — the wallet adds its own funding input(s) and, after the
//! caller's output, its one P2PKH change output (skein docs/WALLET.md "The builder and the
//! signer"), and answers a draft (`reference`). The validator then writes the pool input's Rúnar
//! call over the draft (its change the wallet's change output, as Rúnar's `_changePKH` /
//! `_changeAmount`, so the contract's hashOutputs is the draft's), signs it through the signer
//! (the pool's current validator key, `[1, "amm pool"]`, anyone; validator.zig `currentKeyId`),
//! and finishes it with `signAction {reference, spends}`; then submits it to its own overlay (the
//! topic admits the pool's tokens back at output 0; amm-lookup drops the pool) and answers root.
//! Each wallet call is a thread of the genesis wallet program (launch.zig).
//!
//! Pure: the bodies and the call, over a View and an Oracle; tested with a mocked wallet.
const std = @import("std");
const w = @import("chain");
const color = @import("1color");
const pool = @import("pool");
const unlock = @import("unlock.zig");
const validator = @import("validator.zig");
const View = @import("view.zig").View;
const Oracle = @import("oracle.zig").Oracle;

const cbor = w.cbor;
const Value = cbor.Value;
const beef = w.beef;
const bsvz = w.bsvz;
const Transaction = bsvz.transaction.Transaction;
const Outpoint = validator.Outpoint;

/// The pool the validator rescinds: its current output (held and live in its topic), as the
/// overlay holds it.
pub const Target = struct {
    op: Outpoint,
    script: []const u8,
    satoshis: u64,
    pool: pool.Pool,
};

pub const Found = union(enum) { ok: Target, refused: []const u8 };

const poolOfScript = pool.poolOfScript;

/// The pool to rescind from `from` (any of its outputs, the deploy's among them): a pool of ours,
/// followed to its newest output (validator.zig `newest`). Refused (why) otherwise.
pub fn targetOf(a: std.mem.Allocator, v: View, identity: [33]u8, from: Outpoint) !Found {
    const o = (try v.output(a, from)) orelse return .{ .refused = "the pool output is not held" };
    const p = poolOfScript(o.script) orelse return .{ .refused = "not a pool" };
    if (!std.mem.eql(u8, &p.identity, &identity)) return .{ .refused = "not our pool" };
    const topic = try validator.topicOf(a, p.asset_id);
    const n = (try validator.newest(a, v, topic, from)) orelse return .{ .refused = "the pool's newest output is not held" };
    const s = switch (n) {
        .closed => |c| return .{ .refused = try std.fmt.allocPrint(a, "the pool is closed (by {s})", .{&w.header.toHex(c.by)}) },
        .live => |s| s,
    };
    if (s.pool.rescinded()) return .{ .refused = "the pool is rescinded already" };
    const cur = (try v.output(a, s.outpoint)) orelse return .{ .refused = "the pool's newest output is not held" };
    return .{ .ok = .{ .op = s.outpoint, .script = cur.script, .satoshis = s.satoshis, .pool = s.pool } };
}

pub const Out = struct { satoshis: u64, script: []const u8 };

/// The contract's Rescind output (Pool.runar.go `Rescind`): the pool written back holding the
/// same sats, its prefix the contract's for its TokenReserve, its lock the same but for the
/// ValidatorPubKey, 33 zero bytes.
pub fn rescindOutput(a: std.mem.Allocator, script: []const u8, satoshis: u64) !Out {
    const lock = (color.brc162.decode(script) orelse return error.NotAPool).lock;
    const p = (pool.parse(lock) catch return error.NotAPool) orelse return error.NotAPool;
    const out = try a.dupe(u8, lock);
    @memset(out[p.code_len + 1 + 8 + 33 ..][0..33], 0); // OP_RETURN, TokenReserve, LpPubKey
    var buf: [10]u8 = undefined;
    return .{ .satoshis = satoshis, .script = try std.mem.concat(a, u8, &.{ &.{0x20}, &p.asset_id, color.brc162.pushAmount(&buf, p.token_reserve), &.{0x6d}, out }) };
}

fn pushLen(n: usize) usize {
    return n + @as(usize, if (n <= 0x4b) 1 else if (n <= 0xff) 2 else if (n <= 0xffff) 3 else 5);
}

/// The longest the pool input's unlocking script can be (what the wallet's fee is paid for):
/// the code push, a 72-byte DER signature and its sighash byte, a 20-byte change key hash, an
/// 8-byte change amount, the preimage (its length fixed by the pool's scriptCode) and the method
/// index.
pub fn maxUnlockLen(script: []const u8) !usize {
    const lock = (color.brc162.decode(script) orelse return error.NotAPool).lock;
    const code = unlock.poolCode(lock) orelse return error.NotAPool;
    const sc = unlock.poolScriptCode(lock) orelse return error.NotAPool;
    const varint: usize = if (sc.len < 0xfd) 1 else if (sc.len <= 0xffff) 3 else 5;
    const pre = 4 + 32 + 32 + 32 + 4 + varint + sc.len + 8 + 4 + 32 + 4 + 4;
    return pushLen(code.len) + pushLen(73) + pushLen(20) + pushLen(8) + pushLen(pre) + 1;
}

fn outpointText(a: std.mem.Allocator, op: Outpoint) ![]const u8 {
    return std.fmt.allocPrint(a, "{s}.{d}", .{ &w.header.toHex(op.txid), op.vout });
}

/// The wallet's `createAction` body: the pool as a caller input (its unlocking script to come),
/// `input_beef` its source's BEEF, the contract's one output (the rescinded pool), noSend.
pub fn createBody(a: std.mem.Allocator, t: Target, input_beef: []const u8) !Value {
    const ro = try rescindOutput(a, t.script, t.satoshis);
    const out = struct {
        fn f(al: std.mem.Allocator, o: Out, desc: []const u8) !Value {
            return .{ .map = try al.dupe(cbor.Entry, &.{
                .{ .key = "lockingScript", .value = .{ .bytes = o.script } },
                .{ .key = "satoshis", .value = .{ .uint = o.satoshis } },
                .{ .key = "outputDescription", .value = .{ .text = desc } },
            }) };
        }
    }.f;
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "op", .value = .{ .text = "createAction" } },
        .{ .key = "description", .value = .{ .text = "AMM pool rescinded by its validator" } },
        .{ .key = "inputs", .value = .{ .array = try a.dupe(Value, &.{.{ .map = try a.dupe(cbor.Entry, &.{
            .{ .key = "outpoint", .value = .{ .text = try outpointText(a, t.op) } },
            .{ .key = "unlockingScriptLength", .value = .{ .uint = try maxUnlockLen(t.script) } },
            .{ .key = "inputDescription", .value = .{ .text = "the AMM pool" } },
        }) }}) } },
        .{ .key = "inputBEEF", .value = .{ .bytes = input_beef } },
        .{ .key = "outputs", .value = .{ .array = try a.dupe(Value, &.{
            try out(a, ro, "the AMM pool, rescinded"),
        }) } },
        .{ .key = "labels", .value = .{ .array = try a.dupe(Value, &.{.{ .text = "amm-rescind" }}) } },
        .{ .key = "options", .value = .{ .map = try a.dupe(cbor.Entry, &.{.{ .key = "noSend", .value = .{ .boolean = true } }}) } },
    }) };
}

/// The transaction an Atomic BEEF is about, and the BEEF.
fn subjectOf(a: std.mem.Allocator, bytes: []const u8) !struct { tx: Transaction, b: beef.Beef } {
    const b = try beef.parse(a, bytes);
    const s = b.subject() orelse return error.InvalidBeef;
    const e = b.find(s) orelse return error.InvalidBeef;
    return .{ .tx = try Transaction.parse(a, e.raw orelse return error.InvalidBeef), .b = b };
}

/// The pool input of a draft (the wallet's `createAction` answer, Atomic BEEF) or of the signed
/// Rescind: the input whose source, in the BEEF, is a pool of ours; its index, the pool and the
/// source output.
pub const Located = struct { index: usize, op: Outpoint, pool: pool.Pool, script: []const u8, satoshis: u64, tx: Transaction };

pub fn locate(a: std.mem.Allocator, identity: [33]u8, draft: []const u8) !?Located {
    const d = try subjectOf(a, draft);
    for (d.tx.inputs, 0..) |in, i| {
        const e = d.b.find(in.previous_outpoint.txid.bytes) orelse continue;
        const st = Transaction.parse(a, e.raw orelse continue) catch continue;
        if (in.previous_outpoint.index >= st.outputs.len) continue;
        const o = st.outputs[in.previous_outpoint.index];
        const p = poolOfScript(o.locking_script.bytes) orelse continue;
        if (!std.mem.eql(u8, &p.identity, &identity)) continue;
        return .{
            .index = i,
            .op = .{ .txid = in.previous_outpoint.txid.bytes, .vout = in.previous_outpoint.index },
            .pool = p,
            .script = o.locking_script.bytes,
            .satoshis = @intCast(o.satoshis),
            .tx = d.tx,
        };
    }
    return null;
}

/// A minimal script-number push of `n` (OP_0, OP_1..OP_16, else the shortest little-endian
/// sign-magnitude bytes), as Rúnar writes `_changeAmount`.
fn pushNum(a: std.mem.Allocator, n: u64) ![]u8 {
    if (n == 0) return a.dupe(u8, &.{0x00});
    if (n <= 16) return a.dupe(u8, &.{@intCast(0x50 + n)});
    var b: [9]u8 = undefined;
    var len: usize = 0;
    var v = n;
    while (v > 0) : (v >>= 8) {
        b[len] = @truncate(v);
        len += 1;
    }
    if (b[len - 1] & 0x80 != 0) {
        b[len] = 0;
        len += 1;
    }
    return std.mem.concat(a, u8, &.{ &.{@intCast(len)}, b[0..len] });
}

/// The pool input's unlocking script: the Rúnar call of Rescind (method 2) — `_codePart`, the
/// validator key's signature (`key_id`, through the signer) over the draft's BIP-143 preimage
/// (ALL|FORKID, the pool's scriptCode), the wallet's change as `_changePKH` / `_changeAmount`
/// (none: 20 zero bytes, 0), the preimage, the method index. Refused when the draft's outputs are
/// not the contract's one then at most one P2PKH change.
pub fn unlockRescind(a: std.mem.Allocator, l: Located, key_id: []const u8, oracle: Oracle) ![]const u8 {
    const want = try rescindOutput(a, l.script, l.satoshis);
    const t = l.tx;
    if (t.outputs.len < 1 or t.outputs.len > 2) return error.NotTheContractsOutputs;
    if (@as(u64, @intCast(t.outputs[0].satoshis)) != want.satoshis or !std.mem.eql(u8, t.outputs[0].locking_script.bytes, want.script)) return error.NotTheContractsOutputs;
    var change_pkh: [20]u8 = .{0} ** 20;
    var change: u64 = 0;
    if (t.outputs.len == 2) {
        change_pkh = unlock.p2pkhHash(t.outputs[1].locking_script.bytes) orelse return error.ChangeNotP2pkh;
        change = @intCast(t.outputs[1].satoshis);
    }
    const lock = (color.brc162.decode(l.script) orelse return error.NotAPool).lock;
    const code = unlock.poolCode(lock) orelse return error.NotAPool;
    const sc = unlock.poolScriptCode(lock) orelse return error.NotAPool;
    const pre = try unlock.preimage(a, &t, l.index, sc, l.satoshis, unlock.sighash_all_forkid);
    const digest = unlock.sha256d(pre);
    const der = try oracle.sign(a, key_id, digest);
    const sig = try std.mem.concat(a, u8, &.{ der, &.{@as(u8, unlock.sighash_all_forkid)} });
    if (!unlock.verify(sig, &l.pool.validator, digest, unlock.sighash_all_forkid)) return error.NotThePoolsValidatorKey;
    return std.mem.concat(a, u8, &.{
        try unlock.pushData(a, code),
        try unlock.pushData(a, sig),
        try unlock.pushData(a, &change_pkh),
        try pushNum(a, change),
        try unlock.pushData(a, pre),
        &.{0x52}, // Rescind
    });
}

/// The pool's current validator key ID (validator.zig `currentKeyId`), checked against its
/// ValidatorPubKey.
pub fn keyIdOf(a: std.mem.Allocator, v: View, l: Located) !?[]const u8 {
    return validator.currentKeyId(a, v, l.pool, l.satoshis, l.op);
}

/// The wallet's `signAction` body for a draft: `{reference, spends: {"<i>": {unlockingScript}}}`.
pub fn signBody(a: std.mem.Allocator, reference: []const u8, index: usize, unlocking: []const u8) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "op", .value = .{ .text = "signAction" } },
        .{ .key = "reference", .value = .{ .cid = reference } },
        .{ .key = "spends", .value = .{ .map = try a.dupe(cbor.Entry, &.{.{ .key = try std.fmt.allocPrint(a, "{d}", .{index}), .value = .{ .map = try a.dupe(cbor.Entry, &.{
            .{ .key = "unlockingScript", .value = .{ .bytes = unlocking } },
        }) } }}) } },
    }) };
}

/// Where a Rescind is, from the wallet thread a step was stepped by (its `resolved`): the wallet's
/// result record (`wallet-result`, op `createAction` or `signAction`).
pub const Stage = union(enum) {
    /// The draft: `reference` and the draft's Atomic BEEF.
    draft: struct { reference: []const u8, tx: []const u8 },
    /// Signed: the final transaction, Atomic BEEF (noSend: the overlay's submission broadcasts it).
    signed: []const u8,
    failed: []const u8,
};

pub fn stageOf(result: Value) Stage {
    if (!std.mem.eql(u8, result.getText("kind") orelse "", "wallet-result")) return .{ .failed = "not a wallet result" };
    const op = result.getText("op") orelse "";
    const tx = result.getBytes("tx") orelse return .{ .failed = "the wallet answered no transaction" };
    if (std.mem.eql(u8, op, "createAction")) return .{ .draft = .{ .reference = result.getCid("reference") orelse return .{ .failed = "no draft: the wallet signed the pool input itself" }, .tx = tx } };
    if (std.mem.eql(u8, op, "signAction")) return .{ .signed = tx };
    return .{ .failed = "not a createAction or signAction result" };
}

/// The result record's CID a resolved thread printed: its update's `result` is `{exitCode, stdout,
/// stderr}` and a program's stdout the CID of the record it kept, hex (skein `vm.finish`).
pub fn resultCid(a: std.mem.Allocator, result: Value) !?[]const u8 {
    const out = result.getBytes("stdout") orelse return null;
    const hex = std.mem.trim(u8, out, " \r\n\t");
    if (hex.len == 0 or hex.len % 2 != 0) return null;
    const bytes = try a.alloc(u8, hex.len / 2);
    _ = std.fmt.hexToBytes(bytes, hex) catch return null;
    return bytes;
}

/// The topic the Rescind is submitted for: the pool's token's.
pub fn topicOf(a: std.mem.Allocator, p: pool.Pool) ![]u8 {
    return validator.topicOf(a, p.asset_id);
}
