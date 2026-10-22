//! The Pool contract (pool/Pool.runar.go) as the overlay sees it:
//! recognising a pool, parsing its readonly args and state, and the pool
//! checks. A shared library of this app's programs (shruggr/skein#120: it
//! moved here from amm-poc's amm-topic, whose token rules became
//! skein-1color's topic manager). The 1Color topic does NOT run these checks
//! (decided 2026-10-01: the topic is the token's topic, and a malformed pool
//! is still a valid token output, admitted when the BSV-21 rules allow it).
//! amm-lookup runs `check` in its `admitted` hook and indexes only the
//! pools that pass; amm-validator runs it before signing; amm-p2p's relay
//! checks a pair with it. The parser and the rules (`brc162`, `bsv21`) are
//! skein-1color's `1color` module, re-exported below.
//!
//! A pool is a BRC-162 value output whose lock (the script after the prefix, and after the
//! payload when a deploy carries its affirmation, 0.11.0)
//! is the compiled Pool code followed by `OP_RETURN` and the state:
//!
//!   code (fixtures/pool_artifact.zig, readonly args spliced in at the slots:
//!         assetId as `0x20 <32 bytes>`, lpFeeBps, validatorFeeBps and
//!         commissionBps as script numbers) || OP_RETURN ||
//!   TokenReserve (8 bytes, LE sign-magnitude) || LpPubKey || ValidatorPubKey
//!   || ValidatorIdentity (33 bytes each)
//!
//! Checks (a transaction failing one has no pool indexed):
//! - a pool is at output 0, so there is at most one per transaction;
//! - its prefix id and amount equal the asset id and TokenReserve the code
//!   and state carry (the contract writes the prefix from those copies and
//!   never reads it; it hard-codes the 0x20 push, so a pool exists only
//!   for a token whose binary id is 32 bytes: native, or legacy deployed
//!   at output 0); JSON outputs are never pools.
//!
//! Not checked (decided 2026-10-01): the ValidatorPubKey in state. A pool
//! with any validator key is a pool to the overlay. A wrong key only means
//! the named validator cannot sign for that pool (amm-validator refuses);
//! it proves nothing about consent or token validity. How
//! a validator derives its signing key is its own convention: amm-validator
//! derives and checks it itself with `validatorKey`, `keyId`,
//! `validator_protocol` and `methodOf`, which stay here as library
//! functions.
const std = @import("std");
const c = @import("chain");
const color = @import("1color");
const artifact = @import("fixtures/pool_artifact.zig");

/// The decoder and the rules this file is written over (skein-1color's `1color` module).
pub const brc162 = color.brc162;
pub const bsv21 = color.bsv21;

const bsvz = c.bsvz;

/// Decided 2026-09-29, amm-validator's convention (not an overlay rule):
/// the BRC-43 protocol of the validator's rotating signing key, and its key ID, the outpoint as
/// `<txid, 64 hex, display order>_<vout, decimal>` (BRC-162's id string
/// form). The invoice number is therefore
/// `1-amm pool-<txid>_<vout>`. gen/main.go must match.
pub const validator_protocol = bsvz.primitives.key_deriver.Protocol{ .security_level = 1, .name = "amm pool" };

pub const state_len = 8 + 33 * 3;
const OP_RETURN = 0x6a;

pub const Pool = struct {
    asset_id: [32]u8,
    lp_fee_bps: i64,
    validator_fee_bps: i64,
    /// The relay's commission (constructor param 7), in basis points of
    /// amountIn, paid in the input asset to Swap's per-call commissionPkh.
    commission_bps: i64,
    token_reserve: u64,
    lp: [33]u8,
    validator: [33]u8,
    identity: [33]u8,
};

const template: [artifact.script_hex.len / 2]u8 = blk: {
    @setEvalBranchQuota(100_000);
    var out: [artifact.script_hex.len / 2]u8 = undefined;
    _ = std.fmt.hexToBytes(&out, artifact.script_hex) catch unreachable;
    break :blk out;
};

