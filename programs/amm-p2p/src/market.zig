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
