//! The pages' reads (skein-amm 0.9.0, David Case 2026-10-09). Pure: over a store, the chain state
//! (main.zig wires the VM). (The holders' listing requests moved to the config app in 0.17.0
//! and were removed in 0.19.0.)
//!
//! - **Spends** (`/spends?outpoint=<txid>.<vout>`, filter `spends`): an outpoint's spender, ONE
//!   lookup in the chain app's `chain/state` (its `spent` map), read only, as the overlay reads it —
//!   `{outpoint, spentBy?}` (0.47.0, agent-env plans/read-optimizations-2026-11-07.md D2: the
//!   forward pool-following, `current`, `hops`, `closed`, a full transaction parse per hop, is gone;
//!   following a pool is `/pools/history`). 0.34.0: a list (`?outpoint=a,b`) answers one record per
//!   outpoint in query order (P8).
//!
//! **The public read API** (0.34.0, docs/AMM.md "The public read API"; David Case 2026-11-04,
//! D1/D7/D8): plain GETs, JSON unless the row says bytes, list parameters comma-separated, one
//! record per key in query order, cache headers per class (`state`: `public, max-age=5`; `imm`:
//! `public, max-age=31536000, immutable`, only for the bytes of a proven transaction). The pool
//! routes answer from the pool lookup's own answers (amm-lookup index.zig `answer`, the same code
//! over the same index, the head `amm/ls_amm`):
//!
//! - (0.47.0, D1: P3 `/pools/beef` is gone; a pool's transaction is P9 `/tx?id=` of its current
//!   outpoint's txid, which the pools list carries)
//! - P4 `/positions?pool=`: the lookup's `{poolIds}` answer as JSON (0.49.0: P5 `/fees/validator`, P6
//!   `/fees/commission`, `/pools/at` and `/pools/history` gone: the host folds the pool events);
//! - (0.49.0) `/fees/tokens`: the token fee tally (`tallyJson`);
//! - P7 `/terms`: the validator's terms and the market's, in one (`termsJson`);
//! - P9 `/tx?id=`: a transaction's BEEF (`imm` when proven);
const std = @import("std");
const w = @import("chain");
const lookup = @import("lookup");
const idx = @import("amm_lookup");

const cbor = w.cbor;
const Value = cbor.Value;
const Allocator = std.mem.Allocator;
const Chain = w.state.State;
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

/// A query string's parameter `name`, percent-decoded; null when absent.
pub fn queryParam(a: Allocator, query: []const u8, name: []const u8) !?[]const u8 {
    var it = std.mem.splitScalar(u8, std.mem.trimStart(u8, query, "?"), '&');
    while (it.next()) |kv| {
        const eq = std.mem.indexOfScalar(u8, kv, '=') orelse continue;
        if (!eql(u8, kv[0..eq], name)) continue;
        return try percentDecode(a, kv[eq + 1 ..]);
    }
    return null;
}

fn percentDecode(a: Allocator, s: []const u8) ![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    var i: usize = 0;
    while (i < s.len) : (i += 1) {
        if (s[i] == '%' and i + 2 < s.len) {
            const b = std.fmt.parseInt(u8, s[i + 1 .. i + 3], 16) catch {
                try out.append(a, s[i]);
                continue;
            };
            try out.append(a, b);
            i += 2;
        } else try out.append(a, if (s[i] == '+') ' ' else s[i]);
    }
    return out.items;
}

/// A list parameter (D1: comma-separated values in one parameter, `?lp=02aa…,02bb…`): its values in
/// order, empty ones skipped; null when the parameter is absent.
pub fn queryList(a: Allocator, query: []const u8, name: []const u8) !?[]const []const u8 {
    const v = (try queryParam(a, query, name)) orelse return null;
    var out: std.ArrayList([]const u8) = .empty;
    var it = std.mem.splitScalar(u8, v, ',');
    while (it.next()) |x| {
        const t = std.mem.trim(u8, x, " ");
        if (t.len > 0) try out.append(a, t);
    }
    return out.items;
}

pub const Spend = struct {
    outpoint: Outpoint,
    spent_by: ?[32]u8,
};

