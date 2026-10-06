//! Validator liveness (opldotdev/amm-poc#3, "Revised"): a heartbeat on
//! `tm_<txid>-live` that says "I am validator X (root identity key) and my
//! peer ID is Y", which is also the validator's discovery record.
//!
//! The host beats it (shruggr/skein#126, skein docs/MESSAGES.md "Beacons"):
//! amm-p2p declares the body once; the host's libp2p node publishes a new
//! frame every beat, signed by the instance's signer:
//!
//!   frame: dag-cbor {body: bytes, at: uint (ms since the epoch, the host's
//!          clock), sender: bytes(33) (the instance's identity key),
//!          signature: bytes (DER)}
//!   body:  dag-cbor {identityKey: bytes(33), peerId: bytes}
//!
//! `signature` is BRC-100 `createSignature` by the instance under
//! `[2, "metanet handles envelope"]`, key ID `send`, counterparty `anyone`
//! (verified with the anyone-derived child of `sender`), over sha2-256 of
//! dag-cbor `{kind: "beacon", topic, body, at, sender}` — the topic is
//! signed, not carried (skein src/host/p2p.ts `beaconPreimage`).
//!
//! The verdict (`judge`), given the topic and the GossipSub sender `from`:
//!
//! - the frame or the body does not decode, `identityKey` is not `sender`,
//!   `peerId` is not `from`, or the signature does not verify → **reject**;
//! - `at` more than `max_skew_ms` ahead of this node's clock, or older than
//!   the offline threshold → **ignore** (not forwarded, no penalty);
//! - otherwise **accept**, answered with the entry it becomes (skein#57:
//!   the front door admits it after the message's own `p2p` entry), in this
//!   program's own box `amm-p2p`:
//!
//!     {kind: "amm-live", topic, body, at, sender, signature}
//!
//!   Stepped (main.zig), it is re-verified and applied to the last-seen
//!   map `live` (identity key → at ‖ peer ID) under the program's own head:
//!   a later `at` replaces an earlier one, never the reverse.
//!
//! Who judges (shruggr/skein#120, David 2026-10-06): a host serving a
//! market (`ammP2p.market`) subscribes `tm_<txid>-live` to `validateLive`
//! for each token it serves (`subscribeEvent`, `plan`); no other host does.
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

/// The instance's signing protocol for a beat (skein providers.ts `SIGN_PROTOCOL`, `MESSAGE_KEY_ID`).
pub const protocol = kd.Protocol{ .security_level = 2, .name = "metanet handles envelope" };
pub const key_id = "send";

pub const default_interval_s: u64 = 30;
pub const default_offline_s: u64 = 90;
/// How far ahead of our clock a heartbeat may be dated.
pub const max_skew_ms: u64 = 60_000;

/// What amm-p2p declares: who it is and its peer ID.
pub const Body = struct { identity_key: [33]u8, peer_id: []const u8 };
/// A beat as published: the declared body (its bytes), the beat's time, the instance, its signature.
pub const Beat = struct { body: []const u8, at: u64, sender: [33]u8, signature: []const u8 };
/// A beat heard on `topic`, its body read.
pub const Heard = struct { topic: []const u8, beat: Beat, body: Body };

/// The beacon (shruggr/skein#126, docs/MESSAGES.md "emit"): the host's libp2p node publishes
/// a frame of `body` on `topic` every `every_ms`, without subscribing it.
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

// ---------------------------------------------------------------- the market role

/// What the kernel delivers a `-live` beat to (shruggr/skein#119 `subscribe {topic, program, fn}`):
/// this program's role in the app's record and the handler that judges a frame.
pub const live_program = "amm-p2p";
pub const live_fn = "validateLive";

/// A market's subscription (shruggr/skein#120, David 2026-10-06: liveness by role): deliver each
/// beat on `topic` (`tm_<txid>-live`) to amm-p2p's `validateLive`.
pub fn subscribeEvent(a: Allocator, topic: []const u8) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "event", .value = .{ .text = "subscribe" } },
        .{ .key = "topic", .value = .{ .text = topic } },
        .{ .key = "program", .value = .{ .text = live_program } },
        .{ .key = "fn", .value = .{ .text = live_fn } },
    }) };
}

