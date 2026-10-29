//! A 1Color deploy read from a BEEF, pure (0.22.0: shared by amm-config's enable and amm-setup's
//! deploy event; moved here from programs/amm-config/src/tokens.zig, unchanged in behaviour).
//!
//! The BEEF names the deploy transaction as its subject, and that transaction's output `<vout>` is
//! a deploy (skein-1color's rules, `color.bsv21.tokenOf`: a BRC-162 binary deploy at output 0, a
//! BRC-161 inscription at any): its token id, topic, lookup names and display fields, as the 1Color
//! token list gives them (sym, dec, icon).
//!
//!   deployOf(tokenId, beef)   the token id names the output (amm-config's enable)
//!   deployIn(beef)            the first output of the subject that is a deploy (amm-setup: the
//!                             deploy event carries no token id; a BRC-162 deploy is output 0, a
//!                             legacy BRC-161 deploy the first inscription that deploys)
const std = @import("std");
const w = @import("chain");
const color = @import("1color");

const Allocator = std.mem.Allocator;
const eql = std.mem.eql;

pub const Deploy = struct {
    /// `<txid>_<vout>`, canonical (`_0` included).
    token_id: []const u8,
    topic: []const u8,
    lookup: []const u8,
    sym: []const u8 = "",
    dec: u8 = 0,
    icon: ?[]const u8 = null,
    /// The deploy transaction's id (display hex) and the deploy's output.
    txid: []const u8 = "",
    vout: u32 = 0,
};

pub const DeployCheck = union(enum) { ok: Deploy, refused: []const u8 };

/// The AMM lookup's service name for a token (0.21.0, David 2026-10-29): `ls_amm_<assetId>`, paired
/// with `tm_color_<assetId>` as `ls_color_<assetId>` is. `token_id` canonical (`<txid>_<vout>`).
pub fn ammLookupName(a: Allocator, token_id: []const u8) ![]u8 {
    return std.fmt.allocPrint(a, "ls_amm_{s}", .{token_id});
}

/// A token id in any form (`<txid>`, `<txid>_<vout>`, `<txid>.<vout>`) → its canonical text, or null.
pub fn canonicalId(a: Allocator, text: []const u8) !?[]const u8 {
    const id = color.name.tokenIdOfString(text) orelse return null;
    var buf: [color.name.max_suffix_len]u8 = undefined;
    return try a.dupe(u8, color.name.tokenIdText(&buf, id));
}

const Subject = struct { txid: [32]u8, tx: w.bsvz.transaction.Transaction };

fn subjectOf(a: Allocator, beef_bytes: []const u8) !union(enum) { ok: Subject, refused: []const u8 } {
    const b = w.beef.parse(a, beef_bytes) catch return .{ .refused = "beef: not a BEEF" };
    const subject = b.subject() orelse return .{ .refused = "beef: no transaction" };
    const e = b.find(subject) orelse return .{ .refused = "beef: the subject is not in it" };
    const raw = e.raw orelse return .{ .refused = "beef: the subject's transaction is not in it" };
    const tx = w.bsvz.transaction.Transaction.parse(a, raw) catch return .{ .refused = "beef: the subject does not parse" };
    return .{ .ok = .{ .txid = subject, .tx = tx } };
}

/// The token at output `vout` of `tx` (`txid`) when it is a deploy there: null when the output
/// carries no 1Color token; `.refused` when it is the token's transfer, not its deploy.
fn deployAt(a: Allocator, txid: [32]u8, tx: w.bsvz.transaction.Transaction, vout: u32) !?DeployCheck {
    const kind: color.name.Kind = if (vout == 0) .native else .legacy;
    const nid: color.name.TokenId = .{ .txid = txid, .vout = vout, .kind = kind };
    const id: color.bsv21.TokenId = .{ .txid = txid, .vout = vout, .kind = if (vout == 0) .native else .legacy };
    const t = (try color.bsv21.tokenOf(a, id, txid, vout, tx.outputs[vout].locking_script.bytes)) orelse return null;
    if (t.role != .deploy) return .{ .refused = "not a deploy: the output is a token transfer, not its deploy" };
    var tb: [color.name.max_suffix_len]u8 = undefined;
    var nb: [color.name.max_topic_len]u8 = undefined;
    var lb: [color.name.max_lookup_len]u8 = undefined;
    var out: Deploy = .{
        .token_id = try a.dupe(u8, color.name.tokenIdText(&tb, nid)),
        .topic = try a.dupe(u8, color.name.topicName(&nb, nid)),
        .lookup = try a.dupe(u8, color.name.lookupName(&lb, nid)),
        .txid = try a.dupe(u8, &w.header.toHex(txid)),
        .vout = vout,
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

/// The deploy `beef` (its wire bytes) carries for token `token_id`: refused unless the BEEF parses,
/// its subject is the token id's transaction, and that transaction's output `<vout>` is a deploy.
pub fn deployOf(a: Allocator, token_id: []const u8, beef_bytes: []const u8) !DeployCheck {
    const nid = color.name.tokenIdOfString(token_id) orelse return .{ .refused = "tokenId: want <txid>_<vout>" };
    const sub = switch (try subjectOf(a, beef_bytes)) {
        .refused => |why| return .{ .refused = why },
        .ok => |x| x,
    };
    if (!eql(u8, &sub.txid, &nid.txid)) return .{ .refused = "beef: its subject is not the token id's transaction (want the deploy's Atomic BEEF)" };
    if (nid.vout >= sub.tx.outputs.len) return .{ .refused = "tokenId: the deploy transaction has no such output" };
    return (try deployAt(a, sub.txid, sub.tx, nid.vout)) orelse .{ .refused = "not a deploy: the output carries no 1Color token" };
}

/// The deploy the BEEF's subject is (0.22.0, amm-setup): its first output that is a deploy — output
/// 0 for a BRC-162 binary deploy, the first deploying inscription for a legacy BRC-161 one. Refused
/// when the BEEF has no subject or none of the subject's outputs deploys a token.
pub fn deployIn(a: Allocator, beef_bytes: []const u8) !DeployCheck {
    const sub = switch (try subjectOf(a, beef_bytes)) {
        .refused => |why| return .{ .refused = why },
        .ok => |x| x,
    };
    for (0..sub.tx.outputs.len) |i| {
        const vout: u32 = @intCast(i);
        if (try deployAt(a, sub.txid, sub.tx, vout)) |d| if (d == .ok) return d;
    }
    return .{ .refused = "not a deploy: no output of the subject deploys a 1Color token" };
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
