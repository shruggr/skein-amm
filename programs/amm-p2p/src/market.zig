//! The market's terms as a runtime record (0.16.0, David Case 2026-10-24: the market half of the
//! validator terms, src/terms.zig). One record for the whole skein for now; per-token overrides
//! keyed by token id are the release shape, not built.
//!
//! Root sets it with the message `{fn: "market", args: {minCommissionBps}}` in the box
//! `amm/amm-p2p` (this program's own box) and clears it with `{fn: "market", args: null}` (the role
//! `root` gates `market`). The record is the root of the head `<app>/market`:
//!
//!   {minCommissionBps: uint}     set (an integer, 0 to 10 000)
//!   null                         cleared (no head delete)
//!
//! Unset (no head, or the record `null`): a minimum of 0. The minimum is a listing preference of
//! this market, not a rule (David 2026-10-24: "Are you putting in work to limit things you
//! shouldn't be?"): `amm.swap.terms` answers it and the Swap page lists only the pools at or above
//! it; the relay refuses nothing by it (anyone can swap against any pool through their own skein
//! or straight to the validator).
//!
//! The initial value (0.17.0, shruggr/skein-amm#4): a token instance is created with its admin's
//! default minimum in its config, `config["amm-p2p"].market: {minCommissionBps}` (the app record's:
//! given at the create). The head, once root writes it (its `null` too), wins (`current`).
const std = @import("std");
const c = @import("chain");

const cbor = c.cbor;
const Value = cbor.Value;
const Allocator = std.mem.Allocator;

/// The head the record is under: `<app>/market`.
pub fn headName(a: Allocator, app: []const u8) ![]u8 {
    return std.fmt.allocPrint(a, "{s}/market", .{app});
}

/// The function root's message names (gated by `roles.root`).
pub const fn_market = "market";

/// Basis points: 0 to 10 000.
pub const max_bps: u64 = 10_000;

pub const Market = struct {
    min_commission_bps: u64 = 0,
};

pub const Parsed = union(enum) {
    set: Market,
    clear,
    /// Not the shape: why.
    bad: []const u8,
};

const want = "want args {minCommissionBps} (an integer, 0 to 10000), or null to clear";

/// Root's `args`: `null` (or absent) clears; `{minCommissionBps}`, an integer 0 to 10 000, sets
/// (other keys refused); anything else is refused.
pub fn parseArgs(args: ?Value) Parsed {
    const v = args orelse return .clear;
    if (v == .null) return .clear;
    if (v != .map) return .{ .bad = want };
    for (v.map) |e| if (!std.mem.eql(u8, e.key, "minCommissionBps")) return .{ .bad = "want args {minCommissionBps} only" };
    const x = v.get("minCommissionBps") orelse return .{ .bad = want };
    if (x != .uint or x.uint > max_bps) return .{ .bad = "minCommissionBps is an integer, 0 to 10000 basis points" };
    return .{ .set = .{ .min_commission_bps = x.uint } };
}

/// The record for `m`: `{minCommissionBps}`.
pub fn record(a: Allocator, m: Market) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "minCommissionBps", .value = .{ .uint = m.min_commission_bps } },
    }) };
}

/// The market a record holds: the record `null` (cleared) or not the shape, the default (0).
pub fn fromRecord(v: ?Value) Market {
    return switch (parseArgs(v orelse .null)) {
        .set => |m| m,
        .clear, .bad => .{},
    };
}

/// The program's section of the config the minimum's initial value is under: `config["amm-p2p"]`.
pub const config_section = "amm-p2p";

/// The market now (0.17.0): the head `<app>/market`'s record when there is one (`head`, the record
/// `null` included: cleared, the default), else the app record's `config["amm-p2p"].market`, else
/// the default (0).
pub fn current(head: ?Value, app: ?Value) Market {
    if (head) |h| return fromRecord(h);
    const cf = (app orelse return .{}).get("config") orelse return .{};
    const sec = cf.get(config_section) orelse return .{};
    return fromRecord(sec.get("market"));
}

test "market: the head wins, its null too; else config[\"amm-p2p\"].market; else 0" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const app: Value = .{ .map = try a.dupe(cbor.Entry, &.{.{ .key = "config", .value = .{ .map = try a.dupe(cbor.Entry, &.{.{ .key = config_section, .value = .{ .map = try a.dupe(cbor.Entry, &.{.{ .key = "market", .value = try record(a, .{ .min_commission_bps = 7 }) }}) } }}) } }}) };
    try std.testing.expectEqual(@as(u64, 7), current(null, app).min_commission_bps);
    try std.testing.expectEqual(@as(u64, 0), current(.null, app).min_commission_bps);
    try std.testing.expectEqual(@as(u64, 3), current(try record(a, .{ .min_commission_bps = 3 }), app).min_commission_bps);
    try std.testing.expectEqual(@as(u64, 0), current(null, null).min_commission_bps);
}
