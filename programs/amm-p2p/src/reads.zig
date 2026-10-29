//! The pages' reads (skein-amm 0.9.0, David Case 2026-10-09). Pure: over a store, the chain state
//! (main.zig wires the VM). (The holders' listing requests moved to the config app in 0.17.0
//! and were removed in 0.19.0.)
//!
//! - **Spends** (`/spends?outpoint=<txid>.<vout>`, filter `spends`): what the chain state says of
//!   an outpoint — the LP page follows its pool from the deploy's outpoint to the current pool
//!   output, or to the Close that ended it (the LP's or, 0.11.0, the validator's) (each spender's
//!   output 0 while it is a pool) — `{outpoint, spentBy?, current?, hops, closed}`. The chain
//!   app's `chain/state` (its `spent` map), read only, as the overlay reads it.
const std = @import("std");
const w = @import("chain");
const color = @import("1color");
const pool = @import("pool");

const cbor = w.cbor;
const Value = cbor.Value;
const Allocator = std.mem.Allocator;
const Chain = w.state.State;
const Transaction = w.bsvz.transaction.Transaction;
const eql = std.mem.eql;

// ---------------------------------------------------------------- spends

pub const Outpoint = struct { txid: [32]u8, vout: u32 };

/// `<txid>.<vout>` or `<txid>_<vout>`.
pub fn parseOutpoint(text: []const u8) ?Outpoint {
    if (text.len < 66 or (text[64] != '.' and text[64] != '_')) return null;
    const txid = w.header.fromHex(text[0..64]) catch return null;
    const vout = std.fmt.parseInt(u32, text[65..], 10) catch return null;
    return .{ .txid = txid, .vout = vout };
}

/// A query string's `outpoint` value.
pub fn queryOutpoint(query: []const u8) ?Outpoint {
    var it = std.mem.splitScalar(u8, std.mem.trimStart(u8, query, "?"), '&');
    while (it.next()) |kv| {
        if (std.mem.startsWith(u8, kv, "outpoint=")) return parseOutpoint(kv["outpoint=".len..]);
    }
    return null;
}

pub const Spend = struct {
    outpoint: Outpoint,
    spent_by: ?[32]u8,
    /// Following pool spends: the current pool output (the outpoint itself when unspent).
    current: ?Outpoint,
    hops: u32,
    /// A pool spent by a transaction that did not continue it (Close).
    closed: bool,
};

fn isPool(script: []const u8) bool {
    const tok = color.brc162.decode(script) orelse return false;
    if (tok.role != .value) return false;
    return (pool.parse(tok.lock) catch null) != null;
}

/// What the chain state says of `op`: its spender, and (when `op` is a pool output) the pool's
/// current output, followed spender to spender while each spender's output 0 is a pool.
pub fn spendOf(a: Allocator, ch: *Chain, op: Outpoint) !Spend {
    var out: Spend = .{ .outpoint = op, .spent_by = try ch.spentBy(op.txid, op.vout), .current = null, .hops = 0, .closed = false };
    const raw = (try ch.txRaw(op.txid)) orelse return out;
    const t = Transaction.parse(a, raw) catch return out;
    if (op.vout >= t.outputs.len or !isPool(t.outputs[op.vout].locking_script.bytes)) return out;
    var cur = op;
    var steps: u32 = 0;
    while (steps < 100_000) : (steps += 1) {
        const sp = (try ch.spentBy(cur.txid, cur.vout)) orelse {
            out.current = cur;
            return out;
        };
        const sraw = (try ch.txRaw(sp)) orelse return out;
        const st = Transaction.parse(a, sraw) catch return out;
        if (st.outputs.len == 0 or !isPool(st.outputs[0].locking_script.bytes)) {
            out.closed = true;
            return out;
        }
        cur = .{ .txid = sp, .vout = 0 };
        out.hops += 1;
    }
    return out;
}

/// The `/spends` answer's JSON: `{outpoint, spentBy?, current?, hops, closed}`, outpoints `<txid>.<vout>`.
pub fn spendJson(a: Allocator, s: Spend) ![]const u8 {
    var out: std.Io.Writer.Allocating = .init(a);
    var jw: std.json.Stringify = .{ .writer = &out.writer };
    try jw.beginObject();
    try jw.objectField("outpoint");
    try jw.write(try std.fmt.allocPrint(a, "{s}.{d}", .{ &w.header.toHex(s.outpoint.txid), s.outpoint.vout }));
    if (s.spent_by) |sb| {
        try jw.objectField("spentBy");
        try jw.write(&w.header.toHex(sb));
    }
    if (s.current) |c| {
        try jw.objectField("current");
        try jw.write(try std.fmt.allocPrint(a, "{s}.{d}", .{ &w.header.toHex(c.txid), c.vout }));
    }
    try jw.objectField("hops");
    try jw.write(s.hops);
    try jw.objectField("closed");
    try jw.write(s.closed);
    try jw.endObject();
    return out.written();
}

/// An http answer `{status, type, body}`.
pub fn httpJson(a: Allocator, status: u16, body: []const u8) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "status", .value = .{ .uint = status } },
        .{ .key = "type", .value = .{ .text = "application/json" } },
        .{ .key = "body", .value = .{ .bytes = body } },
    }) };
}
