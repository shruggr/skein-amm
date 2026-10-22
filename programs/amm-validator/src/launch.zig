//! Launching the instance's genesis wallet program (skein docs/WALLET.md "The program"): a thread
//! of its own on `{body: <the request>}`, the args a message to the wallet's box carries. The
//! validator's Close (0.11.0, close.zig) is two such threads: `createAction` with the pool as a
//! caller input, then `signAction` with its unlock (shruggr/skein#93). (0.9.0's claim filing,
//! an `internalize` into the basket `amm-claims`, is gone with the claim.)
const std = @import("std");
const w = @import("chain");

const cbor = w.cbor;
const Value = cbor.Value;

/// The genesis wallet program's record CID: the step input's `programs.wallet` (the genesis's
/// programs, skein docs/VM.md). Null when the instance has none.
pub fn walletProgram(in: Value) ?[]const u8 {
    const ps = in.get("programs") orelse return null;
    return ps.getCid("wallet");
}

/// The args record a wallet thread is launched on: `{body: <the body record>}` — what a message
/// to the wallet's box carries (the wallet reads `args.body`). Both records put in `s`. → its CID.
pub fn launchArgs(a: std.mem.Allocator, s: w.store.Store, body: Value) ![]const u8 {
    const bc = try s.putValue(a, body);
    return s.putValue(a, .{ .map = try a.dupe(cbor.Entry, &.{.{ .key = "body", .value = .{ .cid = bc } }}) });
}