/// The subscription to `topic` ended (the app's own: shruggr/skein#119).
pub fn unsubscribeEvent(a: Allocator, topic: []const u8) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "event", .value = .{ .text = "unsubscribe" } },
        .{ .key = "topic", .value = .{ .text = topic } },
    }) };
}

/// The subscriptions to change: the token topics `want`ed and not `standing` (subscribe), and the
/// ones `standing` and no longer wanted (unsubscribe). A market wants every served token topic
/// while started; a stop, or a host that is not a market, wants none.
pub const Plan = struct { subscribe: []const []const u8, unsubscribe: []const []const u8 };

pub fn plan(a: Allocator, standing: []const []const u8, want: []const []const u8) !Plan {
    var sub: std.ArrayList([]const u8) = .empty;
    var unsub: std.ArrayList([]const u8) = .empty;
    for (want) |t| if (!contains(standing, t) and !contains(sub.items, t)) try sub.append(a, t);
    for (standing) |t| if (!contains(want, t)) try unsub.append(a, t);
    return .{ .subscribe = sub.items, .unsubscribe = unsub.items };
}

fn contains(xs: []const []const u8, x: []const u8) bool {
    for (xs) |y| if (std.mem.eql(u8, x, y)) return true;
    return false;
}

pub fn encodeBody(a: Allocator, b: Body) ![]u8 {
    return cbor.encode(a, .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "identityKey", .value = .{ .bytes = try a.dupe(u8, &b.identity_key) } },
        .{ .key = "peerId", .value = .{ .bytes = b.peer_id } },
    }) });
}

pub fn decodeBody(a: Allocator, bytes: []const u8) !Body {
    const v = cbor.decode(a, bytes) catch return error.Malformed;
    const ik = v.getBytes("identityKey") orelse return error.Malformed;
    if (ik.len != 33) return error.Malformed;
    const pid = v.getBytes("peerId") orelse return error.Malformed;
    if (pid.len == 0) return error.Malformed;
    return .{ .identity_key = ik[0..33].*, .peer_id = pid };
}

fn beatFields(a: Allocator, b: Beat) ![]cbor.Entry {
    return a.dupe(cbor.Entry, &.{
        .{ .key = "body", .value = .{ .bytes = b.body } },
        .{ .key = "at", .value = .{ .uint = b.at } },
        .{ .key = "sender", .value = .{ .bytes = try a.dupe(u8, &b.sender) } },
        .{ .key = "signature", .value = .{ .bytes = b.signature } },
    });
}

/// The frame as the host publishes it (p2p.ts `beaconFrame`).
pub fn encodeFrame(a: Allocator, b: Beat) ![]u8 {
    return cbor.encode(a, .{ .map = try beatFields(a, b) });
}

fn beatOf(v: Value) !Beat {
    const sender = v.getBytes("sender") orelse return error.Malformed;
    if (sender.len != 33) return error.Malformed;
    return .{
        .body = v.getBytes("body") orelse return error.Malformed,
        .at = v.getUint("at") orelse return error.Malformed,
        .sender = sender[0..33].*,
        .signature = v.getBytes("signature") orelse return error.Malformed,
    };
}

pub fn decodeFrame(a: Allocator, bytes: []const u8) !Beat {
    const v = cbor.decode(a, bytes) catch return error.Malformed;
    return beatOf(v);
}

/// What a beat's signature covers: dag-cbor {kind: "beacon", topic, body, at, sender} (p2p.ts `beaconPreimage`).
pub fn preimage(a: Allocator, topic: []const u8, body: []const u8, at: u64, sender: [33]u8) ![]u8 {
    return cbor.encode(a, .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "kind", .value = .{ .text = "beacon" } },
        .{ .key = "topic", .value = .{ .text = topic } },
        .{ .key = "body", .value = .{ .bytes = body } },
        .{ .key = "at", .value = .{ .uint = at } },
        .{ .key = "sender", .value = .{ .bytes = try a.dupe(u8, &sender) } },
    }) });
}

/// What is signed: sha256 of the preimage (BRC-100 createSignature over data).
pub fn digest(a: Allocator, topic: []const u8, body: []const u8, at: u64, sender: [33]u8) ![32]u8 {
    return bsvz.crypto.hash.sha256(try preimage(a, topic, body, at, sender)).bytes;
}

