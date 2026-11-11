//! Getting a signed transaction into this instance's own overlay: the
//! validator only adds its signature; the admission is the overlay engine's.
//! 0.51.0 (David 2026-11-11: one step per swap): the validator calls the
//! engine's in-VM `intake` (skein-overlay 0.28.0) from its own step, args
//! `{beef, topics: [tm_color_<txid>_0]}`, with the signed BEEF — the request's
//! BEEF (the taker's ancestry and BUMPs) with its subject replaced by the signed
//! transaction, every transaction the token's topic admitted (the pool's among
//! them) named by txid alone (`submissionBeef`, 0.45.0: skein 0.3.3's door admits
//! a txid-only entry for a held transaction). The engine routes it (decoded,
//! checked against the chain app's headers and SPV, judged by the topic's
//! program), hands it to the chain app (recorded, its broadcast emitted) and
//! admits it, all in the validator's step, and returns its verdict
//! (messages.zig `answerFromSubmit` maps it). The message form (`{fn: "submit"}`
//! into `<app>/submit`, awaited) is gone from the signing path.
const std = @import("std");
const w = @import("chain");

const cbor = w.cbor;
const Value = cbor.Value;
const beef = w.beef;

/// The engine's in-VM `intake` argument (skein-overlay 0.28.0): `{beef: <bytes>, topics: [topic]}`.
pub fn intakeArgs(a: std.mem.Allocator, signed_beef: []const u8, topic: []const u8) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "beef", .value = .{ .bytes = signed_beef } },
        .{ .key = "topics", .value = .{ .array = try a.dupe(Value, &.{.{ .text = topic }}) } },
    }) };
}

/// The signed BEEF, what a submission carries (0.45.0, David 2026-11-07): the request's with its
/// subject replaced by the signed transaction (same place, parents first), or the signed transaction
/// alone (a raw request) — every transaction the token's topic admitted (`topic.has`: the pool's
/// transaction, a swap before it) named by txid alone, the pool's (`pool_txid`) added so when absent,
/// and nothing left that only those needed: the taker's raw funding with its ancestry back to proven
/// (its BUMPs) is what stays raw. The topic admitted them, so this instance holds them, and skein
/// 0.3.3's door admits a txid-only entry for a held transaction (`kernel.beef`'s `knownRaw` the held
/// set); no `beefFor` merge, no ancestry walk of the chain state. The signed transaction stays the
/// subject: an Atomic BEEF names it, a bare one ends with it.
pub fn submissionBeef(a: std.mem.Allocator, signed: []const u8, ancestry: ?beef.Beef, pool_txid: ?[32]u8, topic: anytype) ![]u8 {
    const txid = beef.txidOf(signed);
    const parsed = try w.bsvz.transaction.Transaction.parse(a, signed);
    var out: beef.Beef = if (ancestry) |b| blk: {
        const subject = b.subject() orelse return error.InvalidBeef;
        const entries = try a.dupe(beef.Entry, b.entries);
        const i = b.indexOf(subject) orelse return error.InvalidBeef;
        entries[i] = .{ .txid = txid, .format = .raw, .raw = signed, .tx = parsed };
        var o = b;
        o.entries = entries;
        if (o.atomic != null) o.atomic = txid;
        break :blk o;
    } else .{
        .version = beef.V1,
        .bumps = &.{},
        .entries = try a.dupe(beef.Entry, &.{.{ .txid = txid, .format = .raw, .raw = signed, .tx = parsed }}),
    };
    out = try topicByTxid(a, out, txid, pool_txid, topic);
    if ((out.subject() == null) or !std.mem.eql(u8, &out.subject().?, &txid)) return error.InvalidBeef;
    return beef.serialize(a, out);
}

/// Whether the instance holds `txid` (the chain state's `txs`, not rejected): what a BEEF may name by
/// txid alone at this instance's door (skein 0.3.3).
pub fn holds(ch: anytype, txid: [32]u8) !bool {
    return (try ch.holds(txid)) and (try ch.status(txid)) != .rejected;
}

