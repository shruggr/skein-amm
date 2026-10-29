//! The config instance's state and decisions (0.17.0, shruggr/skein-amm#4), pure: over values and a
//! store (main.zig wires the VM).
//!
//! - **The enable list**, the root of the head `amm/tokens`:
//!
//!     {kind: "amm-tokens", tokens: [<row>, …]}
//!     row {tokenId, topic, sym, dec, icon?, status, since, url?, reason?, delivered?}
//!
//!   `status` `creating` (asked of the instance manager) → `enabled` (its answer, `url` the token
//!   instance's; `delivered` the create's BEEF delivery as the host answered it: `{verdict,
//!   reason?}`) or `failed` (`reason`, the manager's error); `stopped` after a disable; `starting`
//!   while a stopped token is started again. `since` the enable's time (ms).
//! - **The defaults**, the root of the head `amm/defaults`: `{kind: "amm-defaults", terms?:
//!   {lpFeeBps, validatorFeeBps, commissionBps}, market?: {minCommissionBps}}` — a token instance's
//!   INITIAL values, put in its create's config (`createConfig`); its own records win once root
//!   writes them there.
//! - **A deploy** (`deployOf`): the enable's BEEF names the token id's transaction as its subject,
//!   and that transaction's output `<vout>` is a deploy (skein-1color's rules: a BRC-162 binary
//!   deploy at output 0, a BRC-161 inscription at any): its display fields, as the 1Color token
//!   list gives them (sym, dec, icon).
const std = @import("std");
const w = @import("chain");
const color = @import("1color");
const deploy = @import("deploy");

const cbor = w.cbor;
const Value = cbor.Value;
const Allocator = std.mem.Allocator;
const eql = std.mem.eql;

pub const tokens_head = "amm/tokens";
pub const tokens_kind = "amm-tokens";
pub const defaults_head = "amm/defaults";
pub const defaults_kind = "amm-defaults";
/// Root's box: `amm/admin` (the manifest's mailbox route `admin`).
pub const admin_box = "amm/admin";
/// The image a token instance is created from when the config names none: the host's
/// `$SKEIN_IMAGES_DIR/amm-token` (the repository's image/token).
pub const default_token_image = "amm-token";

/// The AMM lookup's service name for a token, `ls_amm_<assetId>` (src/deploy.zig, 0.22.0: shared
/// with amm-setup).
pub const ammLookupName = deploy.ammLookupName;

pub const Status = enum { creating, enabled, failed, stopped, starting };

// ---------------------------------------------------------------- token ids and deploys

// The deploy read from a BEEF: src/deploy.zig (0.22.0: moved there, shared with amm-setup's deploy
// event; unchanged).
pub const Deploy = deploy.Deploy;
pub const DeployCheck = deploy.DeployCheck;
pub const canonicalId = deploy.canonicalId;
pub const deployOf = deploy.deployOf;
pub const dataUrlOf = deploy.dataUrlOf;

// ---------------------------------------------------------------- the create

fn map(a: Allocator, es: []const cbor.Entry) !Value {
    return .{ .map = try a.dupe(cbor.Entry, es) };
}

/// The token instance's config at birth: `{amm: {overlay: {topics: {<topic>: "color-topic"},
/// lookups: {ls_amm_<id>: {program: "amm-lookup"}, <lookup>: {program: "color-lookup", topics:
/// [<topic>]}}, terms?}, "amm-p2p": {market?}}}` — `terms` and `market` the defaults' when set. The
/// create's `lookups` replaces the token manifest's (skein's config merge is per program).
pub fn createConfig(a: Allocator, d: Deploy, defaults: ?Value) !Value {
    var lookups: std.ArrayList(cbor.Entry) = .empty;
    try lookups.append(a, .{ .key = try ammLookupName(a, d.token_id), .value = try map(a, &.{.{ .key = "program", .value = .{ .text = "amm-lookup" } }}) });
    try lookups.append(a, .{ .key = d.lookup, .value = try map(a, &.{
        .{ .key = "program", .value = .{ .text = "color-lookup" } },
        .{ .key = "topics", .value = .{ .array = try a.dupe(Value, &.{.{ .text = d.topic }}) } },
    }) });
    var overlay: std.ArrayList(cbor.Entry) = .empty;
    try overlay.appendSlice(a, &.{
        .{ .key = "topics", .value = try map(a, &.{.{ .key = d.topic, .value = .{ .text = "color-topic" } }}) },
        .{ .key = "lookups", .value = .{ .map = lookups.items } },
    });
    var app: std.ArrayList(cbor.Entry) = .empty;
    if (defaults) |df| {
        if (df.get("terms")) |t| if (t == .map) try overlay.append(a, .{ .key = "terms", .value = t });
    }
    try app.append(a, .{ .key = "overlay", .value = .{ .map = overlay.items } });
    if (defaults) |df| {
        if (df.get("market")) |m| if (m == .map) try app.append(a, .{ .key = "amm-p2p", .value = try map(a, &.{.{ .key = "market", .value = m }}) });
    }
    return map(a, &.{.{ .key = "amm", .value = .{ .map = app.items } }});
}

