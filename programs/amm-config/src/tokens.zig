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

/// The lookups every token instance serves beside its own `ls_color_<id>` (the token manifest's
/// `config.overlay.lookups`, which a create's `lookups` replaces: skein's config merge is per
/// program, so it is named again). 0.20.0: `ls_color` and `ls_color_deploys` dropped (David,
/// 2026-10-29: one lookup per topic; `ls_color_<id>` answers the token's outputs by outpoint).
pub const manifest_lookups = [_][2][]const u8{.{ "ls_amm", "amm-lookup" }};

pub const Status = enum { creating, enabled, failed, stopped, starting };

// ---------------------------------------------------------------- token ids and deploys

pub const Deploy = struct {
    /// `<txid>_<vout>`, canonical (`_0` included).
    token_id: []const u8,
    topic: []const u8,
    lookup: []const u8,
    sym: []const u8 = "",
    dec: u8 = 0,
    icon: ?[]const u8 = null,
};

pub const DeployCheck = union(enum) { ok: Deploy, refused: []const u8 };

/// A token id in any form (`<txid>`, `<txid>_<vout>`, `<txid>.<vout>`) → its canonical text, or null.
pub fn canonicalId(a: Allocator, text: []const u8) !?[]const u8 {
    const id = color.name.tokenIdOfString(text) orelse return null;
    var buf: [color.name.max_suffix_len]u8 = undefined;
    return try a.dupe(u8, color.name.tokenIdText(&buf, id));
}

/// The deploy `beef` (its wire bytes) carries for token `token_id`: refused unless the BEEF parses,
/// its subject is the token id's transaction, and that transaction's output `<vout>` is a deploy.
pub fn deployOf(a: Allocator, token_id: []const u8, beef_bytes: []const u8) !DeployCheck {
    const nid = color.name.tokenIdOfString(token_id) orelse return .{ .refused = "tokenId: want <txid>_<vout>" };
    const b = w.beef.parse(a, beef_bytes) catch return .{ .refused = "beef: not a BEEF" };
    const subject = b.subject() orelse return .{ .refused = "beef: no transaction" };
    if (!eql(u8, &subject, &nid.txid)) return .{ .refused = "beef: its subject is not the token id's transaction (want the deploy's Atomic BEEF)" };
    const e = b.find(subject) orelse return .{ .refused = "beef: the subject is not in it" };
    const raw = e.raw orelse return .{ .refused = "beef: the subject's transaction is not in it" };
    const tx = w.bsvz.transaction.Transaction.parse(a, raw) catch return .{ .refused = "beef: the subject does not parse" };
    if (nid.vout >= tx.outputs.len) return .{ .refused = "tokenId: the deploy transaction has no such output" };
    const id: color.bsv21.TokenId = .{ .txid = nid.txid, .vout = nid.vout, .kind = if (nid.vout == 0) .native else .legacy };
    const t = (try color.bsv21.tokenOf(a, id, nid.txid, nid.vout, tx.outputs[nid.vout].locking_script.bytes)) orelse return .{ .refused = "not a deploy: the output carries no 1Color token" };
    if (t.role != .deploy) return .{ .refused = "not a deploy: the output is a token transfer, not its deploy" };
    var tb: [color.name.max_suffix_len]u8 = undefined;
    var nb: [color.name.max_topic_len]u8 = undefined;
    var lb: [color.name.max_lookup_len]u8 = undefined;
    var out: Deploy = .{
        .token_id = try a.dupe(u8, color.name.tokenIdText(&tb, nid)),
        .topic = try a.dupe(u8, color.name.topicName(&nb, nid)),
        .lookup = try a.dupe(u8, color.name.lookupName(&lb, nid)),
    };
    switch (t.form) {
        .binary => {
            const m = color.brc162.metadataOf(t.binary.?.payload orelse "");
            out.sym = m.sym orelse "";
            out.dec = m.dec orelse 0;
            if (m.icon) |ic| out.icon = try dataUrlOf(a, ic);
        },
        .json => {
            const j = t.json.?;
            out.sym = j.sym orelse "";
            out.dec = j.dec orelse 0;
            out.icon = j.icon;
        },
    }
    return .{ .ok = out };
}