/// The minimal script-number push Rúnar writes for a bigint constructor arg
/// (and for a method's bigint and bool args).
pub fn canonicalNum(p: brc162.Push) ?i64 {
    switch (p.op) {
        brc162.OP_0 => return 0,
        brc162.OP_1NEGATE => return -1,
        brc162.OP_1...brc162.OP_16 => return p.op - (brc162.OP_1 - 1),
        0x01...0x08 => {},
        else => return null,
    }
    const b = p.data;
    const last = b[b.len - 1];
    if (last & 0x7f == 0 and (b.len == 1 or b[b.len - 2] & 0x80 == 0)) return null; // non-minimal
    var mag: u64 = 0;
    for (b, 0..) |x, i| mag |= @as(u64, if (i == b.len - 1) x & 0x7f else x) << @intCast(8 * i);
    if (mag > std.math.maxInt(i63)) return null;
    const v: i64 = @intCast(mag);
    const neg = last & 0x80 != 0;
    // OP_1..OP_16 and OP_1NEGATE are the minimal forms of those values.
    if ((!neg and v >= 1 and v <= 16) or (neg and v == 1)) return null;
    return if (neg) -v else v;
}

/// pool/Pool.runar.go's methods, in declaration order (its ABI's method
/// index): Swap = 0 (validator-signed), Close = 1 (the LP's alone; skein-amm
/// 0.9.0, David Case 2026-10-09: AddLiquidity and RemoveLiquidity are gone).
pub const Method = enum(u1) { swap = 0, close = 1 };

/// The method a pool input's unlocking script selects. gen/main.go's
/// `unlockPool` (mirroring how the Rúnar SDK builds a call) writes the
/// unlocking script as a sequence of pushes only — the method's args, then
/// Rúnar's change output and preimage — and pushes the method index last, so
/// it is the script's final push, minimally encoded (OP_0/OP_1 here).
/// Null when the script cannot be walked as all pushes, is empty, or its
/// last push is not 0 or 1.
pub fn methodOf(script: []const u8) ?Method {
    var pos: usize = 0;
    var last: ?brc162.Push = null;
    while (pos < script.len) {
        const p = brc162.readPush(script, pos) orelse return null;
        last = p;
        pos = p.next;
    }
    const v = canonicalNum(last orelse return null) orelse return null;
    return switch (v) {
        0 => .swap,
        1 => .close,
        else => null,
    };
}

/// A fee at `bps` basis points of `amount`, rounded up as the contract does
/// (`(amount*bps + 9999)/10000`): a nonzero rate always takes at least 1.
/// Null for a negative rate (the contract's arithmetic is not defined here
/// for one) or an overflow.
pub fn feeOf(amount: u64, bps: i64) ?u64 {
    if (bps < 0) return null;
    const f = (@as(u128, amount) * @as(u128, @intCast(bps)) + 9999) / 10000;
    return std.math.cast(u64, f);
}

/// What `Swap(amountIn, bsvIn)` does to a pool holding `bsv_reserve` sats
/// (pool/Pool.runar.go): the three fees, taken in the input asset, the net
/// that enters the pool, the payout `out` in the other asset, and the new
/// reserves.
pub const SwapQuote = struct {
    lp_fee: u64,
    validator_fee: u64,
    commission: u64,
    net: u64,
    out: u64,
    bsv_reserve: u64,
    token_reserve: u64,
};

/// The contract's arithmetic; null where it asserts (net or out not
/// positive, a reserve not positive) or a rate is negative.
pub fn quoteSwap(p: Pool, bsv_reserve: u64, amount_in: u64, bsv_in: bool) ?SwapQuote {
    if (amount_in == 0) return null;
    const lp = feeOf(amount_in, p.lp_fee_bps) orelse return null;
    const val = feeOf(amount_in, p.validator_fee_bps) orelse return null;
    const com = feeOf(amount_in, p.commission_bps) orelse return null;
    const fees = @as(u128, lp) + val + com;
    if (fees >= amount_in) return null;
    const net: u64 = amount_in - @as(u64, @intCast(fees));
    const r_in: u128 = if (bsv_in) bsv_reserve else p.token_reserve;
    const r_out: u128 = if (bsv_in) p.token_reserve else bsv_reserve;
    const out: u64 = @intCast(@as(u128, net) * r_out / (r_in + net));
    if (out == 0 or out >= r_out) return null;
    const new_in = std.math.cast(u64, r_in + net) orelse return null;
    const new_out: u64 = @intCast(r_out - out);
    return .{
        .lp_fee = lp,
        .validator_fee = val,
        .commission = com,
        .net = net,
        .out = out,
        .bsv_reserve = if (bsv_in) new_in else new_out,
        .token_reserve = if (bsv_in) new_out else new_in,
    };
}