/// `b` (its subject `subject`) with every transaction but the subject that the topic admitted a
/// txid-only entry, `extra` added as one when absent, then only what the subject's raw ancestry
/// reaches (a txid-only or a BUMP-proven entry ends a branch) and the BUMPs those keep; txid-only
/// entries first, then parents first (skein-sdk `beef.sorted`); V2 when any entry is txid-only.
fn topicByTxid(a: std.mem.Allocator, b: beef.Beef, subject: [32]u8, extra: ?[32]u8, topic: anytype) !beef.Beef {
    var entries: std.ArrayList(beef.Entry) = .empty;
    for (b.entries) |e| {
        if (!std.mem.eql(u8, &e.txid, &subject) and e.format != .txid_only and try topic.has(e.txid)) {
            try entries.append(a, .{ .txid = e.txid, .format = .txid_only });
        } else try entries.append(a, e);
    }
    if (extra) |x| {
        const have = for (entries.items) |*e| {
            if (std.mem.eql(u8, &e.txid, &x)) {
                e.* = .{ .txid = x, .format = .txid_only };
                break true;
            }
        } else false;
        if (!have) try entries.append(a, .{ .txid = x, .format = .txid_only });
    }
    // What the subject reaches: raw entries' inputs, back to txid-only or proven ones.
    const keep = try a.alloc(bool, entries.items.len);
    @memset(keep, false);
    var stack: std.ArrayList([32]u8) = .empty;
    try stack.append(a, subject);
    while (stack.pop()) |t| {
        const k = for (entries.items, 0..) |e, i| {
            if (std.mem.eql(u8, &e.txid, &t)) break i;
        } else continue;
        if (keep[k]) continue;
        keep[k] = true;
        const e = entries.items[k];
        if (e.format != .raw) continue;
        const tx = e.tx orelse try w.bsvz.transaction.Transaction.parse(a, e.raw orelse continue);
        for (tx.inputs) |in| try stack.append(a, in.previous_outpoint.txid.bytes);
    }
    // The pool's transaction stays named.
    if (extra) |x| for (entries.items, 0..) |e, i| {
        if (std.mem.eql(u8, &e.txid, &x)) keep[i] = true;
    };
    var bumps: std.ArrayList(w.bsvz.spv.MerklePath) = .empty;
    const remap = try a.alloc(?usize, b.bumps.len);
    @memset(remap, null);
    var txid_only: std.ArrayList(beef.Entry) = .empty;
    var rest: std.ArrayList(beef.Entry) = .empty;
    for (entries.items, keep) |e, k| {
        if (!k) continue;
        var n = e;
        if (e.bump) |bi| {
            if (remap[bi] == null) {
                remap[bi] = bumps.items.len;
                try bumps.append(a, b.bumps[bi]);
            }
            n.bump = remap[bi];
        }
        if (e.format == .txid_only) try txid_only.append(a, n) else try rest.append(a, n);
    }
    var o = b;
    if (txid_only.items.len > 0) o.version = beef.V2;
    o.bumps = bumps.items;
    o.entries = try std.mem.concat(a, beef.Entry, &.{ txid_only.items, rest.items });
    return beef.sorted(a, o);
}

/// The `beef` a `pool_spent` refusal carries (0.45.0): the transaction of the pool's newest state
/// (`txid`) in the same shape as a submission — every transaction the topic admitted (the pool's
/// transaction before it, a swap) named by txid, the others (the other taker's funding) raw with
/// their ancestry back to proven (their BUMPs, from the chain state `ch`), the newest itself raw (with
/// its BUMP once mined). An Atomic BEEF V2.
pub fn newestBeef(a: std.mem.Allocator, ch: anytype, txid: [32]u8, topic: anytype) ![]const u8 {
    var acc = Acc{ .a = a };
    try acc.visit(ch, topic, txid, txid);
    for (acc.entries.items) |e| {
        const bi = e.bump orelse continue;
        for (acc.bumps.items[bi].path[0]) |*l| if (l.hash) |h| if (std.mem.eql(u8, &h.bytes, &e.txid)) {
            l.txid = true;
        };
    }
    return beef.serialize(a, .{ .version = beef.V2, .atomic = txid, .bumps = acc.bumps.items, .entries = acc.entries.items });
}

