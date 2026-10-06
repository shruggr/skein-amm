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
//! Who reads it (0.4.0; shruggr/skein#120, #138, David 2026-10-06): not this program. A host
//! serving a market (`ammP2p.market`) asks the runtime's liveness tool once per token it serves,
//! `{event: "liveness", topic: "tm_<txid>-live", window: offlineSeconds × 1000}` (`livenessEvent`,
//! `plan`); the tool subscribes the topic without admitting its messages, verifies each beat's
//! signature against `sender` and keeps the beats newer than `window`, the latest per sender, in
//! host memory, served at `GET /<app>/.live/<topic>` (skein docs/MESSAGES.md "Liveness (#138)").
//! The page reads that and names the validator; nothing of it is in this program's state. The body
//! (`Body`) stays this app's: the page decodes it.
//!
//! Who beacons (0.3.1, David 2026-10-06): not a role — a node beacons
//! `tm_<txid>-live` for the topics it validates, set up per topic by the
//! owner (`validate` / `unvalidate`, `validation`).
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
/// The liveness window (`ammP2p.offlineSeconds`): a margin over the 30 s beat (David 2026-10-06).
pub const default_offline_s: u64 = 40;

/// What amm-p2p declares: who it is and its peer ID.
pub const Body = struct { identity_key: [33]u8, peer_id: []const u8 };
/// A beat as published: the declared body (its bytes), the beat's time, the instance, its signature.
pub const Beat = struct { body: []const u8, at: u64, sender: [33]u8, signature: []const u8 };

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

/// A market's liveness (shruggr/skein#138, David 2026-10-06): the runtime's liveness tool keeps the
/// verified beats on `topic` (`tm_<txid>-live`) newer than `window_ms`, served at
/// `GET /<app>/.live/<topic>`. Recorded once; no answer comes.
pub fn livenessEvent(a: Allocator, topic: []const u8, window_ms: u64) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "event", .value = .{ .text = "liveness" } },
        .{ .key = "topic", .value = .{ .text = topic } },
        .{ .key = "window", .value = .{ .uint = window_ms } },
    }) };
}

/// The liveness on `topic` ended (its set is gone).
pub fn unlivenessEvent(a: Allocator, topic: []const u8) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "event", .value = .{ .text = "unliveness" } },
        .{ .key = "topic", .value = .{ .text = topic } },
    }) };
}

/// 0.3.x's subscription of `topic` (`subscribe {topic, program: "amm-p2p", fn: "validateLive"}`,
/// shruggr/skein#119) ended: emitted once per topic that 0.3.x left standing, at the first start or
/// stop under 0.4.0 (main.zig `market`), since `validateLive` is gone.
pub fn unsubscribeEvent(a: Allocator, topic: []const u8) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "event", .value = .{ .text = "unsubscribe" } },
        .{ .key = "topic", .value = .{ .text = topic } },
    }) };
}

/// The liveness to change: the token topics `want`ed and not `standing` (liveness), and the ones
/// `standing` and no longer wanted (unliveness). A market wants every served token topic while
/// started; a stop, or a host that is not a market, wants none. Only a standing topic is ended.
pub const Plan = struct { liveness: []const []const u8, unliveness: []const []const u8 };

pub fn plan(a: Allocator, standing: []const []const u8, want: []const []const u8) !Plan {
    var on: std.ArrayList([]const u8) = .empty;
    var off: std.ArrayList([]const u8) = .empty;
    for (want) |t| if (!contains(standing, t) and !contains(on.items, t)) try on.append(a, t);
    for (standing) |t| if (!contains(want, t)) try off.append(a, t);
    return .{ .liveness = on.items, .unliveness = off.items };
}

/// Validation (0.3.1, David 2026-10-06: beaconing is not a role, it comes with validating a topic).
/// The owner sets it up per topic, like a topic's registration: `validate {topic}` adds the topic to
/// the validated set and beacons it; `unvalidate {topic}` removes it and ends its beacon. A stop ends
/// every beacon and keeps the set; a start beacons the set again (and ends any beacon outside it).
pub const ValidationOp = enum { validate, unvalidate, start, stop };

/// What an op does, given the validated set and the beacons standing: the set after it, the topics
/// to `beacon` and the ones to `unbeacon`.
pub const Validation = struct { validated: []const []const u8, beacon: []const []const u8, unbeacon: []const []const u8 };

pub fn validation(a: Allocator, op: ValidationOp, validated: []const []const u8, beaconing: []const []const u8, topic: ?[]const u8) !Validation {
    var set: std.ArrayList([]const u8) = .empty;
    var beacon: std.ArrayList([]const u8) = .empty;
    var unbeacon: std.ArrayList([]const u8) = .empty;
    switch (op) {
        .validate => {
            const t = topic orelse return error.NoTopic;
            try set.appendSlice(a, validated);
            if (!contains(validated, t)) try set.append(a, t);
            if (!contains(beaconing, t)) try beacon.append(a, t);
        },
        .unvalidate => {
            const t = topic orelse return error.NoTopic;
            for (validated) |v| if (!std.mem.eql(u8, v, t)) try set.append(a, v);
            if (contains(beaconing, t)) try unbeacon.append(a, t);
        },
        .start => {
            try set.appendSlice(a, validated);
            try beacon.appendSlice(a, validated);
            for (beaconing) |b| if (!contains(validated, b)) try unbeacon.append(a, b);
        },
        .stop => {
            try set.appendSlice(a, validated);
            try unbeacon.appendSlice(a, beaconing);
        },
    }
    return .{ .validated = set.items, .beacon = beacon.items, .unbeacon = unbeacon.items };
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
