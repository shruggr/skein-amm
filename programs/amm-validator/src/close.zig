//! The validator's Close (skein-amm 0.11.0, David Case 2026-10-21: "delisting becomes the
//! contract's Close"; it replaces 0.9.0's rescind, which spent the claim). Root's message
//! `{fn: "close", args: {pool: "<txid>.<vout>"}}` in the box `amm/validator` (main.zig): the pool
//! (that outpoint, or the newest continuation it leads to) is closed by the contract's
//! `Close(sig, bsvFee)` on the validator's path — `sig` the pool's current validator key's,
//! `bsvFee` 0 — so the payout is the LP's exactly as on the LP's own Close: every sat of the pool
//! to P2PKH of the LP key, then every token to the same key. The validator pays the miner fee and
//! the token output's sat from the instance's wallet.
//!
//! The pool is a caller input of the instance's wallet (shruggr/skein#93): `createAction` with
//! the pool as an input whose unlocking script is the validator's (only its length now, the
//! longest the call can be), the pool's source in `inputBEEF`, the contract's two outputs and
//! `noSend` — the wallet adds its own funding input(s) and, after the caller's outputs, its one
//! P2PKH change output (skein docs/WALLET.md "The builder and the signer"), and answers a draft
//! (`reference`). The validator then writes the pool input's Rúnar call over the draft (its change
//! the wallet's change output, as Rúnar's `_changePKH` / `_changeAmount`, so the contract's
//! hashOutputs is the draft's), signs it through the signer (the pool's current validator key,
//! `[1, "amm pool"]`, key ID the first token input of the transaction that created the pool,
//! anyone; validator.zig `currentKeyId`), and finishes it with `signAction {reference, spends}`;
//! then submits it to its own overlay (the topic admits the LP's tokens; amm-lookup drops the
//! pool) and answers root. Each wallet call is a thread of the genesis wallet program
//! (launch.zig).
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

/// The pool the validator closes: its current output (held and live in its topic), as the
/// overlay holds it.
pub const Target = struct {
    op: Outpoint,
    script: []const u8,
    satoshis: u64,
    pool: pool.Pool,
};

pub const Found = union(enum) { ok: Target, refused: []const u8 };

fn poolOfScript(script: []const u8) ?pool.Pool {
    const tok = color.brc162.decode(script) orelse return null;
    if (tok.role != .value) return null;
    return (pool.parse(tok.lock) catch null) orelse null;
}

/// The pool to close from `from` (any of its outputs, the deploy's among them): a pool of ours,
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
    const cur = (try v.output(a, s.outpoint)) orelse return .{ .refused = "the pool's newest output is not held" };
    return .{ .ok = .{ .op = s.outpoint, .script = cur.script, .satoshis = s.satoshis, .pool = s.pool } };
}

/// The contract's Close outputs at bsvFee 0 (Pool.runar.go `Close`): the pool's sats to P2PKH of
/// the LP key, then every token to the same key (1 sat).
pub const Out = struct { satoshis: u64, script: []const u8 };

pub fn closeOutputs(a: std.mem.Allocator, p: pool.Pool, satoshis: u64) ![2]Out {
    const lp_pkh = bsvz.crypto.hash.hash160(&p.lp).bytes;
    return .{
        .{ .satoshis = satoshis, .script = try pool.payoutScript(a, p.asset_id, satoshis, lp_pkh, true) },
        .{ .satoshis = 1, .script = try pool.payoutScript(a, p.asset_id, p.token_reserve, lp_pkh, false) },
    };
}

fn pushLen(n: usize) usize {
    return n + @as(usize, if (n <= 0x4b) 1 else if (n <= 0xff) 2 else if (n <= 0xffff) 3 else 5);
}

/// The longest the pool input's unlocking script can be (what the wallet's fee is paid for):
/// the code push, a 72-byte DER signature and its sighash byte, bsvFee `OP_0`, a 20-byte change
/// key hash, an 8-byte change amount, the preimage (its length fixed by the pool's scriptCode)
/// and the method index.
pub fn maxUnlockLen(script: []const u8) !usize {
    const lock = (color.brc162.decode(script) orelse return error.NotAPool).lock;
    const code = unlock.poolCode(lock) orelse return error.NotAPool;
    const sc = unlock.poolScriptCode(lock) orelse return error.NotAPool;
    const varint: usize = if (sc.len < 0xfd) 1 else if (sc.len <= 0xffff) 3 else 5;
    const pre = 4 + 32 + 32 + 32 + 4 + varint + sc.len + 8 + 4 + 32 + 4 + 4;
    return pushLen(code.len) + pushLen(73) + 1 + pushLen(20) + pushLen(8) + pushLen(pre) + 1;
}

