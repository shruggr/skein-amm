//! Validator liveness (opldotdev/amm-poc#3, "Revised"): a heartbeat on
//! `tm_<txid>-live` that says "I am validator X (root identity key) and my
//! peer ID is Y", which is also the validator's discovery record.
//!
//! Body (dag-cbor): `{identityKey: bytes(33), peerId: bytes, sig: bytes, at: uint}`.
//!
//! - `at` is milliseconds since the Unix epoch.
//! - `sig` is a DER ECDSA signature over sha256(peerId ‖ at as 8 bytes
//!   big-endian) — BRC-100 `createSignature` over that data — by the BRC-42
//!   child of `identityKey` under BRC-43 security level 1, protocol
//!   `amm live`, key ID `1`, counterparty `anyone` (invoice `1-amm live-1`),
//!   which anyone can derive from the identity key alone.
//!
//! The verdict (`judge`), given the GossipSub sender `from`:
//!
//! - the body does not decode, `peerId` is not `from`, or the signature does
//!   not verify against the derived key → **reject**;
//! - `at` more than `max_skew_ms` ahead of this node's clock, or older than
//!   the offline threshold → **ignore** (not forwarded, no penalty);
//! - otherwise **accept**, answered with the entry it becomes (skein#57:
//!   the front door admits it after the message's own `p2p` entry), in this
//!   program's own box `amm-p2p`:
//!
//!     {kind: "amm-live", identityKey, peerId, at, sig}
//!
//!   Stepped (main.zig), it is re-verified and applied to the last-seen
//!   map `live` (identity key → at ‖ peer ID) under the program's own head:
//!   a later `at` replaces an earlier one, never the reverse.
//!
//! Consumer: `live(map, identityKey, now, threshold)` → the peer ID and
//! time, while `now - at <= threshold`.
const std = @import("std");
const w = @import("chain");

const cbor = w.cbor;
const Value = cbor.Value;
const Allocator = std.mem.Allocator;
const bsvz = w.bsvz;
const kd = bsvz.primitives.key_deriver;
const ec = bsvz.primitives.ec;

pub const protocol = kd.Protocol{ .security_level = 1, .name = "amm live" };
pub const key_id = "1";

pub const default_interval_s: u64 = 30;
pub const default_offline_s: u64 = 90;
/// How far ahead of our clock a heartbeat may be dated.
pub const max_skew_ms: u64 = 60_000;

pub const Body = struct { identity_key: [33]u8, peer_id: []const u8, sig: []const u8, at: u64 };

/// The beacon (shruggr/skein#126, docs/MESSAGES.md "emit"): the host's libp2p node publishes
/// `body` on `topic` every `every_ms`, without subscribing it.
pub fn beaconEvent(a: Allocator, topic: []const u8, every_ms: u64, body: []const u8) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "event", .value = .{ .text = "beacon" } },
        .{ .key = "topic", .value = .{ .text = topic } },
        .{ .key = "every", .value = .{ .uint = every_ms } },
        .{ .key = "body", .value = .{ .bytes = body } },
    }) };
}

/// The beacon on `topic` stopped.
pub fn unbeaconEvent(a: Allocator, topic: []const u8) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "event", .value = .{ .text = "unbeacon" } },
        .{ .key = "topic", .value = .{ .text = topic } },
    }) };
}

pub fn encodeBody(a: Allocator, b: Body) ![]u8 {
    return cbor.encode(a, .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "identityKey", .value = .{ .bytes = try a.dupe(u8, &b.identity_key) } },
        .{ .key = "peerId", .value = .{ .bytes = b.peer_id } },
        .{ .key = "sig", .value = .{ .bytes = b.sig } },
        .{ .key = "at", .value = .{ .uint = b.at } },
    }) });
}

pub fn decodeBody(a: Allocator, bytes: []const u8) !Body {
    const v = cbor.decode(a, bytes) catch return error.Malformed;
    return bodyOf(v);
}

fn bodyOf(v: Value) !Body {
    const ik = v.getBytes("identityKey") orelse return error.Malformed;
    if (ik.len != 33) return error.Malformed;
    const pid = v.getBytes("peerId") orelse return error.Malformed;
    if (pid.len == 0) return error.Malformed;
    return .{
        .identity_key = ik[0..33].*,
        .peer_id = pid,
        .sig = v.getBytes("sig") orelse return error.Malformed,
        .at = v.getUint("at") orelse return error.Malformed,
    };
}

/// The signed data: peerId ‖ at (u64 big-endian).
pub fn message(a: Allocator, peer_id: []const u8, at: u64) ![]u8 {
    var be: [8]u8 = undefined;
    std.mem.writeInt(u64, &be, at, .big);
    return std.mem.concat(a, u8, &.{ peer_id, &be });
}