/// A plain P2PKH lock.
pub fn p2pkh(pkh: [20]u8) [25]u8 {
    return [3]u8{ 0x76, 0xa9, 0x14 } ++ pkh ++ [2]u8{ 0x88, 0xac };
}

/// The contract's `payoutScript`: `amount` to `pkh`, as BSV (P2PKH) or as
/// the pool's token (a BRC-162 value output, `0x20 <assetId> <amount>
/// OP_2DROP` then P2PKH).
pub fn payoutScript(a: std.mem.Allocator, asset_id: [32]u8, amount: u64, pkh: [20]u8, is_bsv: bool) ![]u8 {
    const lock = p2pkh(pkh);
    if (is_bsv) return a.dupe(u8, &lock);
    var buf: [10]u8 = undefined;
    return std.mem.concat(a, u8, &.{ &.{0x20}, &asset_id, brc162.pushAmount(&buf, amount), &.{0x6d}, &lock });
}

/// The contract's `payoutSats`: the amount for BSV, 1 sat for a token output.
pub fn payoutSats(amount: u64, is_bsv: bool) u64 {
    return if (is_bsv) amount else 1;
}

pub const ParseError = error{BadPool};

/// The pool a lock (the script after the token prefix) is, null when it is
/// not the Pool code, error.BadPool when it is the Pool code with malformed
/// args or state.
pub fn parse(lock: []const u8) ParseError!?Pool {
    var asset_id: ?[32]u8 = null;
    var fees: [3]?i64 = .{ null, null, null };
    var tpos: usize = 0;
    var pos: usize = 0;
    for (artifact.slots) |s| {
        const seg = template[tpos..s.offset];
        if (lock.len - pos < seg.len or !std.mem.eql(u8, lock[pos .. pos + seg.len], seg)) return null;
        pos += seg.len;
        tpos = s.offset + 1;
        const p = brc162.readPush(lock, pos) orelse return null;
        pos = p.next;
        switch (s.param) {
            artifact.param_asset_id => {
                if (p.op != 0x20) return error.BadPool;
                const id = p.data[0..32].*;
                if (asset_id) |prev| if (!std.mem.eql(u8, &prev, &id)) return error.BadPool;
                asset_id = id;
            },
            artifact.param_lp_fee_bps, artifact.param_validator_fee_bps, artifact.param_commission_bps => {
                const v = canonicalNum(p) orelse return error.BadPool;
                const f = &fees[
                    switch (s.param) {
                        artifact.param_lp_fee_bps => 0,
                        artifact.param_validator_fee_bps => 1,
                        else => 2,
                    }
                ];
                if (f.*) |prev| if (prev != v) return error.BadPool;
                f.* = v;
            },
            else => return error.BadPool,
        }
    }
    const tail = template[tpos..];
    if (lock.len - pos < tail.len or !std.mem.eql(u8, lock[pos .. pos + tail.len], tail)) return null;
    pos += tail.len;
    if (lock.len - pos != 1 + state_len or lock[pos] != OP_RETURN) return error.BadPool;
    const st = lock[pos + 1 ..];

    if (st[7] & 0x80 != 0) return error.BadPool; // negative reserve
    const reserve = std.mem.readInt(u64, st[0..8], .little);
    var keys: [3][33]u8 = undefined;
    for (&keys, 0..) |*k, i| {
        k.* = st[8 + 33 * i ..][0..33].*;
        if (k[0] != 0x02 and k[0] != 0x03) return error.BadPool;
    }
    return .{
        .asset_id = asset_id orelse return error.BadPool,
        .lp_fee_bps = fees[0] orelse return error.BadPool,
        .validator_fee_bps = fees[1] orelse return error.BadPool,
        .commission_bps = fees[2] orelse return error.BadPool,
        .token_reserve = reserve,
        .lp = keys[0],
        .validator = keys[1],
        .identity = keys[2],
    };
}

/// The key ID for an outpoint: `<txid display hex>_<vout>`.
pub fn keyId(buf: *[64 + 1 + 10]u8, txid: [32]u8, vout: u32) []const u8 {
    return std.fmt.bufPrint(buf, "{s}_{d}", .{ &c.header.toHex(txid), vout }) catch unreachable;
}