/// The key a beat verifies against: the anyone-counterparty child of the sender.
pub fn signingKey(a: Allocator, sender: [33]u8) !ec.PublicKey {
    const anyone = kd.KeyDeriver.init(null);
    return anyone.derivePublicKey(a, protocol, key_id, .{ .type_ = .other, .public_key = try ec.PublicKey.fromSec1(&sender) }, false);
}

/// Whether `b` is the sender's beat on `topic`.
pub fn verify(a: Allocator, topic: []const u8, b: Beat) bool {
    const key = signingKey(a, b.sender) catch return false;
    const der = bsvz.crypto.DerSignature.fromDer(b.signature) catch return false;
    return key.verifyDigest(digest(a, topic, b.body, b.at, b.sender) catch return false, der) catch false;
}

/// A beat signed with the root private key (tests; the host signs through the instance's signer).
pub fn sign(a: Allocator, root: ec.PrivateKey, topic: []const u8, body: []const u8, at: u64) !Beat {
    const sender = (try root.publicKey()).toCompressedSec1();
    const child = try kd.KeyDeriver.init(root).derivePrivateKey(a, protocol, key_id, .{ .type_ = .anyone });
    const s = try child.signDigest(try digest(a, topic, body, at, sender));
    return .{ .body = body, .at = at, .sender = sender, .signature = try a.dupe(u8, s.asSlice()) };
}

pub const Outcome = union(enum) {
    accept: Heard,
    reject: []const u8,
    ignore: []const u8,
};

/// A beat checked: its body read, the body's identity the sender, the signature the sender's.
fn heard(a: Allocator, topic: []const u8, beat: Beat) !Heard {
    const body = try decodeBody(a, beat.body);
    if (!std.mem.eql(u8, &body.identity_key, &beat.sender)) return error.WrongIdentity;
    if (!verify(a, topic, beat)) return error.BadSignature;
    return .{ .topic = topic, .beat = beat, .body = body };
}

/// The verdict on a frame on `topic` from GossipSub sender `from`, at our time `now` (ms).
pub fn judge(a: Allocator, topic: []const u8, frame: []const u8, from: []const u8, now: u64, offline_ms: u64) Outcome {
    const beat = decodeFrame(a, frame) catch return .{ .reject = "Malformed" };
    const h = heard(a, topic, beat) catch |e| return .{ .reject = @errorName(e) };
    if (!std.mem.eql(u8, h.body.peer_id, from)) return .{ .reject = "WrongPeer" };
    if (beat.at > now + max_skew_ms) return .{ .ignore = "FromTheFuture" };
    if (now > beat.at and now - beat.at > offline_ms) return .{ .ignore = "Stale" };
    return .{ .accept = h };
}

/// The entry an accepted heartbeat becomes (box `amm-p2p`), applied by `apply`: the frame and its topic.
pub fn liveEvent(a: Allocator, h: Heard) !Value {
    const fields = try beatFields(a, h.beat);
    return .{ .map = try std.mem.concat(a, cbor.Entry, &.{ &.{
        .{ .key = "kind", .value = .{ .text = "amm-live" } },
        .{ .key = "topic", .value = .{ .text = h.topic } },
    }, fields }) };
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
    const h = try heard(a, event.getText("topic") orelse return error.Malformed, try beatOf(event));
    const at = h.beat.at;
    if (try lastSeen(m, h.body.identity_key)) |s| if (s.at >= at) return false;
    var be: [8]u8 = undefined;
    std.mem.writeInt(u64, &be, at, .big);
    try m.put(&h.body.identity_key, .{ .bytes = try std.mem.concat(a, u8, &.{ &be, h.body.peer_id }) });
    return true;
}

/// Consumer API: the identity's peer ID and last heartbeat, while live
/// (`now - at <= threshold`, all ms); null when never seen or offline.
pub fn live(m: *w.store.Map, identity: [33]u8, now: u64, threshold: u64) !?Seen {
    const s = (try lastSeen(m, identity)) orelse return null;
    if (now > s.at and now - s.at > threshold) return null;
    return s;
}
