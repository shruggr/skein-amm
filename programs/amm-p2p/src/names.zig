//! Topic, protocol, box and schedule names. One overlay topic per token,
//! `tm_<txid>` (the deploy txid, 64 lowercase hex characters in display
//! order, no suffix); skein's overlay engine runs `<topic>`, `<topic>-admit`
//! and `<topic>-proof` (skein#74). This program adds, in the same suffix
//! style:
//!
//!   tm_<txid>-live         GossipSub: validator heartbeats {identityKey, peerId, sig, at}
//!   /amm/proofs/1.0.0      a direct call (a libp2p stream): proofs by block hash (catch-up)
//!
//! The names are parsed here, not by amm-topic.
const std = @import("std");

pub const live_suffix = "-live";
pub const proofs_protocol = "/amm/proofs/1.0.0";

/// This program's own box: the accepted heartbeats (events), the cron
/// provider's ticks and the start/stop messages.
pub const own_box = "amm-p2p";

pub const Kind = enum { overlay, live };

/// A topic name of ours: the overlay topic it belongs to and what it is.
pub const Topic = struct { overlay: []const u8, id: [32]u8, kind: Kind };

/// The deploy txid (display order) of an overlay topic `tm_<txid>`, or null.
pub fn txidOf(name: []const u8) ?[32]u8 {
    if (name.len != 3 + 64 or !std.mem.startsWith(u8, name, "tm_")) return null;
    const hex = name[3..];
    for (hex) |c| if (!((c >= '0' and c <= '9') or (c >= 'a' and c <= 'f'))) return null;
    var id: [32]u8 = undefined;
    _ = std.fmt.hexToBytes(&id, hex) catch return null;
    return id;
}

pub fn parse(name: []const u8) ?Topic {
    if (std.mem.endsWith(u8, name, live_suffix)) {
        const o = name[0 .. name.len - live_suffix.len];
        if (txidOf(o)) |id| return .{ .overlay = o, .id = id, .kind = .live };
        return null;
    }
    const id = txidOf(name) orelse return null;
    return .{ .overlay = name, .id = id, .kind = .overlay };
}

pub fn live(a: std.mem.Allocator, overlay: []const u8) ![]u8 {
    return std.mem.concat(a, u8, &.{ overlay, live_suffix });
}