/// The validator signing key for a pool whose key ID is the outpoint
/// txid:vout: BRC-42, the identity key as the (public) parent and the anyone
/// key (private key 1) as the counterparty, which anyone can compute.
pub fn validatorKey(a: std.mem.Allocator, identity: [33]u8, txid: [32]u8, vout: u32) ![33]u8 {
    const kd = bsvz.primitives.key_deriver.KeyDeriver.init(null); // the anyone key
    var buf: [64 + 1 + 10]u8 = undefined;
    const child = try kd.derivePublicKey(a, validator_protocol, keyId(&buf, txid, vout), .{
        .type_ = .other,
        .public_key = try bsvz.primitives.ec.PublicKey.fromSec1(&identity),
    }, false);
    return child.toCompressedSec1();
}

// ---------------------------------------------------------------- the affirmation (0.11.0)

/// The affirmation (skein-amm 0.11.0, David Case 2026-10-21; docs/AMM.md "The affirmation").
/// Before the LP's wallet builds a deploy, the validator checks the proposed pool against its
/// terms and signs, with the pool's validator key (the ValidatorPubKey in state), the pool's
/// canonical parameters and a hash of the terms it agreed to. The deploy's pool output carries
/// that DER signature as its BRC-162 payload (`0x20 <assetId> <amount> OP_2DROP <push sig>
/// OP_DROP` then the Pool code and state); the contract never reads the prefix, and every
/// continuation it writes has no payload. amm-lookup lists a deploy's pool only when the
/// payload verifies (`affirmed`); a continuation is listed when the pool it spends was.
///
/// The terms, as the validator holds them (`config.amm.ammValidator`) and a pool carries them.
pub const Terms = struct {
    lp_fee_bps: i64 = 30,
    validator_fee_bps: i64 = 5,
    commission_bps: i64 = 0,
};

/// The affirmation message's tag (its first bytes): a domain separator and the layout's version.
pub const affirm_tag = "skein-amm affirm 1";

/// The affirmation message's length: the tag, the asset id, three fees, two reserves, three keys
/// and the terms hash.
pub const affirm_len = affirm_tag.len + 32 + 8 * 3 + 8 * 2 + 33 * 3 + 32;

fn le64(out: []u8, v: u64) void {
    std.mem.writeInt(u64, out[0..8], v, .little);
}

/// sha256 of the terms' canonical bytes: lpFeeBps ‖ validatorFeeBps ‖ commissionBps, each a
/// two's-complement int64, little-endian (24 bytes).
pub fn termsHash(t: Terms) [32]u8 {
    var b: [24]u8 = undefined;
    le64(b[0..8], @bitCast(t.lp_fee_bps));
    le64(b[8..16], @bitCast(t.validator_fee_bps));
    le64(b[16..24], @bitCast(t.commission_bps));
    var out: [32]u8 = undefined;
    std.crypto.hash.sha2.Sha256.hash(&b, &out, .{});
    return out;
}

/// The terms a pool carries (its readonly fees).
pub fn termsOf(p: Pool) Terms {
    return .{ .lp_fee_bps = p.lp_fee_bps, .validator_fee_bps = p.validator_fee_bps, .commission_bps = p.commission_bps };
}

/// The bytes the affirmation signs (docs/AMM.md "The affirmation"):
///
///   "skein-amm affirm 1" (18 bytes, ASCII)
///   ‖ AssetId (32: the bytes the pool script pushes, the token's deploy txid in internal order)
///   ‖ LpFeeBps ‖ ValidatorFeeBps ‖ CommissionBps (int64 LE each)
///   ‖ TokenReserve ‖ the pool output's satoshis (uint64 LE each)
///   ‖ LpPubKey ‖ ValidatorPubKey ‖ ValidatorIdentity (33 each, compressed)
///   ‖ termsHash (32: `termsHash` of the terms agreed to)
///
/// 221 bytes; the signature is ECDSA (DER) by the ValidatorPubKey over sha256 of them.
pub fn affirmMessage(p: Pool, satoshis: u64, terms_hash: [32]u8) [affirm_len]u8 {
    var m: [affirm_len]u8 = undefined;
    var at: usize = 0;
    const put = struct {
        fn f(buf: []u8, pos: *usize, bytes: []const u8) void {
            @memcpy(buf[pos.* .. pos.* + bytes.len], bytes);
            pos.* += bytes.len;
        }
    }.f;
    var n: [8]u8 = undefined;
    put(&m, &at, affirm_tag);
    put(&m, &at, &p.asset_id);
    for ([_]i64{ p.lp_fee_bps, p.validator_fee_bps, p.commission_bps }) |f| {
        le64(&n, @bitCast(f));
        put(&m, &at, &n);
    }
    le64(&n, p.token_reserve);
    put(&m, &at, &n);
    le64(&n, satoshis);
    put(&m, &at, &n);
    put(&m, &at, &p.lp);
    put(&m, &at, &p.validator);
    put(&m, &at, &p.identity);
    put(&m, &at, &terms_hash);
    std.debug.assert(at == affirm_len);
    return m;
}

