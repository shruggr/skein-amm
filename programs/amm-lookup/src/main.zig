//! amm-lookup: the AMM pool liquidity lookup service, served as `ls_amm_<assetId>` (skein-overlay's
//! lookup contract, the `lookup` module), over every 1Color token topic the
//! overlay serves. Its metadata and documentation answer the overlay's listing and
//! documentation routes (skein-overlay#2). See README.md.
const std = @import("std");
const idx = @import("index.zig");
const lookup = @import("lookup");
const vm = @import("overlay_sk");

pub const version = "0.1.0";

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
    \\The listed pools of the AMM's Pool contract, over every 1Color token topic this
    \\overlay serves (`tm_color_<txid>_<vout>`). A pool is a BRC-162 value output at output 0 whose
    \\lock is the compiled Pool code followed by its state, its prefix agreeing with the asset id and
    \\the TokenReserve its code and state carry. A pool is listed when it attests itself: its
    \\ValidatorPubKey is the ValidatorIdentity's child keyed by the Nonce in its state (derived by
    \\this lookup), and the Affirmation in its state is that key's signature of the pool's
    \\parameters. A Swap's continuation of a listed pool is listed when its ValidatorPubKey is the
    \\identity's child keyed by the spent pool outpoint. The LP's Close and the validator's Rescind
    \\(which clears the ValidatorPubKey) end the listing.
    \\
    \\Every query names its token, `tokenId` (`<txid>`, `<txid>_<vout>` or `<txid>.<vout>`,
    \\the deploy outpoint):
    \\
    \\- `{tokenId}`: every live pool of the token, as
    \\  `{lpPubKey, outpoint, bsvReserve, tokenReserve, liquidityFeeBps, validationFeeBps,
    \\  commissionBps, validatorIdentityKey}` (freeform);
    \\- `{tokenId, outpoint}`: that pool, or its newest continuation,
    \\  `{current, hops}` (freeform); with `beef: true`, the pool output and its
    \\  transaction's BEEF (output-list);
    \\- `{tokenId, validatorIdentityKey}`: that validator's listed pools of the token;
    \\- `{tokenId, lpPubKeys: [hex]}`: per LpPubKey `{lpPubKey, sats, tokens, swaps, current}`, the LP
    \\  fees its pool's Swaps paid (sats on sats-in Swaps, tokens on token-in), `current` its live pool
    \\  or null;
    \\- `{tokenId, validatorIdentityKeys: [hex]}`: per identity `{validatorIdentityKey, sats, tokens,
    \\  swaps, pools}`, the validator fees over its live pools' Swaps;
    \\- `{tokenId, commissionPkhs: [hex]}`: per key hash `{commissionPkh, sats, tokens, swaps}`, the
    \\  commissions paid to it.
    \\
    \\The fees are rows written as each Swap is admitted (removed on a rejection), from this lookup's
    \\0.33.0 on: Swaps admitted before have none. An unknown key answers zeros.
    \\
    \\No beat body of its own (0.48.0): the host and the pages read the validators' beats on
    \\`tm_color_<assetId>-live`.
    \\
    ;
}

pub fn main() u8 {
    stepTime() catch return 1; // a hook without its `at`: not this engine's call
    return lookup.main(idx.spec);
}

/// The time for the hooks (a trade row's and a history row's `at`): 0.48.2, the hook argument's `at`
/// — the time its transaction was admitted (skein-overlay 0.23.1), so a reindex re-applies the time
/// admission gave it; any other call, the in-VM call input's `now` (skein docs/VM.md, "call"). The
/// input is read again by `lookup.main`.
fn stepTime() !void {
    var arena_state = std.heap.ArenaAllocator.init(std.heap.wasm_allocator);
    defer arena_state.deinit();
    const a = arena_state.allocator();
    const in = try vm.input(a);
    const func = in.getText("fn") orelse "";
    const hook = std.mem.eql(u8, func, "admitted") or std.mem.eql(u8, func, "spent") or std.mem.eql(u8, func, "rejected");
    idx.now_ms = if (hook) lookup.atOf(try vm.callArg(a, in)) orelse return error.BadArgs else in.getUint("now") orelse 0;
}
