//! amm-lookup: the AMM pool liquidity lookup service, served as `ls_amm_<assetId>` (skein-overlay's
//! lookup contract, the `lookup` module), over every 1Color token topic the
//! overlay serves. Its metadata and documentation answer the overlay's listing and
//! documentation routes (skein-overlay#2). See README.md.
const std = @import("std");
const idx = @import("index.zig");
const lookup = @import("lookup");
const vm = @import("overlay_sk");
const chain = @import("chain");
const cbor = chain.cbor;

const ext = struct {
    /// The kernel's name-carrying in-VM call (shruggr/skein `call-permissions`, the contract's §1).
    extern "skein" fn callAs(prog: [*]const u8, prog_len: u32, func: [*]const u8, func_len: u32, name: [*]const u8, name_len: u32, arg: [*]const u8, arg_len: u32, out: [*]u8, cap: u32) i32;
};

/// `idx.filer` in the VM: the wallet app's `file` by the named call under the basket (the wallet's
/// program from its app record, `wallet/app` `programs.wallet`). A refusal fails the call, and so
/// the hook's step (F3).
fn fileByCall(a: std.mem.Allocator, basket: []const u8, arg: cbor.Value) anyerror!void {
    const rc = (try vm.head(a, "wallet/app")) orelse return error.NoWalletApp;
    const rec = try vm.store().getValue(a, rc);
    const prog = (rec.get("programs") orelse return error.NoWalletApp).getCid("wallet") orelse return error.NoWalletApp;
    const bytes = try cbor.encode(a, arg);
    const func = "file";
    _ = try vm.result(a, ext.callAs, .{ prog.ptr, @as(u32, @intCast(prog.len)), func.ptr, @as(u32, @intCast(func.len)), basket.ptr, @as(u32, @intCast(basket.len)), bytes.ptr, @as(u32, @intCast(bytes.len)) });
}

pub const version = "0.55.0";

pub fn metadata(_: std.mem.Allocator, _: []const u8) anyerror!lookup.Metadata {
    return .{
        .short_description = "AMM pools (the Pool contract over 1Color tokens): the listed pools of a token (a self-attesting pool and its Swap continuations, unspent), one pool or its newest continuation, a validator's pools; its beat, the prices.",
        .version = version,
        .information_url = "https://github.com/shruggr/skein-amm",
    };
}

pub fn documentation(_: std.mem.Allocator, _: []const u8) anyerror![]const u8 {
    return
    \\# AMM pools (ls_amm_<assetId>)
    \\
    \\Served per token as `ls_amm_<assetId>`, paired with the token topic `tm_color_<assetId>`.
    \\
    \\The listed pools of the AMM's Pool contract. A pool is listed when it attests itself (its
    \\ValidatorPubKey the ValidatorIdentity's child keyed by its Nonce, its Affirmation that key's
    \\signature of the pool's parameters); a Swap's continuation of a listed pool is listed when its
    \\ValidatorPubKey is the identity's child keyed by the spent pool outpoint. The LP's Close and the
    \\validator's Rescind end the listing.
    \\
    \\0.49.0: a recognizer. What it recognizes it emits as events (`amm/pool/<poolId>`,
    \\`amm/output/<txid>.<vout>`, `amm/hop`, `amm/commission/<txid>.<vout>`), read at
    \\`/events/…`; it stores only the token fee tally.
    \\
    \\Every query names its token, `tokenId` (`<txid>`, `<txid>_<vout>` or `<txid>.<vout>`):
    \\
    \\- `{tokenId}`: every live pool of the token, `{poolId, outpoint, bsvReserve, tokenReserve,
    \\  lpPubKey, liquidityFeeBps, validationFeeBps, commissionBps, validatorIdentityKey, at,
    \\  swaps}` (freeform);
    \\- `{tokenId, poolIds: [<txid>_<vout>]}`: per pool `{poolId, lpPubKey, status, outpoint?,
    \\  current?, sats, tokens, swaps}`, its state and the LP fees of its Swaps.
    \\
    ;
}

pub fn main() u8 {
    stepTime();
    return lookup.main(idx.spec);
}

/// `idx.open_chain` in the VM: the chain app's state as this step sees it now (the head
/// `chain/state`, read only; skein-sdk `open`: the record names its own network). No head: none.
fn chainByHead(a: std.mem.Allocator) anyerror!?lookup.Chain {
    const h = (try vm.head(a, lookup.chain_head)) orelse return null;
    return try chain.open(a, vm.store(), h, .main);
}

/// The step's context for the hooks: the in-VM call input's `now` (an unmined hop's `at`, a revert row's; skein docs/VM.md,
/// "call"; replay-exact), this instance's identity (`self.identity`: which fees are ours) and the
/// wallet's `file` by the named call. The events are the service's own (skein-overlay 0.27.0:
/// `svc.emit`, `svc.events`, `svc.app`). The input is read again by `lookup.main`.
fn stepTime() void {
    const a = std.heap.page_allocator;
    const in = vm.input(a) catch return;
    idx.now_ms = in.getUint("now") orelse 0;
    idx.filer = fileByCall;
    idx.open_chain = chainByHead;
    if (in.get("self")) |me| if (me.getBytes("identity")) |k| if (k.len == 33) {
        idx.self_identity = k[0..33].*;
    };
}