const Acc = struct {
    a: std.mem.Allocator,
    entries: std.ArrayList(beef.Entry) = .empty,
    bumps: std.ArrayList(w.bsvz.spv.MerklePath) = .empty,

    fn visit(acc: *Acc, ch: anytype, topic: anytype, subject: [32]u8, txid: [32]u8) anyerror!void {
        const a = acc.a;
        for (acc.entries.items) |e| if (std.mem.eql(u8, &e.txid, &txid)) return;
        if (!std.mem.eql(u8, &txid, &subject) and try topic.has(txid)) {
            try acc.entries.append(a, .{ .txid = txid, .format = .txid_only });
            return;
        }
        const raw = (try ch.txRaw(txid)) orelse return error.MissingAncestor;
        const tx = try w.bsvz.transaction.Transaction.parse(a, raw);
        if ((try ch.status(txid)) == .proven) {
            const p = (try ch.proofFor(txid)) orelse return error.MissingProof;
            const idx = for (acc.bumps.items, 0..) |*q, i| {
                if (q.block_height != p.block_height) continue;
                q.combine(&p, a) catch continue;
                break i;
            } else blk: {
                try acc.bumps.append(a, p);
                break :blk acc.bumps.items.len - 1;
            };
            try acc.entries.append(a, .{ .txid = txid, .format = .raw_with_bump, .bump = idx, .raw = raw, .tx = tx });
            return;
        }
        for (tx.inputs) |in| try acc.visit(ch, topic, subject, in.previous_outpoint.txid.bytes);
        try acc.entries.append(a, .{ .txid = txid, .format = .raw, .raw = raw, .tx = tx });
    }
};

/// A signed BEEF (the wallet's Atomic BEEF of a Rescind, 0.29.5) merged with `pool`, the Atomic
/// BEEF of the pool's transaction (`merged`): the subject unchanged.
pub fn withPool(a: std.mem.Allocator, signed_beef: []const u8, pool: []const u8) ![]u8 {
    const b = try beef.parse(a, signed_beef);
    const subject = b.subject() orelse return error.InvalidBeef;
    const out = try merged(a, b, try beef.parse(a, pool));
    if ((out.subject() == null) or !std.mem.eql(u8, &out.subject().?, &subject)) return error.InvalidBeef;
    return beef.serialize(a, out);
}

/// `b` with every transaction of `x` it lacks (0.29.5): `x`'s BUMPs combined with `b`'s per block
/// (as the chain state's BEEF writer does), a transaction both carry kept once (`x`'s when only it
/// has a BUMP), then parents first (skein-sdk `beef.sorted`), each proven transaction's leaf
/// flagged in its BUMP. `b`'s envelope (version, atomic subject) is kept.
pub fn merged(a: std.mem.Allocator, b: beef.Beef, x: beef.Beef) !beef.Beef {
    var bumps: std.ArrayList(w.bsvz.spv.MerklePath) = .empty;
    try bumps.appendSlice(a, b.bumps);
    const entries = try a.dupe(beef.Entry, b.entries);
    var added: std.ArrayList(beef.Entry) = .empty;
    for (x.entries) |e| {
        const have = b.indexOf(e.txid);
        if (have) |i| if (entries[i].format == .raw_with_bump or e.format != .raw_with_bump) continue;
        var n = e;
        if (e.bump) |bi| n.bump = try addBump(a, &bumps, x.bumps[bi]);
        if (n.tx == null) if (n.raw) |raw| {
            n.tx = try w.bsvz.transaction.Transaction.parse(a, raw);
        };
        if (have) |i| entries[i] = n else try added.append(a, n);
    }
    var out = b;
    out.bumps = bumps.items;
    out.entries = try std.mem.concat(a, beef.Entry, &.{ added.items, entries });
    for (out.entries) |e| {
        const bi = e.bump orelse continue;
        for (out.bumps[bi].path[0]) |*l| if (l.hash) |h| if (std.mem.eql(u8, &h.bytes, &e.txid)) {
            l.txid = true;
        };
    }
    return beef.sorted(a, out);
}

fn addBump(a: std.mem.Allocator, bumps: *std.ArrayList(w.bsvz.spv.MerklePath), p: w.bsvz.spv.MerklePath) !usize {
    for (bumps.items, 0..) |*q, i| {
        if (q.block_height != p.block_height) continue;
        q.combine(&p, a) catch continue;
        return i;
    }
    try bumps.append(a, p);
    return bumps.items.len - 1;
}