/// sha256 of `affirmMessage`: what the validator's key signs (the signer signs it as is).
pub fn affirmDigest(p: Pool, satoshis: u64, terms_hash: [32]u8) [32]u8 {
    var out: [32]u8 = undefined;
    std.crypto.hash.sha2.Sha256.hash(&affirmMessage(p, satoshis, terms_hash), &out, .{});
    return out;
}

/// Whether `sig` (DER) is the pool's ValidatorPubKey's signature of its affirmation at the
/// terms the pool carries.
pub fn affirmationVerifies(p: Pool, satoshis: u64, sig: []const u8) bool {
    const der = bsvz.crypto.DerSignature.fromDer(sig) catch return false;
    const pk = bsvz.primitives.ec.PublicKey.fromSec1(&p.validator) catch return false;
    return pk.verifyDigest(affirmDigest(p, satoshis, termsHash(termsOf(p))), der) catch false;
}

/// An affirmed deploy's pool output: its pool and the affirmation (the payload). Null when the
/// script is not a pool, or carries no payload.
pub const Affirmed = struct { pool: Pool, sig: []const u8 };

pub fn affirmationOf(script: []const u8) ?Affirmed {
    const tok = brc162.decode(script) orelse return null;
    if (tok.role != .value) return null;
    const sig = tok.payload orelse return null;
    const p = (parse(tok.lock) catch return null) orelse return null;
    return .{ .pool = p, .sig = sig };
}

/// Whether the pool output (`script`, holding `satoshis`) is an affirmed deploy's: a pool whose
/// payload is its ValidatorPubKey's signature of its affirmation.
pub fn affirmed(script: []const u8, satoshis: u64) bool {
    const af = affirmationOf(script) orelse return false;
    return affirmationVerifies(af.pool, satoshis, af.sig);
}

/// The deploy's pool script with the affirmation in: the prefix of `bare` (a pool script with no
/// payload), `<push sig> OP_DROP`, then the rest of `bare`.
pub fn withAffirmation(a: std.mem.Allocator, bare: []const u8, sig: []const u8) ![]u8 {
    const tok = brc162.decode(bare) orelse return error.NotAPool;
    if (tok.payload != null) return error.HasPayload;
    if (sig.len == 0 or sig.len > 0x4b) return error.BadSignature;
    const cut = bare.len - tok.lock.len;
    return std.mem.concat(a, u8, &.{ bare[0..cut], &.{@intCast(sig.len)}, sig, &.{0x75}, tok.lock });
}

pub const Violation = enum {
    malformed,
    not_at_output_0,
    prefix_mismatch,
};

/// Check every pool among a transaction's admitted token outputs
/// (`j.outputs`). Null when they pass. Run by amm-lookup and amm-validator,
/// not by the topic.
pub fn check(j: bsv21.Judgement) ?Violation {
    for (j.outputs) |o| {
        // A pool is a binary value output; JSON outputs are never pools.
        const b = o.token.binary orelse continue;
        if (b.role != .value) continue;
        const p = (parse(b.lock) catch return .malformed) orelse continue;
        if (o.index != 0) return .not_at_output_0;
        // The contract hard-codes a 32-byte id push (vout 0); a 36-byte
        // legacy id cannot match it.
        if (b.id.?.vout != 0 or !std.mem.eql(u8, &p.asset_id, &b.id.?.txid) or p.token_reserve != b.amount) return .prefix_mismatch;
    }
    return null;
}