/// What the chain state says of `op`: its spender (the first held spender not rejected), one lookup.
pub fn spendOf(ch: *Chain, op: Outpoint) !Spend {
    return .{ .outpoint = op, .spent_by = try ch.spentBy(op.txid, op.vout) };
}

/// The `/spends` answer's JSON: `{outpoint, spentBy?}`, the outpoint `<txid>.<vout>`.
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

// ---------------------------------------------------------------- the public read API (0.34.0)

/// The cache classes (the route table's): `state` a short public cache; `imm` immutable, only for
/// the bytes of a proven transaction.
pub const Cache = enum {
    state,
    imm,

    pub fn header(c: Cache) []const u8 {
        return switch (c) {
            .state => "public, max-age=5",
            .imm => "public, max-age=31536000, immutable",
        };
    }
};

/// An http answer `{status, type, headers: {cache-control}, body}`.
pub fn httpCached(a: Allocator, status: u16, content_type: []const u8, cache: Cache, body: []const u8) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "status", .value = .{ .uint = status } },
        .{ .key = "type", .value = .{ .text = content_type } },
        .{ .key = "headers", .value = .{ .map = try a.dupe(cbor.Entry, &.{.{ .key = "cache-control", .value = .{ .text = cache.header() } }}) } },
        .{ .key = "body", .value = .{ .bytes = body } },
    }) };
}

/// `{"error": <text>}` with `status` (no cache header).
pub fn httpError(a: Allocator, status: u16, text: []const u8) !Value {
    var out: std.Io.Writer.Allocating = .init(a);
    var jw: std.json.Stringify = .{ .writer = &out.writer };
    try jw.beginObject();
    try jw.objectField("error");
    try jw.write(text);
    try jw.endObject();
    return httpJson(a, status, out.written());
}

/// A value (maps, lists, text, numbers, booleans, null) as JSON: the lookup's freeform answers.
pub fn writeValue(jw: *std.json.Stringify, v: Value) !void {
    switch (v) {
        .map => |es| {
            try jw.beginObject();
            for (es) |e| {
                try jw.objectField(e.key);
                try writeValue(jw, e.value);
            }
            try jw.endObject();
        },
        .array => |xs| {
            try jw.beginArray();
            for (xs) |x| try writeValue(jw, x);
            try jw.endArray();
        },
        .text => |t| try jw.write(t),
        .boolean => |b| try jw.write(b),
        .uint => |n| try jw.write(n),
        .nint => |n| try jw.write(-1 - @as(i128, n)),
        .null => try jw.write(null),
        else => return error.BadValue,
    }
}

/// Lowercase hex of `bytes`.
pub fn hexAlloc(a: Allocator, bytes: []const u8) ![]const u8 {
    const digits = "0123456789abcdef";
    const out = try a.alloc(u8, bytes.len * 2);
    for (bytes, 0..) |b, i| {
        out[i * 2] = digits[b >> 4];
        out[i * 2 + 1] = digits[b & 0xf];
    }
    return out;
}

pub fn valueJson(a: Allocator, v: Value) ![]const u8 {
    var out: std.Io.Writer.Allocating = .init(a);
    var jw: std.json.Stringify = .{ .writer = &out.writer };
    try writeValue(&jw, v);
    return out.written();
}

/// The pool lookup's index (amm-lookup's head `<app>/ls_amm`'s record `state`, null: none yet), as
/// amm-lookup loads it.
pub fn loadLookup(a: Allocator, s: w.store.Store, state: ?[]const u8) !lookup.Service {
    return lookup.Service.loadIndex(a, s, idx.service_name, idx.service_name, &idx.maps, state);
}

/// The pool lookup's freeform answer to `{tokenId, <field>: keys}` (`field` null: `{tokenId}`, every
/// live pool) — amm-lookup's `answer`, the code `/lookup` runs.
pub fn lookupAnswer(a: Allocator, svc: *lookup.Service, ch: *Chain, token: []const u8, field: ?[]const u8, keys: []const []const u8) !Value {
    var es: std.ArrayList(cbor.Entry) = .empty;
    try es.append(a, .{ .key = "tokenId", .value = .{ .text = token } });
    if (field) |f| {
        const vs = try a.alloc(Value, keys.len);
        for (keys, vs) |k, *v| v.* = .{ .text = k };
        try es.append(a, .{ .key = f, .value = .{ .array = vs } });
    }
    const ans = try idx.answer(a, svc, ch, .{ .map = es.items });
    return switch (ans) {
        .freeform => |v| v,
        .output_list => error.BadQuery,
    };
}