/// The names the host routes to a token's instance at the bare host (shruggr/skein#137):
/// `[tm_color_<id>, ls_color_<id>, ls_amm_<id>]`, the create's and a start's `topics`.
pub fn topicsOf(a: Allocator, token_id: []const u8, topic: []const u8, lookup: []const u8) !Value {
    return .{ .array = try a.dupe(Value, &.{ .{ .text = topic }, .{ .text = lookup }, .{ .text = try ammLookupName(a, token_id) } }) };
}

/// The instance manager's `create` body: `{handle: <tokenId>, image, config, beef, topics:
/// [tm_color_<id>, ls_color_<id>, ls_amm_<id>]}`.
pub fn createBody(a: Allocator, d: Deploy, image: []const u8, defaults: ?Value, beef_bytes: []const u8) !Value {
    return map(a, &.{
        .{ .key = "handle", .value = .{ .text = d.token_id } },
        .{ .key = "image", .value = .{ .text = image } },
        .{ .key = "config", .value = try createConfig(a, d, defaults) },
        .{ .key = "beef", .value = .{ .bytes = beef_bytes } },
        .{ .key = "topics", .value = try topicsOf(a, d.token_id, d.topic, d.lookup) },
    });
}

/// `start {handle, topics: [tm_color_<id>, ls_color_<id>, ls_amm_<id>]}` (a stopped token enabled again); null
/// if `token_id` is not a token id.
pub fn startBody(a: Allocator, token_id: []const u8) !?Value {
    const nid = color.name.tokenIdOfString(token_id) orelse return null;
    var nb: [color.name.max_topic_len]u8 = undefined;
    var lb: [color.name.max_lookup_len]u8 = undefined;
    var tb: [color.name.max_suffix_len]u8 = undefined;
    return try map(a, &.{
        .{ .key = "handle", .value = .{ .text = token_id } },
        .{ .key = "topics", .value = try topicsOf(a, color.name.tokenIdText(&tb, nid), try a.dupe(u8, color.name.topicName(&nb, nid)), try a.dupe(u8, color.name.lookupName(&lb, nid))) },
    });
}

/// `stop {handle}`.
pub fn handleBody(a: Allocator, token_id: []const u8) !Value {
    return map(a, &.{.{ .key = "handle", .value = .{ .text = token_id } }});
}

// ---------------------------------------------------------------- the enable list

/// The rows of the list record (none: no record yet).
pub fn rowsOf(rec: ?Value) []const Value {
    const r = rec orelse return &.{};
    if (!eql(u8, r.getText("kind") orelse "", tokens_kind)) return &.{};
    return r.getArray("tokens") orelse &.{};
}

/// The row for `token_id`, or null.
pub fn find(rows: []const Value, token_id: []const u8) ?Value {
    for (rows) |r| if (eql(u8, r.getText("tokenId") orelse "", token_id)) return r;
    return null;
}

/// The list record with `row` in place of the token's row (or added).
pub fn withRow(a: Allocator, rows: []const Value, row: Value) !Value {
    const id = row.getText("tokenId").?;
    var out: std.ArrayList(Value) = .empty;
    var replaced = false;
    for (rows) |r| {
        if (eql(u8, r.getText("tokenId") orelse "", id)) {
            try out.append(a, row);
            replaced = true;
        } else try out.append(a, r);
    }
    if (!replaced) try out.append(a, row);
    return map(a, &.{
        .{ .key = "kind", .value = .{ .text = tokens_kind } },
        .{ .key = "tokens", .value = .{ .array = out.items } },
    });
}