/// What is signed: sha256 of the data (BRC-100 createSignature over data).
pub fn digest(a: Allocator, peer_id: []const u8, at: u64) ![32]u8 {
    return bsvz.crypto.hash.sha256(try message(a, peer_id, at)).bytes;
}

/// The attestation key: the anyone-counterparty child of the identity key.
pub fn attestationKey(a: Allocator, identity: [33]u8) !ec.PublicKey {
    const anyone = kd.KeyDeriver.init(null);
    return anyone.derivePublicKey(a, protocol, key_id, .{ .type_ = .other, .public_key = try ec.PublicKey.fromSec1(&identity) }, false);
}

/// Whether `sig` is the identity's attestation of `peer_id` at `at`.
pub fn verify(a: Allocator, b: Body) bool {
    const key = attestationKey(a, b.identity_key) catch return false;
    const der = bsvz.crypto.DerSignature.fromDer(b.sig) catch return false;
    return key.verifyDigest(digest(a, b.peer_id, b.at) catch return false, der) catch false;
}

/// Sign an attestation with the root private key (tests; a validator signs through its wallet).
pub fn sign(a: Allocator, root: ec.PrivateKey, peer_id: []const u8, at: u64) !Body {
    const child = try kd.KeyDeriver.init(root).derivePrivateKey(a, protocol, key_id, .{ .type_ = .anyone });
    const s = try child.signDigest(try digest(a, peer_id, at));
    return .{ .identity_key = (try root.publicKey()).toCompressedSec1(), .peer_id = peer_id, .sig = try a.dupe(u8, s.asSlice()), .at = at };
}

pub const Outcome = union(enum) {
    accept: Body,
    reject: []const u8,
    ignore: []const u8,
};

/// The verdict on a heartbeat from GossipSub sender `from`, at our time `now` (ms).
pub fn judge(a: Allocator, body_bytes: []const u8, from: []const u8, now: u64, offline_ms: u64) Outcome {
    const b = decodeBody(a, body_bytes) catch return .{ .reject = "Malformed" };
    if (!std.mem.eql(u8, b.peer_id, from)) return .{ .reject = "WrongPeer" };
    if (!verify(a, b)) return .{ .reject = "BadSignature" };
    if (b.at > now + max_skew_ms) return .{ .ignore = "FromTheFuture" };
    if (now > b.at and now - b.at > offline_ms) return .{ .ignore = "Stale" };
    return .{ .accept = b };
}

/// The entry an accepted heartbeat becomes (box `amm-p2p`), applied by `apply`.
pub fn liveEvent(a: Allocator, b: Body) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "kind", .value = .{ .text = "amm-live" } },
        .{ .key = "identityKey", .value = .{ .bytes = try a.dupe(u8, &b.identity_key) } },
        .{ .key = "peerId", .value = .{ .bytes = b.peer_id } },
        .{ .key = "at", .value = .{ .uint = b.at } },
        .{ .key = "sig", .value = .{ .bytes = b.sig } },
    }) };
}

// ---------------------------------------------------------------- the last-seen map

pub const Seen = struct { peer_id: []const u8, at: u64 };

pub fn seenOf(v: w.store.MValue) ?Seen {
    if (v != .bytes or v.bytes.len < 9) return null;
    return .{ .at = std.mem.readInt(u64, v.bytes[0..8], .big), .peer_id = v.bytes[8..] };
}

/// The last heartbeat recorded for an identity.
pub fn lastSeen(m: *w.store.Map, identity: [33]u8) !?Seen {
    return seenOf((try m.get(&identity)) orelse return null);
}

/// Apply an `amm-live` entry (re-verified: an entry is only as good as its
/// signature) to the map: a later `at` replaces an earlier one. → whether it changed.
pub fn apply(a: Allocator, m: *w.store.Map, event: Value) !bool {
    const b = try bodyOf(event);
    if (!verify(a, b)) return error.BadSignature;
    if (try lastSeen(m, b.identity_key)) |s| if (s.at >= b.at) return false;
    var be: [8]u8 = undefined;
    std.mem.writeInt(u64, &be, b.at, .big);
    try m.put(&b.identity_key, .{ .bytes = try std.mem.concat(a, u8, &.{ &be, b.peer_id }) });
    return true;
}

/// Consumer API: the identity's peer ID and last heartbeat, while live
/// (`now - at <= threshold`, all ms); null when never seen or offline.
pub fn live(m: *w.store.Map, identity: [33]u8, now: u64, threshold: u64) !?Seen {
    const s = (try lastSeen(m, identity)) orelse return null;
    if (now > s.at and now - s.at > threshold) return null;
    return s;
}