/// An embedded icon as a data URL, as skein-1color's token list writes it (color_lookup.zig
/// `dataUrlOf`); null when the media type is not an RFC 6838 `type/subtype`.
pub fn dataUrlOf(a: Allocator, ic: color.brc162.Icon) !?[]const u8 {
    if (!isMediaType(ic.media_type)) return null;
    const enc = std.base64.standard.Encoder;
    const out = try a.alloc(u8, "data:".len + ic.media_type.len + ";base64,".len + enc.calcSize(ic.bytes.len));
    const head = try std.fmt.bufPrint(out, "data:{s};base64,", .{ic.media_type});
    _ = enc.encode(out[head.len..], ic.bytes);
    return out;
}

fn isMediaType(t: []const u8) bool {
    const slash = std.mem.indexOfScalar(u8, t, '/') orelse return false;
    return restrictedName(t[0..slash]) and restrictedName(t[slash + 1 ..]);
}

fn restrictedName(n: []const u8) bool {
    if (n.len == 0 or n.len > 127 or !std.ascii.isAlphanumeric(n[0])) return false;
    for (n[1..]) |ch| if (!std.ascii.isAlphanumeric(ch) and std.mem.indexOfScalar(u8, "!#$&-^_.+", ch) == null) return false;
    return true;
}

// ---------------------------------------------------------------- the create

fn map(a: Allocator, es: []const cbor.Entry) !Value {
    return .{ .map = try a.dupe(cbor.Entry, es) };
}

/// The token instance's config at birth: `{amm: {overlay: {topics: {<topic>: "color-topic"},
/// lookups: {ls_amm, <lookup>: {program: "color-lookup", topics: [<topic>]}}, terms?}, "amm-p2p":
/// {market?}}}` — `terms` and `market` the defaults' when set.
pub fn createConfig(a: Allocator, d: Deploy, defaults: ?Value) !Value {
    var lookups: std.ArrayList(cbor.Entry) = .empty;
    for (manifest_lookups) |l| try lookups.append(a, .{ .key = l[0], .value = try map(a, &.{.{ .key = "program", .value = .{ .text = l[1] } }}) });
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
/// `[tm_color_<id>, ls_color_<id>]`, the create's and a start's `topics`.
pub fn topicsOf(a: Allocator, topic: []const u8, lookup: []const u8) !Value {
    return .{ .array = try a.dupe(Value, &.{ .{ .text = topic }, .{ .text = lookup } }) };
}

/// The instance manager's `create` body: `{handle: <tokenId>, image, config, beef, topics:
/// [tm_color_<id>, ls_color_<id>]}`.
pub fn createBody(a: Allocator, d: Deploy, image: []const u8, defaults: ?Value, beef_bytes: []const u8) !Value {
    return map(a, &.{
        .{ .key = "handle", .value = .{ .text = d.token_id } },
        .{ .key = "image", .value = .{ .text = image } },
        .{ .key = "config", .value = try createConfig(a, d, defaults) },
        .{ .key = "beef", .value = .{ .bytes = beef_bytes } },
        .{ .key = "topics", .value = try topicsOf(a, d.topic, d.lookup) },
    });
}

/// `start {handle, topics: [tm_color_<id>, ls_color_<id>]}` (a stopped token enabled again); null
/// if `token_id` is not a token id.
pub fn startBody(a: Allocator, token_id: []const u8) !?Value {
    const nid = color.name.tokenIdOfString(token_id) orelse return null;
    var nb: [color.name.max_topic_len]u8 = undefined;
    var lb: [color.name.max_lookup_len]u8 = undefined;
    return try map(a, &.{
        .{ .key = "handle", .value = .{ .text = token_id } },
        .{ .key = "topics", .value = try topicsOf(a, try a.dupe(u8, color.name.topicName(&nb, nid)), try a.dupe(u8, color.name.lookupName(&lb, nid))) },
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