/// A new row: the deploy's fields, `creating`, since `at`.
pub fn creatingRow(a: Allocator, d: Deploy, at: u64) !Value {
    var es: std.ArrayList(cbor.Entry) = .empty;
    try es.appendSlice(a, &.{
        .{ .key = "tokenId", .value = .{ .text = d.token_id } },
        .{ .key = "topic", .value = .{ .text = d.topic } },
        .{ .key = "sym", .value = .{ .text = d.sym } },
        .{ .key = "dec", .value = .{ .uint = d.dec } },
    });
    if (d.icon) |ic| try es.append(a, .{ .key = "icon", .value = .{ .text = ic } });
    try es.appendSlice(a, &.{
        .{ .key = "status", .value = .{ .text = @tagName(Status.creating) } },
        .{ .key = "since", .value = .{ .uint = at } },
    });
    return .{ .map = es.items };
}

/// `row` with `status` and the given fields set (each replacing its own), and `drop`'s keys removed.
pub fn updated(a: Allocator, row: Value, status: Status, set: []const cbor.Entry, drop: []const []const u8) !Value {
    var es: std.ArrayList(cbor.Entry) = .empty;
    outer: for (row.map) |e| {
        if (eql(u8, e.key, "status")) continue;
        for (set) |x| if (eql(u8, x.key, e.key)) continue :outer;
        for (drop) |k| if (eql(u8, k, e.key)) continue :outer;
        try es.append(a, e);
    }
    try es.append(a, .{ .key = "status", .value = .{ .text = @tagName(status) } });
    try es.appendSlice(a, set);
    return .{ .map = es.items };
}

pub fn statusOf(row: Value) ?Status {
    return std.meta.stringToEnum(Status, row.getText("status") orelse return null);
}

/// The instance manager's answer to a create or start, applied to `row`: `{error}` → `failed`
/// (a create; a start's failure leaves it `stopped`, the reason kept); else `enabled` with `url`
/// (and, a create's, `delivered`).
pub fn answered(a: Allocator, row: Value, ans: Value, starting: bool) !Value {
    if (ans.getText("error")) |e| return updated(a, row, if (starting) .stopped else .failed, &.{.{ .key = "reason", .value = .{ .text = e } }}, &.{});
    var set: std.ArrayList(cbor.Entry) = .empty;
    if (ans.getText("url")) |u| try set.append(a, .{ .key = "url", .value = .{ .text = u } });
    if (ans.get("delivered")) |d| if (d == .map) try set.append(a, .{ .key = "delivered", .value = d });
    return updated(a, row, .enabled, set.items, &.{"reason"});
}

/// The `/tokens` answer's JSON: the rows newest first (by `since`, then token id).
pub fn tokensJson(a: Allocator, rows: []const Value) ![]const u8 {
    const sorted = try a.dupe(Value, rows);
    std.mem.sort(Value, sorted, {}, struct {
        fn f(_: void, x: Value, y: Value) bool {
            const sx = x.getUint("since") orelse 0;
            const sy = y.getUint("since") orelse 0;
            if (sx != sy) return sx > sy;
            return std.mem.order(u8, x.getText("tokenId") orelse "", y.getText("tokenId") orelse "") == .lt;
        }
    }.f);
    var out: std.Io.Writer.Allocating = .init(a);
    var jw: std.json.Stringify = .{ .writer = &out.writer };
    try jw.beginArray();
    for (sorted) |r| try writeJson(&jw, r);
    try jw.endArray();
    return out.written();
}

/// A value as JSON (maps, lists, text, numbers, booleans; bytes as hex).
pub fn writeJson(jw: *std.json.Stringify, v: Value) !void {
    switch (v) {
        .map => |es| {
            try jw.beginObject();
            for (es) |e| {
                try jw.objectField(e.key);
                try writeJson(jw, e.value);
            }
            try jw.endObject();
        },
        .array => |xs| {
            try jw.beginArray();
            for (xs) |x| try writeJson(jw, x);
            try jw.endArray();
        },
        .text => |t| try jw.write(t),
        .boolean => |b| try jw.write(b),
        .uint => |n| try jw.write(n),
        .nint => |n| try jw.write(-1 - @as(i128, n)),
        .null => try jw.write(null),
        .bytes => |b| {
            var out: std.ArrayList(u8) = .empty;
            defer out.deinit(std.heap.page_allocator);
            for (b) |x| out.appendSlice(std.heap.page_allocator, &std.fmt.bytesToHex([1]u8{x}, .lower)) catch return error.WriteFailed;
            try jw.write(out.items);
        },
        else => try jw.write(null),
    }
}

// ---------------------------------------------------------------- the defaults

pub const DefaultsChange = union(enum) { ok: Value, bad: []const u8 };

const terms_fields = [_][]const u8{ "lpFeeBps", "validatorFeeBps", "commissionBps" };

fn bps(v: Value) bool {
    return v == .uint and v.uint <= 10_000;
}