/// D6 (David 2026-11-07): the request's BEEF proven as this instance's door will prove the submission,
/// before the signature — skein-sdk's verifier (chain `spv.verify`, what skein
/// 0.3.3's `kernel.beef` runs) with `rootAt` the chain state's headers and `knownRaw` the held set
/// (`holds`: in the chain state's `txs`, not rejected), over every transaction of the request but the
/// subject; then the subject's own inputs as the verifier reads an unproven transaction's: each
/// source earlier in the BEEF or held, each script verified but the pool input's (`pool`: its slot is
/// ours, empty until we sign). `ancestry` null: a raw request, every source held. Null when it
/// passes; else the refusal's text, the verifier's error name first, then what it means, and the
/// transaction or input it is about where known (an unknown txid-only entry, a missing source).
pub fn requestProblem(a: std.mem.Allocator, ancestry: ?beef.Beef, subject: w.bsvz.transaction.Transaction, pool: struct { txid: [32]u8, vout: u32 }, ch: anytype) !?[]const u8 {
    const Ch = @TypeOf(ch);
    const Ctx = struct {
        ch: Ch,
        /// The last txid `knownRaw` was asked for and does not hold: what an UnknownTxidOnly or a
        /// MissingInput is about.
        unknown: ?[32]u8 = null,
        fn rootAt(ptr: *anyopaque, height: u32) anyerror!?[32]u8 {
            const c: *@This() = @ptrCast(@alignCast(ptr));
            return c.ch.chain().rootAt(height);
        }
        fn knownRaw(ptr: *anyopaque, _: std.mem.Allocator, txid: [32]u8) anyerror!?[]const u8 {
            const c: *@This() = @ptrCast(@alignCast(ptr));
            const raw = if (try holds(c.ch, txid)) try c.ch.txRaw(txid) else null;
            if (raw == null) c.unknown = txid;
            return raw;
        }
    };
    var ctx = Ctx{ .ch = ch };
    const vctx: w.spv.Context = .{ .ptr = &ctx, .rootAtFn = Ctx.rootAt, .knownRawFn = Ctx.knownRaw };
    const subject_txid = (try subject.txid(a)).bytes;
    if (ancestry) |b| {
        var rest: std.ArrayList(beef.Entry) = .empty;
        for (b.entries) |e| if (!std.mem.eql(u8, &e.txid, &subject_txid)) try rest.append(a, e);
        var others = b;
        others.entries = rest.items;
        others.atomic = null;
        _ = w.spv.verify(a, others, vctx) catch |e| switch (e) {
            error.OutOfMemory => return error.OutOfMemory,
            else => return try why(a, e, ctx.unknown, null),
        };
    }
    for (subject.inputs, 0..) |in, k| {
        const src_txid = in.previous_outpoint.txid.bytes;
        ctx.unknown = null;
        const in_beef: ?beef.Entry = if (ancestry) |b| (if (b.find(src_txid)) |e| e.* else null) else null;
        const src_raw = (if (in_beef) |e| (e.raw orelse try Ctx.knownRaw(&ctx, a, src_txid)) else try Ctx.knownRaw(&ctx, a, src_txid)) orelse
            return try why(a, error.MissingInput, src_txid, k);
        const src = w.bsvz.transaction.Transaction.parse(a, src_raw) catch return try why(a, error.MissingInput, src_txid, k);
        if (in.previous_outpoint.index >= src.outputs.len) return try why(a, error.MissingInput, src_txid, k);
        if (in.previous_outpoint.index == pool.vout and std.mem.eql(u8, &src_txid, &pool.txid)) continue;
        const ok = w.bsvz.script.interpreter.verifyPrevout(.{
            .allocator = a,
            .tx = &subject,
            .input_index = k,
            .previous_output = src.outputs[in.previous_outpoint.index],
            .unlocking_script = in.unlocking_script,
        }) catch false;
        if (!ok) return try why(a, error.ScriptFailed, null, k);
    }
    return null;
}

/// The refusal's text for a verifier error (the meanings skein's door gives them, door.zig `spvWhy`).
fn why(a: std.mem.Allocator, e: anyerror, txid: ?[32]u8, input: ?usize) ![]const u8 {
    const meaning = switch (e) {
        error.UnknownHeader => "a BUMP's height has no header in chain/state",
        error.RootMismatch => "a BUMP's merkle root is not the header's at its height in chain/state",
        error.NotInBump => "a transaction marked with a BUMP is not in it",
        error.MissingInput => "a transaction no BUMP proves spends an output of a transaction neither before it in the BEEF nor held here",
        error.UnknownTxidOnly => "a txid-only entry for a transaction this instance does not hold (only a held one may be named by txid alone)",
        error.ScriptFailed => "an unlocking script of a transaction no BUMP proves does not verify",
        error.NotParentsFirst => "its transactions spend in a cycle: no order puts parents first",
        else => "the BEEF's check failed",
    };
    var out: std.ArrayList(u8) = .empty;
    try out.print(a, "{s}: {s}", .{ @errorName(e), meaning });
    if (txid) |t| try out.print(a, " ({s})", .{&w.header.toHex(t)});
    if (input) |k| try out.print(a, " (the swap's input {d})", .{k});
    return out.items;
}
