//! The discovery toggle (0.18.0, shruggr/skein#137), pure: the 1Color discovery topic `tm_color`
//! (every token's deploy, BRC-207) and its lookup `ls_color` (0.20.0; `ls_color_deploys` gone) served by an
//! instance of their own, `discovery`, created from the token image with a config that names them
//! and nothing else (no beef, no terms, no market: a market-only instance of the deploys topic).
//! main.zig wires the VM.
//!
//! - **The record**, the root of the head `amm/discovery`:
//!
//!     {kind: "amm-discovery", on, status, since, url?, reason?}
//!
//!   `on` root's last ask; `status` `creating` (asked of the instance manager) → `enabled` (`url`
//!   the instance's) or `failed` (`reason`); `stopping` → `stopped`; `starting` → `enabled` (or
//!   back to `stopped` with its `reason`). No record: off (`{on: false}` read).
//! - **Root's message** `{fn: "discovery", args: {on: true|false}}` in the box `amm/admin`
//!   (`decide`): on with no instance (no record, or `failed`) → `create`; on with a stopped one →
//!   `start`; off with an enabled one → `stop`; else refused (already so, or in flight).
//!   Enabling a token does not submit its deploy here: the 1Color deploy page submits each new
//!   deploy to `tm_color` at the pages' origin (routed by topic, skein#137).
const std = @import("std");
const w = @import("chain");

const cbor = w.cbor;
const Value = cbor.Value;
const Allocator = std.mem.Allocator;
const eql = std.mem.eql;

pub const head = "amm/discovery";
pub const kind = "amm-discovery";
/// The instance's handle (fixed).
pub const handle = "discovery";
pub const topic = "tm_color";
/// Its lookup (0.20.0): color-lookup as `ls_color` over `tm_color`; a deploy by its outpoint.
pub const lookup = "ls_color";
/// The names the host routes to the instance at the bare host (shruggr/skein#137): the create's and
/// a start's `topics`.
pub const topics = [_][]const u8{ topic, lookup };

pub const Status = enum { creating, enabled, failed, stopping, stopped, starting };

fn map(a: Allocator, es: []const cbor.Entry) !Value {
    return .{ .map = try a.dupe(cbor.Entry, es) };
}

/// The instance manager's `create` body: `{handle: "discovery", image, config: {amm: {overlay:
/// {topics: {tm_color: "color-topic"}, lookups: {ls_color: {program: "color-lookup", topics:
/// ["tm_color"]}}}}}, topics: ["tm_color", "ls_color"]}` — no beef.
pub fn createBody(a: Allocator, image: []const u8) !Value {
    const lookups = try map(a, &.{
        .{ .key = lookup, .value = try map(a, &.{
            .{ .key = "program", .value = .{ .text = "color-lookup" } },
            .{ .key = "topics", .value = .{ .array = try a.dupe(Value, &.{.{ .text = topic }}) } },
        }) },
    });
    const overlay = try map(a, &.{
        .{ .key = "topics", .value = try map(a, &.{.{ .key = topic, .value = .{ .text = "color-topic" } }}) },
        .{ .key = "lookups", .value = lookups },
    });
    return map(a, &.{
        .{ .key = "handle", .value = .{ .text = handle } },
        .{ .key = "image", .value = .{ .text = image } },
        .{ .key = "config", .value = try map(a, &.{.{ .key = "amm", .value = try map(a, &.{.{ .key = "overlay", .value = overlay }}) }}) },
        .{ .key = "topics", .value = try topicsValue(a) },
    });
}

fn topicsValue(a: Allocator) !Value {
    var out: [topics.len]Value = undefined;
    for (topics, 0..) |t, i| out[i] = .{ .text = t };
    return .{ .array = try a.dupe(Value, &out) };
}

/// `start {handle, topics}`.
pub fn startBody(a: Allocator) !Value {
    return map(a, &.{ .{ .key = "handle", .value = .{ .text = handle } }, .{ .key = "topics", .value = try topicsValue(a) } });
}

/// `stop {handle}`.
pub fn handleBody(a: Allocator) !Value {
    return map(a, &.{.{ .key = "handle", .value = .{ .text = handle } }});
}

pub fn statusOf(rec: ?Value) ?Status {
    const r = rec orelse return null;
    if (!eql(u8, r.getText("kind") orelse "", kind)) return null;
    return std.meta.stringToEnum(Status, r.getText("status") orelse return null);
}

/// Root's `args` → `on`, or null (not `{on: true|false}`).
pub fn onArg(args: ?Value) ?bool {
    const x = args orelse return null;
    if (x != .map) return null;
    const v = x.get("on") orelse return null;
    return if (v == .boolean) v.boolean else null;
}

