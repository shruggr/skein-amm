//! amm-lookup: the AMM pool liquidity lookup service, served as `ls_amm_<assetId>` (skein-overlay's
//! lookup contract, the `lookup` module), over every 1Color token topic the
//! overlay serves. Its metadata and documentation answer the overlay's listing and
//! documentation routes (skein-overlay#2). See README.md.
const std = @import("std");
const idx = @import("index.zig");
const lookup = @import("lookup");
const vm = @import("overlay_sk");
const cbor = @import("chain").cbor;

/// The kernel's event index read (skein 0.5.0 `events`), which skein-overlay's `sk` module does not declare.
const ext = struct {
    extern "skein" fn events(q: [*]const u8, len: u32, out: [*]u8, cap: u32) i32;
};

/// `idx.reader` in the VM: the query as dag-cbor in, the answer decoded.
pub fn readEvents(a: std.mem.Allocator, q: cbor.Value) anyerror!cbor.Value {
    const bytes = try cbor.encode(a, q);
    return cbor.decode(a, try vm.result(a, ext.events, .{ bytes.ptr, @as(u32, @intCast(bytes.len)) }));
}

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
    \\The listed pools of the AMM's Pool contract. A pool is listed when it attests itself (its
    \\ValidatorPubKey the ValidatorIdentity's child keyed by its Nonce, its Affirmation that key's
    \\signature of the pool's parameters); a Swap's continuation of a listed pool is listed when its
    \\ValidatorPubKey is the identity's child keyed by the spent pool outpoint. The LP's Close and the
    \\validator's Rescind end the listing.
    \\
    \\0.49.0: a recognizer. What it recognizes it emits as events (`amm/pool/<poolId>`,
    \\`amm/output/<txid>.<vout>`, `amm/hop`, `amm/commission/<txid>.<vout>`), read at
    \\`/events/…`; it stores only its counters and the token fee tally.
    \\
    \\Every query names its token, `tokenId` (`<txid>`, `<txid>_<vout>` or `<txid>.<vout>`):
    \\
    \\- `{tokenId}`: every live pool of the token, `{poolId, outpoint, bsvReserve, tokenReserve,
    \\  lpPubKey, liquidityFeeBps, validationFeeBps, commissionBps, validatorIdentityKey, at, swaps,
    \\  lifetime}` (freeform);
    \\- `{tokenId, poolIds: [<txid>_<vout>]}`: per pool `{poolId, lpPubKey, status, outpoint?,
    \\  current?, sats, tokens, swaps}`, its state and the LP fees of its Swaps.
    \\
    ;
}

pub fn main() u8 {
    stepTime();
    return lookup.main(idx.spec);
}

/// The step's context for the hooks: the in-VM call input's `now` (a row's `at`; skein docs/VM.md,
/// "call"; replay-exact), the app the engine runs in (the hook's `app`: every event name's first
/// segment), this instance's identity (`self.identity`: which fees are ours), and the event index
/// read (skein-sdk `sk.events`). The input is read again by `lookup.main`; what is kept here lives
/// in the page allocator for the call.
fn stepTime() void {
    const a = std.heap.page_allocator;
    const in = vm.input(a) catch return;
    idx.now_ms = in.getUint("now") orelse 0;
    idx.reader = readEvents;
    if (in.get("self")) |me| if (me.getBytes("identity")) |k| if (k.len == 33) {
        idx.self_identity = k[0..33].*;
    };
    const arg = vm.callArg(a, in) catch return;
    idx.app = lookup.appOf(arg);
}