/// P8's JSON over several outpoints: one record per outpoint in query order (`spendJson` each).
pub fn spendsJson(a: Allocator, ss: []const Spend) ![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    try out.append(a, '[');
    for (ss, 0..) |s, i| {
        if (i > 0) try out.append(a, ',');
        try out.appendSlice(a, try spendJson(a, s));
    }
    try out.append(a, ']');
    return out.items;
}

/// The validator's side of P7: this instance's identity and peer ID with its terms.
pub const ValidatorTerms = struct {
    identity: ?[]const u8,
    peer_id: ?[]const u8,
    lp_fee_bps: u64,
    validator_fee_bps: u64,
    commission_bps: u64,
    /// The validator's minimum leg, sats (0.40.0): 0, none.
    min_leg_sats: u64 = 0,
};

/// P7's JSON: `{validator: {identity, peerId, lpFeeBps, validatorFeeBps, commissionBps, minLegSats} | null, market:
/// {identityKey, minCommissionBps}}` (0.49.0: the market's `identityKey`, the identity each leg's
/// commission key derives from, src/pool.zig `commissionPkh`; `commissionPkh` gone) — `amm.pool.terms` and `amm.swap.terms` in one, unsigned
/// (`validator` null: no terms set, a market only; `identity`/`peerId`/`commissionPkh` null when the
/// signer gives none).
pub fn termsJson(a: Allocator, v: ?ValidatorTerms, identity: ?[]const u8, min_commission_bps: u64) ![]const u8 {
    var out: std.Io.Writer.Allocating = .init(a);
    var jw: std.json.Stringify = .{ .writer = &out.writer };
    try jw.beginObject();
    try jw.objectField("validator");
    if (v) |t| {
        try jw.beginObject();
        try jw.objectField("identity");
        if (t.identity) |k| try jw.write(try hexAlloc(a, k)) else try jw.write(null);
        try jw.objectField("peerId");
        try jw.write(t.peer_id);
        try jw.objectField("lpFeeBps");
        try jw.write(t.lp_fee_bps);
        try jw.objectField("validatorFeeBps");
        try jw.write(t.validator_fee_bps);
        try jw.objectField("commissionBps");
        try jw.write(t.commission_bps);
        try jw.objectField("minLegSats");
        try jw.write(t.min_leg_sats);
        try jw.endObject();
    } else try jw.write(null);
    try jw.objectField("market");
    try jw.beginObject();
    try jw.objectField("identityKey");
    if (identity) |k| try jw.write(try hexAlloc(a, k)) else try jw.write(null);
    try jw.objectField("minCommissionBps");
    try jw.write(min_commission_bps);
    try jw.endObject();
    try jw.endObject();
    return out.written();
}

/// A txid in display hex (64 characters), parsed.
pub fn parseTxid(text: []const u8) ?[32]u8 {
    if (text.len != 64) return null;
    return w.header.fromHex(text) catch null;
}

/// P9's cache class: `imm` only when the transaction is proven in the chain state (mined, its BUMP
/// against the chain's headers), else `state`.
pub fn txCache(ch: *Chain, txid: [32]u8) !Cache {
    return if ((try ch.status(txid)) == .proven) .imm else .state;
}

// ---------------------------------------------------------------- the token fee tally (0.49.0)

/// `/fees/tokens`'s JSON: `{tokenId, units}`, the token units of this instance's own fee outputs
/// still unspent (amm-lookup `tally`).
pub fn tallyJson(a: Allocator, token: []const u8, units: u64) ![]const u8 {
    var out: std.Io.Writer.Allocating = .init(a);
    var jw: std.json.Stringify = .{ .writer = &out.writer };
    try jw.beginObject();
    try jw.objectField("tokenId");
    try jw.write(token);
    try jw.objectField("units");
    try jw.write(units);
    try jw.endObject();
    return out.written();
}
