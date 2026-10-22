//! amm-lookup: `ls_amm`, the AMM pool liquidity lookup service (skein-overlay's
//! lookup contract, the `lookup` module), over every 1Color token topic the
//! overlay serves. Its metadata and documentation answer the overlay's listing and
//! documentation routes (skein-overlay#2). See README.md.
const std = @import("std");
const idx = @import("index.zig");
const lookup = @import("lookup");

pub const version = "0.1.0";

pub fn metadata(_: std.mem.Allocator, _: []const u8) anyerror!lookup.Metadata {
    return .{
        .short_description = "AMM pools (the Pool contract over 1Color tokens): the listed pools of a token (an affirmed deploy's pool and its continuations, unspent), one pool or its newest continuation, a validator's pools; its beat, the prices.",
        .version = version,
        .information_url = "https://github.com/shruggr/skein-amm",
    };
}

pub fn documentation(_: std.mem.Allocator, _: []const u8) anyerror![]const u8 {
    return
    \\# AMM pools (ls_amm)
    \\
    \\The listed pools of the AMM's Pool contract, over every 1Color token topic this
    \\overlay serves (`tm_color_<txid>_<vout>`). A pool is a BRC-162 value output at output 0 whose
    \\lock is the compiled Pool code followed by its state, its prefix agreeing with the asset id and
    \\the TokenReserve its code and state carry. A deploy's pool is listed when its output carries
    \\the validator's affirmation as its BRC-162 payload and it verifies: the ValidatorPubKey in the
    \\pool's state signed the pool's parameters and the hash of the terms it agreed to. A swap's
    \\continuation of a listed pool is listed; a Close (the LP's or the validator's) ends it.
    \\
    \\Every query names its token, `tokenId` (`<txid>`, `<txid>_<vout>` or `<txid>.<vout>`,
    \\the deploy outpoint):
    \\
    \\- `{tokenId}`: every live pool of the token, as
    \\  `{outpoint, bsvReserve, tokenReserve, liquidityFeeBps, validationFeeBps,
    \\  commissionBps, validatorIdentityKey, lastSeen?}` (freeform);
    \\- `{tokenId, outpoint}`: that pool, or its newest continuation,
    \\  `{current, hops}` (freeform); with `beef: true`, the pool output and its
    \\  transaction's BEEF (output-list);
    \\- `{tokenId, validatorIdentityKey}`: that validator's listed pools of the token.
    \\
    \\Its beat on `ls_amm-live` (dag-cbor): `{tokens: {<assetId>: {<validator identity>: {sats, tokens,
    \\pools}}}}`, this overlay's listed pools summed per token and validator: the prices.
    \\
    ;
}

pub fn main() u8 {
    return lookup.main(idx.spec);
}