fn checkTerms(t: Value) ?[]const u8 {
    if (t != .map) return "terms: want {lpFeeBps, validatorFeeBps, commissionBps} (integers, 0 to 10000), or null";
    for (t.map) |e| {
        const known = for (terms_fields) |f| {
            if (eql(u8, f, e.key)) break true;
        } else false;
        if (!known) return "terms: want {lpFeeBps, validatorFeeBps, commissionBps} only";
    }
    for (terms_fields) |f| if (!bps(t.get(f) orelse return "terms: want {lpFeeBps, validatorFeeBps, commissionBps} (integers, 0 to 10000), or null")) return "terms: each fee is an integer, 0 to 10000 basis points";
    return null;
}

fn checkMarket(m: Value) ?[]const u8 {
    if (m != .map) return "market: want {minCommissionBps} (an integer, 0 to 10000), or null";
    for (m.map) |e| if (!eql(u8, e.key, "minCommissionBps")) return "market: want {minCommissionBps} only";
    if (!bps(m.get("minCommissionBps") orelse return "market: want {minCommissionBps}")) return "market: minCommissionBps is an integer, 0 to 10000 basis points";
    return null;
}

/// Root's `defaults {terms?, market?}` over the record `prev`: each named key set (a map, checked
/// as root's `terms` / `market` messages check theirs) or cleared (`null`); a key not named kept.
pub fn withDefaults(a: Allocator, prev: ?Value, args: ?Value) !DefaultsChange {
    const x = args orelse return .{ .bad = "want args {terms?, market?}" };
    if (x != .map) return .{ .bad = "want args {terms?, market?}" };
    for (x.map) |e| if (!eql(u8, e.key, "terms") and !eql(u8, e.key, "market")) return .{ .bad = "want args {terms?, market?} only" };
    if (x.get("terms")) |t| if (t != .null) if (checkTerms(t)) |why| return .{ .bad = why };
    if (x.get("market")) |m| if (m != .null) if (checkMarket(m)) |why| return .{ .bad = why };
    var es: std.ArrayList(cbor.Entry) = .empty;
    try es.append(a, .{ .key = "kind", .value = .{ .text = defaults_kind } });
    for ([_][]const u8{ "terms", "market" }) |k| {
        const v: ?Value = if (x.get(k)) |n| (if (n == .null) null else n) else if (prev) |p| p.get(k) else null;
        if (v) |vv| try es.append(a, .{ .key = k, .value = vv });
    }
    return .{ .ok = .{ .map = es.items } };
}

/// The `/defaults` answer's JSON: `{terms?, market?}` (`{}` when none is set).
pub fn defaultsJson(a: Allocator, rec: ?Value) ![]const u8 {
    var out: std.Io.Writer.Allocating = .init(a);
    var jw: std.json.Stringify = .{ .writer = &out.writer };
    try jw.beginObject();
    if (rec) |r| for ([_][]const u8{ "terms", "market" }) |k| if (r.get(k)) |v| {
        try jw.objectField(k);
        try writeJson(&jw, v);
    };
    try jw.endObject();
    return out.written();
}

// ---------------------------------------------------------------- answers

/// Root's answer: `{fn, request, replyTo, result}`.
pub fn answer(a: Allocator, func: []const u8, message: ?[]const u8, result: Value) !Value {
    return map(a, &.{
        .{ .key = "fn", .value = .{ .text = func } },
        .{ .key = "request", .value = if (message) |m| .{ .cid = m } else .null },
        .{ .key = "replyTo", .value = if (message) |m| .{ .cid = m } else .null },
        .{ .key = "result", .value = result },
    });
}

/// Root's refusal: `{fn, request, replyTo, error: {code, message}}`.
pub fn refusal(a: Allocator, func: []const u8, message: ?[]const u8, code: []const u8, text: []const u8) !Value {
    return map(a, &.{
        .{ .key = "fn", .value = .{ .text = func } },
        .{ .key = "request", .value = if (message) |m| .{ .cid = m } else .null },
        .{ .key = "replyTo", .value = if (message) |m| .{ .cid = m } else .null },
        .{ .key = "error", .value = try map(a, &.{
            .{ .key = "code", .value = .{ .text = code } },
            .{ .key = "message", .value = .{ .text = text } },
        }) },
    });
}

/// A message body that answers a request (`request` or `replyTo` in it): this program's own answer
/// to root, looped back when root's key is the instance's own (a one-key host) — not a request.
pub fn isAnswer(body: Value) bool {
    return body.get("request") != null or body.get("replyTo") != null;
}