fn outpointText(a: std.mem.Allocator, op: Outpoint) ![]const u8 {
    return std.fmt.allocPrint(a, "{s}.{d}", .{ &w.header.toHex(op.txid), op.vout });
}

/// The wallet's `createAction` body: the pool as a caller input (its unlocking script to come),
/// `input_beef` its source's BEEF, the contract's two outputs, noSend.
pub fn createBody(a: std.mem.Allocator, t: Target, input_beef: []const u8) !Value {
    const outs = try closeOutputs(a, t.pool, t.satoshis);
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
        .{ .key = "description", .value = .{ .text = "AMM pool closed by its validator" } },
        .{ .key = "inputs", .value = .{ .array = try a.dupe(Value, &.{.{ .map = try a.dupe(cbor.Entry, &.{
            .{ .key = "outpoint", .value = .{ .text = try outpointText(a, t.op) } },
            .{ .key = "unlockingScriptLength", .value = .{ .uint = try maxUnlockLen(t.script) } },
            .{ .key = "inputDescription", .value = .{ .text = "the AMM pool" } },
        }) }}) } },
        .{ .key = "inputBEEF", .value = .{ .bytes = input_beef } },
        .{ .key = "outputs", .value = .{ .array = try a.dupe(Value, &.{
            try out(a, outs[0], "the LP's BSV"),
            try out(a, outs[1], "the LP's tokens"),
        }) } },
        .{ .key = "labels", .value = .{ .array = try a.dupe(Value, &.{.{ .text = "amm-close" }}) } },
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
/// Close: the input whose source, in the BEEF, is a pool of ours; its index, the pool and the
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

/// The pool input's unlocking script: the Rúnar call of Close (method 1) on the validator's
/// path — `_codePart`, the validator key's signature (`key_id`, through the signer) over the
/// draft's BIP-143 preimage (ALL|FORKID, the pool's scriptCode), bsvFee 0, the wallet's change as
/// `_changePKH` / `_changeAmount` (none: 20 zero bytes, 0), the preimage, the method index.
/// Refused when the draft's outputs are not the contract's two then at most one P2PKH change.
pub fn unlockClose(a: std.mem.Allocator, l: Located, key_id: []const u8, oracle: Oracle) ![]const u8 {
    const outs = try closeOutputs(a, l.pool, l.satoshis);
    const t = l.tx;
    if (t.outputs.len < 2 or t.outputs.len > 3) return error.NotTheContractsOutputs;
    for (outs, t.outputs[0..2]) |want, got| {
        if (@as(u64, @intCast(got.satoshis)) != want.satoshis or !std.mem.eql(u8, got.locking_script.bytes, want.script)) return error.NotTheContractsOutputs;
    }
    var change_pkh: [20]u8 = .{0} ** 20;
    var change: u64 = 0;
    if (t.outputs.len == 3) {
        change_pkh = unlock.p2pkhHash(t.outputs[2].locking_script.bytes) orelse return error.ChangeNotP2pkh;
        change = @intCast(t.outputs[2].satoshis);
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
        &.{0x00}, // bsvFee 0
        try unlock.pushData(a, &change_pkh),
        try pushNum(a, change),
        try unlock.pushData(a, pre),
        &.{0x51}, // Close
    });
}

/// The pool's current validator key ID (validator.zig `currentKeyId`): the first token input of
/// the transaction that created the pool output, checked against its ValidatorPubKey.
pub fn keyIdOf(a: std.mem.Allocator, v: View, identity: [33]u8, l: Located) !?[]const u8 {
    const op = (try validator.currentKeyId(a, v, l.pool.asset_id, identity, l.op, l.pool.validator)) orelse return null;
    const buf = try a.create([64 + 1 + 10]u8);
    return try a.dupe(u8, pool.keyId(buf, op.txid, op.vout));
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

/// Where a Close is, from the wallet thread a step was stepped by (its `resolved`): the wallet's
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

/// The topic the Close is submitted for: the pool's token's.
pub fn topicOf(a: std.mem.Allocator, p: pool.Pool) ![]u8 {
    return validator.topicOf(a, p.asset_id);
}
