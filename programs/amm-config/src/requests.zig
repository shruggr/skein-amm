//! The holders' listing requests (skein-amm 0.9.0, David Case 2026-10-09; moved from amm-p2p to the
//! config app in 0.17.0, shruggr/skein-amm#4: the token list is the config instance's). Pure: over
//! a store and maps (main.zig wires the VM).
//!
//! A holder asks this exchange to list a token, a message `{fn: "request", args: {tokenId}}` in
//! the box `amm/requests` from anyone; recorded under the head `amm/requests` (`{kind:
//! "amm-requests", requests: <map: tokenId → record>}`, each record `{tokenId, from, at}`, the
//! latest per token). Nothing else runs. The read route `/requests` (filter `requests`) lists them,
//! newest first, less the tokens on the enable list (an enable settles a request).
const std = @import("std");
const w = @import("chain");
const color = @import("1color");

const cbor = w.cbor;
const Value = cbor.Value;
const Allocator = std.mem.Allocator;
const eql = std.mem.eql;

pub const requests_head = "amm/requests";
pub const requests_box = "amm/requests";
pub const requests_kind = "amm-requests";

/// A token id as the pages write it: `<txid>_<vout>` (BRC-207's assetId), any form parsed.
pub fn tokenIdText(a: Allocator, text: []const u8) ![]const u8 {
    const id = color.name.tokenIdOfString(text) orelse return error.BadTokenId;
    return std.fmt.allocPrint(a, "{s}_{d}", .{ &w.header.toHex(id.txid), id.vout });
}

/// The requests record's map, loaded from the head's record (null: none yet).
pub fn requestsMap(a: Allocator, s: w.store.Store, rec: ?Value) !w.store.Map {
    const maps = try w.store.Maps.create(a, s);
    const root = if (rec) |r| (if (eql(u8, r.getText("kind") orelse "", requests_kind)) r.getCid("requests") else return error.BadState) else null;
    return maps.map(root);
}

/// Record a holder's request for `token_id` (any form) from `from` at `at`: → the new head record.
pub fn record(a: Allocator, s: w.store.Store, prev: ?Value, token_id: []const u8, from: []const u8, at: u64) !Value {
    var m = try requestsMap(a, s, prev);
    const id = try tokenIdText(a, token_id);
    const rc = try s.putValue(a, .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "tokenId", .value = .{ .text = id } },
        .{ .key = "from", .value = .{ .bytes = from } },
        .{ .key = "at", .value = .{ .uint = at } },
    }) });
    try m.putLink(id, rc);
    try m.flush();
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "kind", .value = .{ .text = requests_kind } },
        .{ .key = "requests", .value = if (m.root) |r| .{ .cid = r } else .null },
    }) };
}

pub const Request = struct { token_id: []const u8, from: []const u8, at: u64 };

/// The requests, newest first, less the tokens whose topic is in `settled_topics` (the enable list's).
pub fn list(a: Allocator, s: w.store.Store, rec: ?Value, settled_topics: []const []const u8) ![]Request {
    var m = try requestsMap(a, s, rec);
    var out: std.ArrayList(Request) = .empty;
    for (try m.prefixed(&.{})) |kv| {
        if (kv.value != .cid) continue;
        const r = try s.getValue(a, kv.value.cid);
        const id = r.getText("tokenId") orelse continue;
        const topic = try std.fmt.allocPrint(a, "tm_color_{s}", .{id});
        const settled = for (settled_topics) |t| {
            if (eql(u8, t, topic)) break true;
        } else false;
        if (settled) continue;
        try out.append(a, .{ .token_id = id, .from = r.getBytes("from") orelse "", .at = r.getUint("at") orelse 0 });
    }
    std.mem.sort(Request, out.items, {}, struct {
        fn f(_: void, x: Request, y: Request) bool {
            return x.at > y.at;
        }
    }.f);
    return out.items;
}

/// The `/requests` answer's JSON: `[{tokenId, from, at}]`.
pub fn requestsJson(a: Allocator, rs: []const Request) ![]const u8 {
    var out: std.Io.Writer.Allocating = .init(a);
    var jw: std.json.Stringify = .{ .writer = &out.writer };
    try jw.beginArray();
    for (rs) |r| {
        try jw.beginObject();
        try jw.objectField("tokenId");
        try jw.write(r.token_id);
        try jw.objectField("from");
        try jw.write(try hexOf(a, r.from));
        try jw.objectField("at");
        try jw.write(r.at);
        try jw.endObject();
    }
    try jw.endArray();
    return out.written();
}

fn hexOf(a: Allocator, b: []const u8) ![]const u8 {
    const digits = "0123456789abcdef";
    const out = try a.alloc(u8, b.len * 2);
    for (b, 0..) |x, i| {
        out[2 * i] = digits[x >> 4];
        out[2 * i + 1] = digits[x & 0xf];
    }
    return out;
}