pub const Decision = union(enum) { create, start, stop, refused: []const u8 };

/// What root's `{on}` asks of the instance manager, given the record.
pub fn decide(a: Allocator, rec: ?Value, on: bool) !Decision {
    const st = statusOf(rec);
    if (on) return switch (st orelse return .create) {
        .failed => .create,
        .stopped => .start,
        .enabled => .{ .refused = "discovery is on already" },
        else => .{ .refused = try std.fmt.allocPrint(a, "discovery is {s}: wait for it", .{@tagName(st.?)}) },
    };
    return switch (st orelse return .{ .refused = "discovery is off already" }) {
        .enabled => .stop,
        .stopped, .failed => .{ .refused = "discovery is off already" },
        else => .{ .refused = try std.fmt.allocPrint(a, "discovery is {s}: wait for it", .{@tagName(st.?)}) },
    };
}

/// `rec` (or a new record) with `on`, `status`, `since` = `at` when given, the `set` fields (each
/// replacing its own) and `drop`'s keys removed.
pub fn with(a: Allocator, rec: ?Value, on: bool, status: Status, at: ?u64, set: []const cbor.Entry, drop: []const []const u8) !Value {
    var es: std.ArrayList(cbor.Entry) = .empty;
    try es.appendSlice(a, &.{
        .{ .key = "kind", .value = .{ .text = kind } },
        .{ .key = "on", .value = .{ .boolean = on } },
        .{ .key = "status", .value = .{ .text = @tagName(status) } },
    });
    if (at) |t| try es.append(a, .{ .key = "since", .value = .{ .uint = t } });
    if (rec) |r| if (r == .map) outer: for (r.map) |e| {
        for ([_][]const u8{ "kind", "on", "status" }) |k| if (eql(u8, k, e.key)) continue :outer;
        if (at != null and eql(u8, e.key, "since")) continue;
        for (set) |x| if (eql(u8, x.key, e.key)) continue :outer;
        for (drop) |k| if (eql(u8, k, e.key)) continue :outer;
        try es.append(a, e);
    };
    try es.appendSlice(a, set);
    return .{ .map = es.items };
}

/// The record once root's ask is sent: `creating` / `starting` (on) or `stopping` (off).
pub fn asked(a: Allocator, rec: ?Value, d: Decision, at: u64) !Value {
    return switch (d) {
        .create => with(a, rec, true, .creating, at, &.{}, &.{ "reason", "url" }),
        .start => with(a, rec, true, .starting, at, &.{}, &.{"reason"}),
        .stop => with(a, rec, false, .stopping, at, &.{}, &.{"reason"}),
        .refused => unreachable,
    };
}

/// The instance manager's answer applied to the record: a create's or start's `{url}` → `enabled`;
/// a stop's → `stopped`; `{error}` → `failed` (a create), back to `stopped` (a start) or `enabled`
/// (a stop), the error as `reason`.
pub fn answered(a: Allocator, rec: Value, ans: Value) !Value {
    const st = statusOf(rec) orelse return error.BadState;
    if (ans.getText("error")) |e| {
        const reason: []const cbor.Entry = &.{.{ .key = "reason", .value = .{ .text = e } }};
        return switch (st) {
            .creating => with(a, rec, true, .failed, null, reason, &.{}),
            .starting => with(a, rec, false, .stopped, null, reason, &.{}),
            .stopping => with(a, rec, true, .enabled, null, reason, &.{}),
            else => error.BadState,
        };
    }
    return switch (st) {
        .creating, .starting => blk: {
            var set: std.ArrayList(cbor.Entry) = .empty;
            if (ans.getText("url")) |u| try set.append(a, .{ .key = "url", .value = .{ .text = u } });
            break :blk with(a, rec, true, .enabled, null, set.items, &.{"reason"});
        },
        .stopping => with(a, rec, false, .stopped, null, &.{}, &.{"reason"}),
        else => error.BadState,
    };
}

/// The `/discovery` answer's JSON: the record's fields (no `kind`), or `{"on":false}`.
pub fn json(a: Allocator, rec: ?Value) ![]const u8 {
    if (statusOf(rec) == null) return a.dupe(u8, "{\"on\":false}");
    var out: std.Io.Writer.Allocating = .init(a);
    var jw: std.json.Stringify = .{ .writer = &out.writer };
    try jw.beginObject();
    for (rec.?.map) |e| {
        if (eql(u8, e.key, "kind")) continue;
        try jw.objectField(e.key);
        switch (e.value) {
            .text => |t| try jw.write(t),
            .boolean => |b| try jw.write(b),
            .uint => |n| try jw.write(n),
            else => try jw.write(null),
        }
    }
    try jw.endObject();
    return out.written();
}
