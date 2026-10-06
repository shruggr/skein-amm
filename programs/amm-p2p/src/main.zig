//! amm-p2p: the AMM overlay's validator liveness beacon, the marketplace
//! relay and the app's pages, as one skein program (Zig, wasm32-wasi).
//! The standard overlay gossip — submissions on `<topic>`, STEAKs on
//! `<topic>-admit`, proofs on `<topic>-proof` — is skein's overlay engine's
//! (skein#74), not this program's.
//!
//! The verdicts, bodies and plans are in liveness.zig, proofs.zig and
//! schedule.zig (pure, tested natively); the libp2p shapes are libp2p.zig.
//! This file wires them to the VM.
//!
//! **Called** (input kind "call"; `fn`):
//!
//!   validateLive    a libp2p:tm_<txid>-live message → {verdict, reason?, admit?: [the amm-live entry (box amm/amm-p2p)]}
//!                   (delivered by the market role's subscription, shruggr/skein#119: `ammP2p.market`)
//!   proofsByBlock   a /amm/proofs/1.0.0 frame → {verdict: accept, body: {bump} | {missing: true}}
//!                   (a utility: no row routes it since 0.2.0; sync is shruggr/skein#112's `want`)
//!   serve           route http `/` (prefix): the app's pages, `www/` of the app's own tree
//!                   (skein-sdk `files.serve`, shruggr/skein#125)
//!   live            {identityKey: bytes(33) | hex, threshold?: ms} → {peerId, peerIdText, at} | null   (the consumer API)
//!   call            route /amm/call (APPS.md §4): {fn, args} → {fn, result} | {fn, error}; `amm.swap.submit`,
//!                   `amm.pool.submit` and `amm.liquidity.submit` answer {wait: true} and, called again with
//!                   `resolved`, the record
//!   amm.swap.submit, amm.swap.status, amm.swap.terms   an in-VM call of the interface amm.swap/1 (relay.zig)
//!   amm.pool.submit, amm.pool.status   an in-VM call of the interface amm.pool/1 (relay.zig: the pool deploy)
//!   amm.liquidity.submit, amm.liquidity.status   an in-VM call of the interface amm.liquidity/1 (relay.zig: AddLiquidity)
//!
//! **Stepped** (a `mailbox` row from anyone on the boxes `amm` and `amm/amm-p2p`; from the owner on
//! `amm/validate`):
//!
//!   event   {kind: "amm-live", …}                      an accepted heartbeat: the last-seen map
//!   message {fn: "validate" | "unvalidate", args: {topic: "tm_<txid>"}}   in box `amm/validate` only
//!                                                     (0.3.2: the row from `$owner` is the permission;
//!                                                     the set gates amm-validator's signing): the
//!                                                     topic into (out of) the validated set, its
//!                                                     `beacon` (`unbeacon`) on `tm_<txid>-live`;
//!                                                     idempotent; answered {topic, validating}
//!   message {kind: "amm-p2p-start" | "amm-p2p-stop"}   from the owner (the manifest's `start`, in box `amm`):
//!                                                     a `beacon` event per validated topic on
//!                                                     `tm_<txid>-live` (the host publishes it every
//!                                                     heartbeatSeconds), or `unbeacon` for each (the
//!                                                     set kept); with
//!                                                     `ammP2p.market`, a `subscribe` of each served
//!                                                     `tm_<txid>-live` to `validateLive` (or `unsubscribe`)
//!   message {kind: "amm-p2p-start" | "amm-p2p-stop", jobs: [...]}   the cron fallback: the schedules asked of
//!                                                     (or stopped at) the cron provider (`local` `cron`)
//!   message {kind: "amm-p2p-tick", job, name, due}      from the cron provider (skein#69): the heartbeat
//!                                                     (publish), or a catch-up pass (a thread resting on
//!                                                     the libp2p provider's answers to its direct calls)
//!   message {fn, args} in box `amm`                    the app's box (APPS.md §4): amm.swap.submit | status | terms,
//!                                                     amm.pool.submit | status, amm.liquidity.submit | status,
//!                                                     answered to the sender (a submit: when the relay settles)
//!   thread  {kind: "amm-swap-relay" | "amm-pool-relay" | "amm-liquidity-relay", id}   the relay (launched by a submit): a thread resting on
//!                                                     the libp2p provider's answers to its dial of the validator
//!
//! State under the head `amm/p2p` (`relay.p2p_head`): {kind: "amm-p2p-state", maps: {live, cursor, beacons, subscriptions}, validated: ["tm_<txid>", …]}.
//! The app's state under the head `amm/app` (relay.zig `Book`; skein #77:
//! `app.headOf`): the installed app record's `state`, or (an instance wired
//! by its genesis, no app record) the head's root itself: {kind:
//! "amm-app-state", swaps, pools, liquidity}.
//!
//! Config: the installed app record's `config.amm` (`ammP2p`, `commission`),
//! else genesis `defaults.ammP2p` and `defaults.ammCommission`, JSON objects
//! in strings; the topics (`ammP2p.topics`, else every `tm_<txid>` the
//! overlay serves, declared or registered with the engine, as the engine
//! reads its configuration: skein-overlay `engine_vm.configured`);
//! the functions' declarations: the app record's `provides`, else genesis
//! `defaults.ammProvides` (JSON) (README.md).
const std = @import("std");
const w = @import("chain");
const ov = @import("skein_overlay");
const vm = @import("overlay_sk");
const wire = @import("wallet").wire;
const names = @import("names.zig");
const libp2p = @import("libp2p.zig");
const schedule = @import("schedule.zig");
const proofs = @import("proofs.zig");
const liveness = @import("liveness.zig");
const views = @import("views.zig");
const relay = @import("relay.zig");
const app = @import("app");
const sk = @import("sk");
const files = @import("files");
const scbor = @import("sdk_cbor");
const dagjson = @import("dagjson");

const cbor = w.cbor;
const Value = cbor.Value;
const Map = w.store.Map;
const Allocator = std.mem.Allocator;
const eql = std.mem.eql;

/// The imports skein-overlay's `sk` module does not declare: the signer.
const ext = struct {
    extern "skein" fn wallet(frame: [*]const u8, len: u32, out: [*]u8, cap: u32) i32;
};

pub fn main() u8 {
    return vm.main("amm-p2p", run);
}

/// The input as the overlay engine reads its configuration (skein-overlay `engine_vm.configured`:
/// the app this program was installed in, its served topics with the registered ones in (the
/// engine's head `<app>/topics`), its roles), for the topics this program works on (`config`).
var overlay_in: ?Value = null;

fn run(a: Allocator) anyerror!void {
    const in = try vm.input(a);
    const is_call = eql(u8, in.getText("kind") orelse "", "call");
    overlay_in = ov.engine_vm.configured(a, in, if (is_call) vm.callArg(a, in) catch null else null) catch null;
    if (is_call) return call(a, in);
    return step(a, in);
}

// ---------------------------------------------------------------- config

const Config = struct {
    topics: []const []const u8,
    heartbeat_s: u64 = liveness.default_interval_s,
    offline_s: u64 = liveness.default_offline_s,
    catchup_s: u64 = 600,
    window: u32 = 12,
    batch: u32 = 6,
    /// Catch-up peers: base58 peer IDs or multiaddrs with /p2p/<id>, as `dial` takes them.
    peers: []const []const u8 = &.{},
    /// How long catch-up waits on a direct call's answer (ms).
    reply_timeout_ms: u64 = 30_000,
    /// The market role (`ammP2p.market`, shruggr/skein#120, David 2026-10-06): this host serves a
    /// market, so it subscribes the `tm_<txid>-live` beacons of the tokens it serves and keeps the
    /// validator map the relay picks from. Off: nothing subscribed, the map stays empty.
    market: bool = false,
};

/// A configuration section: the installed app record's `config.amm.<key>`
/// (as JSON text), else the genesis default `<genesis>` (JSON text).
fn configText(a: Allocator, in: Value, key: []const u8, genesis: []const u8) !?[]const u8 {
    if (try appRecord(a)) |rec| if (rec.get("config")) |c| if (c.get(relay.app_name)) |amm| if (amm.get(key)) |v| return try jsonOf(a, v);
    const d = in.get("defaults") orelse return null;
    return d.getText(genesis);
}

/// A configuration value (maps, lists, text, numbers, booleans) as JSON text.
fn jsonOf(a: Allocator, v: Value) ![]const u8 {
    var out: std.Io.Writer.Allocating = .init(a);
    var jw: std.json.Stringify = .{ .writer = &out.writer };
    try writeJson(&jw, v);
    return out.written();
}

fn writeJson(jw: *std.json.Stringify, v: Value) !void {
    switch (v) {
        .map => |es| {
            try jw.beginObject();
            for (es) |e| {
                try jw.objectField(e.key);
                try writeJson(jw, e.value);
            }
            try jw.endObject();
        },
        .array => |xs| {
            try jw.beginArray();
            for (xs) |x| try writeJson(jw, x);
            try jw.endArray();
        },
        .text => |t| try jw.write(t),
        .boolean => |b| try jw.write(b),
        .uint => |n| try jw.write(n),
        .nint => |n| try jw.write(-1 - @as(i128, n)),
        .null => try jw.write(null),
        else => return error.BadConfig,
    }
}

/// The relay's commission: `{pkh?: <40 hex> | null}`.
fn commissionConfig(a: Allocator, in: Value) !relay.Commission {
    var c: relay.Commission = .{};
    const text = (try configText(a, in, "commission", "ammCommission")) orelse return c;
    const j = std.json.parseFromSliceLeaky(std.json.Value, a, text, .{}) catch return error.BadConfig;
    if (j != .object) return error.BadConfig;
    if (j.object.get("pkh")) |v| switch (v) {
        .null => {},
        .string => |h| {
            var pkh: [20]u8 = undefined;
            if (h.len != 40) return error.BadConfig;
            _ = std.fmt.hexToBytes(&pkh, h) catch return error.BadConfig;
            c.pkh = pkh;
        },
        else => return error.BadConfig,
    };
    return c;
}

fn config(a: Allocator, in: Value) !Config {
    var cfg: Config = .{ .topics = &.{} };
    if (try configText(a, in, "ammP2p", "ammP2p")) |text| {
        const j = std.json.parseFromSliceLeaky(std.json.Value, a, text, .{}) catch return error.BadConfig;
        if (j != .object) return error.BadConfig;
        const o = j.object;
        inline for (.{ .{ "heartbeatSeconds", "heartbeat_s" }, .{ "offlineSeconds", "offline_s" }, .{ "catchupSeconds", "catchup_s" }, .{ "window", "window" }, .{ "batch", "batch" }, .{ "replyTimeoutMs", "reply_timeout_ms" } }) |f| {
            if (o.get(f[0])) |v| @field(cfg, f[1]) = if (v == .integer and v.integer > 0) @intCast(v.integer) else return error.BadConfig;
        }
        if (o.get("market")) |v| cfg.market = if (v == .bool) v.bool else return error.BadConfig;
        if (o.get("topics")) |v| cfg.topics = try strings(a, v);
        if (o.get("peers")) |v| cfg.peers = try strings(a, v);
    }
    if (cfg.topics.len == 0) {
        // Every token topic `tm_<txid>` the overlay serves now: the ones registered with the
        // engine (its head `<app>/topics`) and any `config.overlay.topics` names.
        const cin = overlay_in orelse in;
        var out: std.ArrayList([]const u8) = .empty;
        for (ov.calls.servedTopics(a, cin) catch return cfg) |t| if (names.parse(t)) |n| if (n.kind == .overlay) try out.append(a, t);
        cfg.topics = out.items;
    }
    return cfg;
}

fn strings(a: Allocator, v: std.json.Value) ![]const []const u8 {
    if (v != .array) return error.BadConfig;
    const out = try a.alloc([]const u8, v.array.items.len);
    for (v.array.items, out) |x, *o| o.* = if (x == .string) x.string else return error.BadConfig;
    return out;
}

// ---------------------------------------------------------------- this program's state

const state_head = relay.p2p_head;

const State = struct {
    s: w.store.Store,
    live: Map,
    cursor: Map,
    /// The beacons standing: served topic `tm_<txid>` → when its beacon was asked (ms).
    beacons: Map,
    /// The market's subscriptions standing: served topic `tm_<txid>` → when `tm_<txid>-live` was subscribed (ms).
    subscriptions: Map,
    /// The validated set (0.3.1): the topics `tm_<txid>` the owner's `validate` added, in order, the
    /// record's `validated` (inline, so the owner's page reads it with one explorer read of the head).
    /// Kept across a stop; the beacons (and the cron fallback's heartbeat) follow it.
    validated: []const []const u8 = &.{},
    validated_dirty: bool = false,

    fn load(a: Allocator, s: w.store.Store) !State {
        const maps = try w.store.Maps.create(a, s);
        var st: State = .{ .s = s, .live = maps.map(null), .cursor = maps.map(null), .beacons = maps.map(null), .subscriptions = maps.map(null) };
        const c = (try vm.head(a, state_head)) orelse return st;
        const rec = try s.getValue(a, c);
        if (!eql(u8, rec.getText("kind") orelse "", "amm-p2p-state")) return error.BadState;
        const m = rec.get("maps") orelse return error.BadState;
        st.live = maps.map(m.getCid("live"));
        st.cursor = maps.map(m.getCid("cursor"));
        st.beacons = maps.map(m.getCid("beacons"));
        st.subscriptions = maps.map(m.getCid("subscriptions"));
        if (rec.getArray("validated")) |vs| {
            const out = try a.alloc([]const u8, vs.len);
            for (vs, out) |v, *o| o.* = if (v == .text) v.text else return error.BadState;
            st.validated = out;
        }
        return st;
    }

    /// Save and advance the head when anything changed: → the state record's CID, or null.
    fn commit(self: *State, a: Allocator) !?[]const u8 {
        if (!self.live.dirty and !self.cursor.dirty and !self.beacons.dirty and !self.subscriptions.dirty and !self.validated_dirty) return null;
        var es: [4]cbor.Entry = undefined;
        inline for (.{ "live", "cursor", "beacons", "subscriptions" }, 0..) |n, i| {
            const m = &@field(self, n);
            try m.flush();
            es[i] = .{ .key = n, .value = if (m.root) |r| .{ .cid = r } else .null };
        }
        const c = try self.s.putValue(a, .{ .map = try a.dupe(cbor.Entry, &.{
            .{ .key = "kind", .value = .{ .text = "amm-p2p-state" } },
            .{ .key = "maps", .value = .{ .map = try a.dupe(cbor.Entry, &es) } },
            .{ .key = "validated", .value = try textArray(a, self.validated) },
        }) });
        try vm.advance(state_head, c);
        return c;
    }
};

fn intOf(v: ?w.store.MValue) u32 {
    const x = v orelse return 0;
    return if (x == .int and x.int >= 0 and x.int <= std.math.maxInt(u32)) @intCast(x.int) else 0;
}

/// The app's overlay state (`<app>/state`) over the chain app's (`chain/state`, read only), as the
/// step or call sees them (skein-overlay `engine_vm.load`).
fn loadState(a: Allocator, in: Value) !*ov.state.State {
    const st = try a.create(ov.state.State);
    st.* = (try ov.engine_vm.load(a, overlay_in orelse in)).st;
    st.now = @intCast(in.getUint("now") orelse in.getUint("at") orelse 0);
    return st;
}

// ---------------------------------------------------------------- calls

fn call(a: Allocator, in: Value) !void {
    const func = in.getText("fn") orelse return error.BadInput;
    const arg = try vm.callArg(a, in);
    if (eql(u8, func, "call") and arg.get("match") != null) return vm.answer(a, try appRoute(a, in, arg));
    if (eql(u8, func, "serve")) return vm.answer(a, try servePages(a, arg));
    if (isAppFn(func)) return vm.answer(a, try appCall(a, in, func, arg));
    const out: Value = if (eql(u8, func, "validateLive"))
        try validateLive(a, in, arg)
    else if (eql(u8, func, "proofsByBlock"))
        try proofsByBlock(a, in, arg)
    else if (eql(u8, func, "live"))
        // A route request (the front door's call, `GET /amm/live`) or the consumer API's `{identityKey, threshold?}`.
        (if (arg.getText("method") != null) try liveHttp(a, arg, in.getUint("now") orelse 0) else try liveOf(a, arg, in.getUint("now") orelse 0))
    else
        return error.UnknownFunction;
    try vm.answer(a, out);
}

/// A heartbeat's verdict, with the entry an accept admits. Any error on our side is `ignore` (never penalise what we could not evaluate).
fn validateLive(a: Allocator, in: Value, arg: Value) !Value {
    const msg = try libp2p.inbound(arg);
    const v = liveVerdict(a, in, msg) catch |e| libp2p.Verdict{ .ignore = @errorName(e) };
    return libp2p.answer(a, v);
}

/// A beat on `tm_<txid>-live`, delivered by the market role's subscription (`market`; shruggr/skein#119,
/// David 2026-10-06: a host serving a market keeps the validator map; one that does not, subscribes nothing).
fn liveVerdict(a: Allocator, in: Value, msg: libp2p.Inbound) !libp2p.Verdict {
    const topic = msg.topic orelse return error.NoTopic;
    const t = names.parse(topic) orelse return error.UnknownTopic;
    if (t.kind != .live) return error.WrongTopic;
    const cfg = try config(a, in);
    return switch (liveness.judge(a, topic, msg.body, msg.from, in.getUint("now") orelse 0, cfg.offline_s * 1000)) {
        .accept => |b| .{ .accept = try a.dupe(libp2p.Admit, &.{.{ .box = names.own_box, .event = try liveness.liveEvent(a, b) }}) },
        .reject => |why| .{ .reject = why },
        .ignore => |why| .{ .ignore = why },
    };
}

/// The serving side of the direct call: one request frame, one reply frame. A utility since 0.2.0:
/// no row routes `/amm/proofs/1.0.0` (sync is shruggr/skein#112's `want`).
fn proofsByBlock(a: Allocator, in: Value, arg: Value) !Value {
    const msg = try libp2p.inbound(arg);
    return libp2p.directAnswer(a, try proofs.serve(a, views.held(try loadState(a, in)), msg.body));
}

fn identityArg(v: ?Value) ![33]u8 {
    const x = v orelse return error.BadArgs;
    var out: [33]u8 = undefined;
    switch (x) {
        .bytes => |b| if (b.len == 33) @memcpy(&out, b) else return error.BadArgs,
        .text => |t| _ = std.fmt.hexToBytes(&out, t) catch return error.BadArgs,
        else => return error.BadArgs,
    }
    return out;
}

/// The consumer API: `live(identityKey, now, threshold)` over the last-seen map.
fn liveOf(a: Allocator, arg: Value, now: u64) !Value {
    const id = try identityArg(arg.get("identityKey"));
    var st = try State.load(a, vm.store());
    const threshold = arg.getUint("threshold") orelse liveness.default_offline_s * 1000;
    const s = (try liveness.live(&st.live, id, now, threshold)) orelse return .null;
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "peerId", .value = .{ .bytes = s.peer_id } },
        .{ .key = "peerIdText", .value = .{ .text = try libp2p.peerIdText(a, s.peer_id) } },
        .{ .key = "at", .value = .{ .uint = s.at } },
    }) };
}

/// The last-seen map as a route handler's answer (docs/MESSAGES.md "Route
/// handlers"): `GET /amm/live[?identityKey=<hex>][&threshold=<ms>]` →
/// `{now, thresholdMs, validators: [{identityKey, peerId, at, ageMs, live}]}`,
/// every identity this node has recorded a heartbeat from (or the one asked),
/// `live` when its last heartbeat is within the threshold (default the
/// offline threshold). A read: nothing is written.
fn liveHttp(a: Allocator, req: Value, now: u64) !Value {
    if (!eql(u8, req.getText("method") orelse "", "GET")) return httpJson(a, 405, .{ .status = "error", .message = "GET only" });
    const threshold: u64 = if (try queryParam(a, req, "threshold")) |t|
        (std.fmt.parseInt(u64, t, 10) catch return httpJson(a, 400, .{ .status = "error", .message = "threshold: milliseconds" }))
    else
        liveness.default_offline_s * 1000;
    const want: ?[33]u8 = if (try queryParam(a, req, "identityKey")) |h|
        (identityArg(.{ .text = h }) catch return httpJson(a, 400, .{ .status = "error", .message = "identityKey: 33 bytes, hex" }))
    else
        null;
    const Row = struct { identityKey: []const u8, peerId: []const u8, at: u64, ageMs: u64, live: bool };
    var rows: std.ArrayList(Row) = .empty;
    var st = try State.load(a, vm.store());
    for (try st.live.prefixed("")) |kv| {
        if (kv.key.len != 33) continue;
        if (want) |k| if (!eql(u8, kv.key, &k)) continue;
        const s = liveness.seenOf(kv.value) orelse continue;
        const age = if (now > s.at) now - s.at else 0;
        try rows.append(a, .{
            .identityKey = try vm.hexAlloc(a, kv.key),
            .peerId = try libp2p.peerIdText(a, s.peer_id),
            .at = s.at,
            .ageMs = age,
            .live = age <= threshold,
        });
    }
    return httpJson(a, 200, .{ .now = now, .thresholdMs = threshold, .validators = rows.items });
}

fn httpJson(a: Allocator, status: u64, v: anytype) !Value {
    var out: std.Io.Writer.Allocating = .init(a);
    try std.json.Stringify.value(v, .{}, &out.writer);
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "status", .value = .{ .uint = status } },
        .{ .key = "type", .value = .{ .text = "application/json" } },
        .{ .key = "body", .value = .{ .bytes = out.written() } },
    }) };
}

/// A query parameter of a route request (`query`, `a=b&c=d`), percent-decoded.
fn queryParam(a: Allocator, req: Value, name: []const u8) !?[]const u8 {
    const q = req.getText("query") orelse return null;
    var it = std.mem.splitScalar(u8, if (std.mem.startsWith(u8, q, "?")) q[1..] else q, '&');
    while (it.next()) |kv| {
        const i = std.mem.indexOfScalar(u8, kv, '=') orelse continue;
        if (!eql(u8, kv[0..i], name)) continue;
        const v = try a.dupe(u8, kv[i + 1 ..]);
        for (v) |*c| if (c.* == '+') {
            c.* = ' ';
        };
        return std.Uri.percentDecodeInPlace(v);
    }
    return null;
}

// ---------------------------------------------------------------- outbound: emit to a provider

/// The key of this host's provider `name` (`libp2p`, `cron`): the address book's entry at (`local`,
/// `name`) (skein-sdk 0.7 `sk.peerAt`; the book has no roles, shruggr/skein#126).
fn providerKey(a: Allocator, name: []const u8) ![]const u8 {
    const k = (try sk.peerAt(a, "local", name)) orelse return error.NoProvider;
    return if (k.len == 33) k else error.NoProvider;
}

/// Emit `body` to `to` in `box` (docs/MESSAGES.md "emit"): → the message's CID. It goes out when the step ends without error.
fn emit(a: Allocator, to: []const u8, box: []const u8, body: Value) ![]const u8 {
    const msg = try cbor.encode(a, .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "to", .value = .{ .bytes = to } },
        .{ .key = "box", .value = .{ .text = box } },
        .{ .key = "body", .value = .{ .bytes = try cbor.encode(a, body) } },
    }) });
    return vm.result(a, vm.sk.emit, .{ msg.ptr, @as(u32, @intCast(msg.len)) });
}

fn toLibp2p(a: Allocator, r: libp2p.Request) ![]const u8 {
    return emit(a, try providerKey(a, "libp2p"), r.box, r.body);
}

fn walletCall(a: Allocator, frame: []const u8) ![]const u8 {
    return vm.result(a, ext.wallet, .{ frame.ptr, @as(u32, @intCast(frame.len)) });
}

/// This instance's libp2p peer ID (David, 2026-10-05): the signer's public key for protocol
/// `[2, "skein instance"]` (skein src/host/signer.ts `INSTANCE_PROTOCOL`), key ID
/// `libp2p:<handle>`, counterparty self, as an identity multihash (libp2p.zig `peerIdOf`).
/// The host derives its node's key the same way, from the instance's root (skein 387e057,
/// src/host/signer.ts `peerKey`: `[2, "skein instance"]` / `libp2p:<handle>` / self), so this
/// is the peer ID the node runs.
fn selfPeerId(a: Allocator, in: Value) ![]const u8 {
    const me = in.get("self") orelse return error.NoSelf;
    const handle = me.getText("handle") orelse return error.NoHandle;
    const key_id = try std.fmt.allocPrint(a, "libp2p:{s}", .{handle});
    const pk = try wire.publicKeyResult(try walletCall(a, try wire.getPublicKeyFrameFor(a, libp2p.instance_protocol.level, libp2p.instance_protocol.name, key_id, .self, null)));
    return libp2p.peerIdOf(a, pk);
}

// ---------------------------------------------------------------- steps

fn step(a: Allocator, in: Value) !void {
    const s = vm.store();
    const args = in.get("args") orelse return error.BadInput;
    // The relay thread (launched by amm.swap.submit or amm.pool.submit).
    if (relay.Kind.ofRelay(args.getText("kind") orelse "")) |kind| return relayStep(a, in, args, kind);
    // A message in the app's box: a call {fn, args} (APPS.md §4).
    if (eql(u8, args.getText("box") orelse "", relay.app_name)) {
        // Stepped again when the relay thread it launched comes to rest: the relay answered the sender.
        if (in.get("resolved") != null) {
            _ = try vm.finish(a, s, try resultRecord(a, "relayed", &.{}));
            return;
        }
        if (args.getCid("body")) |bc| {
            const body = try s.getValue(a, bc);
            if (body.get("fn") != null) return appMessage(a, in, args, body);
        }
    }
    const at: u64 = in.getUint("at") orelse return error.BadInput;
    var st = try State.load(a, s);
    var fields: std.ArrayList(cbor.Entry) = .empty;
    var op: []const u8 = undefined;

    const reply = in.get("reply");
    const resumed = (reply != null and reply.? == .map) or (in.getBool("woke") orelse false);
    if (resumed) {
        // A catch-up pass's thread, stepped by the libp2p provider's answer or its deadline.
        op = "catchup";
        try catchup(a, in, &st, at, &fields);
    } else if (args.getCid("event")) |ec| {
        const ev = try s.getValue(a, ec);
        op = ev.getText("kind") orelse return error.BadEvent;
        if (!eql(u8, op, "amm-live")) return error.BadEvent;
        // An accepted heartbeat (the handler's `admit` entry): re-verified, into the last-seen map.
        try fields.append(a, .{ .key = "changed", .value = .{ .boolean = try liveness.apply(a, &st.live, ev) } });
    } else if (args.getCid("body")) |bc| blk: {
        const body = try s.getValue(a, bc);
        const sender = args.getBytes("sender") orelse return error.BadInput;
        // Validation, per topic (0.3.1): `{fn: "validate" | "unvalidate", args: {topic}}`.
        if (body.getText("fn")) |f| {
            const vop = std.meta.stringToEnum(liveness.ValidationOp, f) orelse return error.BadMessage;
            if (vop != .validate and vop != .unvalidate) return error.BadMessage;
            op = f;
            try validationMessage(a, in, &st, args, body, f, vop, &fields);
            break :blk;
        }
        op = body.getText("kind") orelse return error.BadMessage;
        if (eql(u8, op, schedule.tick_kind)) {
            if (!eql(u8, sender, try providerKey(a, "cron"))) return error.NotTheCronProvider;
            const job = try schedule.jobOf(body);
            try fields.append(a, .{ .key = "job", .value = .{ .text = @tagName(job) } });
            switch (job) {
                .heartbeat => try heartbeat(a, in, &st, &fields),
                .catchup => try catchup(a, in, &st, at, &fields),
            }
        } else if (eql(u8, op, schedule.start_kind) or eql(u8, op, schedule.stop_kind)) {
            const owner = in.getBytes("owner") orelse "";
            if (!eql(u8, sender, owner) and !eql(u8, sender, try providerKey(a, "cron"))) return error.NotTheOwner;
            const cfg = try config(a, in);
            const start = eql(u8, op, schedule.start_kind);
            // The market role: subscribe the served tokens' `-live` beacons (or end them).
            try market(a, in, &st, cfg, start, &fields);
            // The beacon (#126): the host publishes the heartbeat on each validated topic; nothing
            // ticks. A stop ends the beacons and keeps the validated set; a start beacons it again.
            // A start naming `jobs` asks the cron provider instead (the fallback, below).
            if (body.getArray("jobs") == null) {
                try applyValidation(a, in, &st, cfg, if (start) .start else .stop, null, &fields);
                break :blk;
            }
            const cron = try providerKey(a, "cron");
            const jobs = try schedule.jobsOf(a, body);
            const out = try a.alloc(Value, jobs.len);
            for (jobs, out) |j, *o| {
                const req = if (eql(u8, op, schedule.start_kind))
                    try schedule.tick(a, j, 1000 * switch (j) {
                        .heartbeat => cfg.heartbeat_s,
                        .catchup => cfg.catchup_s,
                    }, names.own_box)
                else
                    try schedule.stop(a, j);
                // The answer ({replyTo, name, next} or {replyTo, error}) is recorded in box `cron`; nothing awaits it.
                _ = try emit(a, cron, "cron", req);
                o.* = .{ .text = schedule.name(j) };
            }
            try fields.append(a, .{ .key = "schedules", .value = .{ .array = out } });
        } else return error.BadMessage;
    } else return error.BadInput;

    if (try st.commit(a)) |c| try fields.append(a, .{ .key = "state", .value = .{ .cid = c } });
    var es: std.ArrayList(cbor.Entry) = .empty;
    try es.appendSlice(a, &.{
        .{ .key = "kind", .value = .{ .text = "amm-p2p-result" } },
        .{ .key = "op", .value = .{ .text = op } },
    });
    try es.appendSlice(a, fields.items);
    _ = try vm.finish(a, s, .{ .map = es.items });
}

fn identityKey(a: Allocator) ![33]u8 {
    return wire.publicKeyResult(try walletCall(a, try wire.identityKeyFrame(a)));
}

/// The heartbeat body (opldotdev/amm-poc#3): this instance's identity and peer ID. The beat's
/// time and signature are the host's frame (skein docs/MESSAGES.md "Beacons"; liveness.zig).
fn heartbeatBody(a: Allocator, in: Value) ![]const u8 {
    return liveness.encodeBody(a, .{ .identity_key = try identityKey(a), .peer_id = try selfPeerId(a, in) });
}

/// The cron fallback's heartbeat (a tick): the frame the host's beacon would publish — the body,
/// the tick's time, signed here through the signer (the same key and protocol the host uses) — on
/// each validated topic's `-live` topic, through the libp2p provider (its answer is not awaited).
fn heartbeat(a: Allocator, in: Value, st: *State, fields: *std.ArrayList(cbor.Entry)) !void {
    const at = in.getUint("at") orelse return error.BadInput;
    const validated = st.validated;
    if (validated.len == 0) return fields.append(a, .{ .key = "published", .value = .{ .uint = 0 } });
    const body = try heartbeatBody(a, in);
    const sender = try identityKey(a);
    for (validated) |t| {
        const topic = try names.live(a, t);
        const d = try liveness.digest(a, topic, body, at, sender);
        const sig = try wire.signatureResult(try walletCall(a, try wire.createSignatureFrame(a, liveness.protocol.security_level, liveness.protocol.name, liveness.key_id, .anyone, d)));
        const frame = try liveness.encodeFrame(a, .{ .body = body, .at = at, .sender = sender, .signature = sig });
        _ = try toLibp2p(a, try libp2p.publish(a, topic, frame));
    }
    try fields.append(a, .{ .key = "published", .value = .{ .uint = validated.len } });
}

/// Validation and the beacon (0.3.1; shruggr/skein#126, docs/MESSAGES.md "emit"). Beaconing is not
/// a role (David 2026-10-06): a node beacons `tm_<txid>-live` for the topics it validates. The
/// owner sets that up per topic (`validate` / `unvalidate`, like a topic's registration); the
/// record's `validated` is the set, the map `beacons` the beacons standing; `liveness.validation`
/// says what an op changes. A `beacon` event is `{event: "beacon", topic: "tm_<txid>-live", every:
/// heartbeatSeconds × 1000, body}` — the host's libp2p node publishes `body` on it every `every`
/// ms, logging nothing per beat; an `unbeacon` ends it. A stop ends every beacon and keeps the
/// set; a start beacons the set again. Each beat is the host's frame `{body, at, sender,
/// signature}` (liveness.zig): fresh and signed per beat.
fn applyValidation(a: Allocator, in: Value, st: *State, cfg: Config, op: liveness.ValidationOp, topic: ?[]const u8, fields: *std.ArrayList(cbor.Entry)) !void {
    const at: u64 = in.getUint("at") orelse return error.BadInput;
    var standing: std.ArrayList([]const u8) = .empty;
    for (try st.beacons.prefixed("")) |kv| try standing.append(a, try a.dupe(u8, kv.key));
    const v = try liveness.validation(a, op, st.validated, standing.items, topic);
    if (v.validated.len != st.validated.len) {
        st.validated = v.validated;
        st.validated_dirty = true;
    }
    for (v.unbeacon) |t| {
        _ = try vm.emitEvent(a, try liveness.unbeaconEvent(a, try names.live(a, t)));
        _ = try st.beacons.remove(t);
    }
    if (v.beacon.len > 0) {
        const body = try heartbeatBody(a, in);
        for (v.beacon) |t| {
            _ = try vm.emitEvent(a, try liveness.beaconEvent(a, try names.live(a, t), cfg.heartbeat_s * 1000, body));
            try st.beacons.put(t, .{ .int = @intCast(at) });
        }
    }
    try fields.appendSlice(a, &.{
        .{ .key = "validated", .value = try textArray(a, v.validated) },
        .{ .key = "beacons", .value = try textArray(a, v.beacon) },
        .{ .key = "unbeacons", .value = try textArray(a, v.unbeacon) },
    });
}

fn contains(xs: []const []const u8, x: []const u8) bool {
    for (xs) |y| if (eql(u8, x, y)) return true;
    return false;
}

fn textArray(a: Allocator, xs: []const []const u8) !Value {
    const out = try a.alloc(Value, xs.len);
    for (xs, out) |x, *o| o.* = .{ .text = x };
    return .{ .array = out };
}

/// `{fn: "validate" | "unvalidate", args: {topic: "tm_<txid>"}}` in box `amm/validate` (0.3.2): the
/// topic into (or out of) the validated set, its beacon asked (or ended); idempotent. The row
/// `{"address": "validate", "sender": "$owner", "program": "amm-p2p"}` is the permission: taken in
/// that box only (`names.mayValidate`; elsewhere the step errors `NotTakenHere`), with no sender
/// check here. The set is also the validator's: amm-validator signs for no topic outside it.
/// Answered to the sender (box `amm`, when the address book reaches it) with `{topic, validating}`.
fn validationMessage(a: Allocator, in: Value, st: *State, args: Value, body: Value, func: []const u8, op: liveness.ValidationOp, fields: *std.ArrayList(cbor.Entry)) !void {
    const sender = args.getBytes("sender") orelse return error.BadInput;
    if (!names.mayValidate(args.getText("box"))) return error.NotTakenHere;
    const call_args = body.get("args") orelse return error.BadMessage;
    const topic = call_args.getText("topic") orelse return error.BadMessage;
    const n = names.parse(topic) orelse return error.BadTopic;
    if (n.kind != .overlay) return error.BadTopic;
    try applyValidation(a, in, st, try config(a, in), op, topic, fields);
    const result: Value = .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "topic", .value = .{ .text = topic } },
        .{ .key = "validating", .value = .{ .boolean = op == .validate } },
    }) };
    const answer = try relay.answerMessage(a, func, args.getCid("message") orelse return error.BadInput, result);
    try fields.appendSlice(a, &.{
        .{ .key = "answer", .value = answer },
        .{ .key = "sent", .value = .{ .boolean = try answerSender(a, sender, answer) } },
    });
}

/// The market role (`ammP2p.market`, shruggr/skein#120, David 2026-10-06): a start subscribes
/// `tm_<txid>-live` for every served token topic not yet subscribed — `{event: "subscribe", topic,
/// program: "amm-p2p", fn: "validateLive"}` (shruggr/skein#119): the kernel delivers each beat there,
/// and an accepted one is stepped into the map `live` — and unsubscribes a topic subscribed before
/// and no longer served (deregistered meanwhile); a stop, or a start with the role off, unsubscribes
/// every standing one. The standing set is the map `subscriptions`. amm-p2p is not stepped by a
/// registration (the owner's `register` goes to the engine): the set is reconciled at each start.
fn market(a: Allocator, in: Value, st: *State, cfg: Config, start: bool, fields: *std.ArrayList(cbor.Entry)) !void {
    const at: u64 = in.getUint("at") orelse return error.BadInput;
    var standing: std.ArrayList([]const u8) = .empty;
    for (try st.subscriptions.prefixed("")) |kv| try standing.append(a, try a.dupe(u8, kv.key));
    const p = try liveness.plan(a, standing.items, if (start and cfg.market) cfg.topics else &.{});
    const subscribed = try a.alloc(Value, p.subscribe.len);
    for (p.subscribe, subscribed) |t, *o| {
        _ = try vm.emitEvent(a, try liveness.subscribeEvent(a, try names.live(a, t)));
        try st.subscriptions.put(t, .{ .int = @intCast(at) });
        o.* = .{ .text = t };
    }
    const unsubscribed = try a.alloc(Value, p.unsubscribe.len);
    for (p.unsubscribe, unsubscribed) |t, *o| {
        _ = try vm.emitEvent(a, try liveness.unsubscribeEvent(a, try names.live(a, t)));
        _ = try st.subscriptions.remove(t);
        o.* = .{ .text = t };
    }
    try fields.appendSlice(a, &.{
        .{ .key = "market", .value = .{ .boolean = cfg.market } },
        .{ .key = "subscribed", .value = .{ .array = subscribed } },
        .{ .key = "unsubscribed", .value = .{ .array = unsubscribed } },
    });
}

/// The `/` route (`{transport: "http", address: "/", prefix: true, program: "amm-p2p", fn: "serve",
/// root: "www", index: "index.html"}`): the app's pages from its own tree, the installed app
/// record's `tree` (skein-sdk `files.serve`, shruggr/skein#125).
fn servePages(a: Allocator, arg: Value) !Value {
    const req = try toSdk(a, arg);
    const m = try app.manifestOf(a, relay.app_name);
    return fromSdk(a, try files.serve(a, req, scbor.Value.cidOf(m.get("tree")), files.rowOptions(req)));
}

// ---------------------------------------------------------------- catch-up (the pull half, placeholder)

/// The peers to ask, as `dial` takes them: configured ones, then validators heard from recently.
fn catchupPeers(a: Allocator, cfg: Config, st: *State, now: u64) ![]const []const u8 {
    var out: std.ArrayList([]const u8) = .empty;
    try out.appendSlice(a, cfg.peers);
    for (try st.live.prefixed("")) |kv| {
        if (kv.key.len != 33) continue;
        const s = (try liveness.live(&st.live, kv.key[0..33].*, now, cfg.offline_s * 1000)) orelse continue;
        try out.append(a, try libp2p.peerIdText(a, s.peer_id));
    }
    return out.items;
}

/// Where a catch-up pass is: the topic (index into the configured topics),
/// the block being asked for and the last of the pass's batch, the peers and
/// which one is being asked, the dial message whose answers the thread
/// awaits (the libp2p provider answers a dial with `{stream}`, then each
/// frame read, all naming the dial), the direct call once open, and the
/// deadline. Carried from step to step in the thread's result record
/// (`catchup`).
const Pass = struct {
    topic: u32,
    height: u32,
    to: u32,
    peers: []const []const u8,
    peer: u32 = 0,
    dial: ?[]const u8 = null,
    stream: ?u64 = null,
    until: u64 = 0,

    fn value(p: Pass, a: Allocator) !Value {
        const ps = try a.alloc(Value, p.peers.len);
        for (p.peers, ps) |x, *o| o.* = .{ .text = x };
        return .{ .map = try a.dupe(cbor.Entry, &.{
            .{ .key = "topic", .value = .{ .uint = p.topic } },
            .{ .key = "height", .value = .{ .uint = p.height } },
            .{ .key = "to", .value = .{ .uint = p.to } },
            .{ .key = "peers", .value = .{ .array = ps } },
            .{ .key = "peer", .value = .{ .uint = p.peer } },
            .{ .key = "dial", .value = if (p.dial) |c| .{ .cid = c } else .null },
            .{ .key = "stream", .value = if (p.stream) |x| .{ .uint = x } else .null },
            .{ .key = "until", .value = .{ .uint = p.until } },
        }) };
    }

    fn of(a: Allocator, v: Value) !Pass {
        const pv = v.getArray("peers") orelse return error.BadPass;
        const ps = try a.alloc([]const u8, pv.len);
        for (pv, ps) |x, *o| o.* = if (x == .text) x.text else return error.BadPass;
        return .{
            .topic = @intCast(v.getUint("topic") orelse return error.BadPass),
            .height = @intCast(v.getUint("height") orelse return error.BadPass),
            .to = @intCast(v.getUint("to") orelse return error.BadPass),
            .peers = ps,
            .peer = @intCast(v.getUint("peer") orelse 0),
            .dial = v.getCid("dial"),
            .stream = v.getUint("stream"),
            .until = v.getUint("until") orelse 0,
        };
    }

    fn nextPeer(p: *Pass) void {
        p.peer += 1;
        p.dial = null;
        p.stream = null;
    }

    fn nextBlock(p: *Pass) void {
        p.height += 1;
        p.peer = 0;
        p.dial = null;
        p.stream = null;
    }
};

/// The pass the thread's last step left, from its result record (the
/// update's stdout is the record's CID in hex, vm.finish).
fn resumedPass(a: Allocator, in: Value) !?Pass {
    const s = vm.store();
    const tip = in.getCid("tip") orelse return null;
    const u = try s.getValue(a, tip);
    const res = u.get("result") orelse return null;
    const out = std.mem.trim(u8, res.getBytes("stdout") orelse return null, " \n");
    if (out.len == 0 or out.len % 2 != 0) return null;
    const cid = try a.alloc(u8, out.len / 2);
    _ = std.fmt.hexToBytes(cid, out) catch return null;
    const rec = s.getValue(a, cid) catch return null;
    return Pass.of(a, rec.get("catchup") orelse return null) catch null;
}

/// Rest the thread on the dial's answers until the pass's deadline.
fn rest(a: Allocator, p: Pass, fields: *std.ArrayList(cbor.Entry)) !void {
    try vm.awaitRecord(p.dial.?);
    try vm.deadline(@intCast(p.until));
    try fields.append(a, .{ .key = "catchup", .value = try p.value(a) });
}

/// One catch-up step. Started by a tick (no pass yet), or stepped again by
/// the libp2p provider's answer to the pass's dial (`{stream}`, a frame,
/// its end, an error) or by the deadline: take what came, then ask on, one
/// block and one peer at a time, while the topics have unsettled
/// transactions; rest on the next dial, or end the pass.
fn catchup(a: Allocator, in: Value, st: *State, at: u64, fields: *std.ArrayList(cbor.Entry)) !void {
    const cfg = try config(a, in);
    const st0 = try loadState(a, in);
    st0.now = @intCast(at);
    const ch = st0.ch;
    var proven: u64 = 0;
    var settled: u64 = 0;
    defer fields.append(a, .{ .key = "proven", .value = .{ .uint = settled + proven } }) catch {};
    var cur: ?Pass = try resumedPass(a, in);
    var next_topic: u32 = if (cur) |p| p.topic + 1 else 0;

    const reply: ?Value = if (in.get("reply")) |r| (if (r == .map) r else null) else null;
    if (cur) |*p| if (p.dial) |dial| {
        if (reply) |r| {
            const reply_to = r.getCid("replyTo") orelse "";
            const box = r.getText("box") orelse "";
            const body = try vm.store().getValue(a, r.getCid("body") orelse return error.BadInput);
            if (!eql(u8, reply_to, dial)) {
                // Not this dial's answer (a late one): rest again.
                if (at < p.until) return rest(a, p.*, fields);
                p.nextPeer();
            } else switch (libp2p.dialAnswer(box, body) catch libp2p.DialAnswer{ .closed = "BadAnswer" }) {
                .opened => |sid| {
                    // The direct call is open: send the request, rest on its reply frame.
                    p.stream = sid;
                    if (p.topic >= cfg.topics.len) return error.TopicsChanged;
                    const hdr = (try ch.chain().at(p.height)) orelse return error.NoHeader;
                    _ = try toLibp2p(a, try libp2p.send(a, sid, try proofs.encodeRequest(a, .{ .block_hash = hdr.hash, .topic = cfg.topics[p.topic] })));
                    if (at < p.until) return rest(a, p.*, fields);
                    _ = try toLibp2p(a, try libp2p.close(a, sid));
                    p.nextPeer();
                },
                .frame => |frame| {
                    if (p.stream) |sid| _ = try toLibp2p(a, try libp2p.close(a, sid));
                    if (try takeReply(a, st0, p.height, frame)) |k| {
                        proven += k;
                        p.nextBlock();
                    } else p.nextPeer();
                },
                .closed => p.nextPeer(),
            }
        } else {
            // The deadline: no answer in time.
            if (p.stream) |sid| _ = try toLibp2p(a, try libp2p.close(a, sid));
            p.nextPeer();
        }
    };

    const tip = (try ch.chain().tip()) orelse return finishCatchup(proven);
    while (true) {
        if (cur == null) {
            // The next topic with a plan; a topic with nothing unsettled has its cursor follow the tip.
            while (next_topic < cfg.topics.len) : (next_topic += 1) {
                const topic = cfg.topics[next_topic];
                const ck = try w.store.nameKey(a, topic, &.{});
                const cursor: ?u32 = if (try st.cursor.get(ck)) |v| intOf(v) else null;
                const plan = proofs.catchupPlan(cursor, tip, cfg.window, cfg.batch, try unsettled(st0, topic)) orelse continue;
                if (plan.from > plan.to) {
                    try st.cursor.put(ck, .{ .int = plan.cursor });
                    continue;
                }
                cur = .{ .topic = next_topic, .height = plan.from, .to = plan.to, .peers = try catchupPeers(a, cfg, st, at) };
                next_topic += 1;
                break;
            }
            if (cur == null) break;
        }
        const p = &cur.?;
        if (p.topic >= cfg.topics.len) {
            cur = null;
            continue;
        }
        if (p.height > p.to) {
            try st.cursor.put(try w.store.nameKey(a, cfg.topics[p.topic], &.{}), .{ .int = p.to });
            cur = null;
            continue;
        }
        if (p.peer >= @min(p.peers.len, 3)) {
            p.nextBlock();
            continue;
        }
        if ((try ch.chain().at(p.height)) == null) {
            p.nextBlock();
            continue;
        }
        // Dial the peer on the proofs protocol and rest on the provider's answers.
        try finishCatchup(proven);
        settled += proven;
        proven = 0;
        p.dial = try toLibp2p(a, try libp2p.dial(a, p.peers[p.peer], names.proofs_protocol));
        p.stream = null;
        p.until = at + cfg.reply_timeout_ms;
        return rest(a, p.*, fields);
    }
    return finishCatchup(proven);
}

/// Settle what the pass proved. STOP (shruggr/skein#120, skein-amm docs/AMM.md "Not built"): in
/// amm-poc the pass applied each proof to the chain core under the head `wallet` (the transitional
/// `wallet` grant), called the `rejected` hooks and advanced that head here. Since skein #79 the
/// chain state is the chain app's alone (`chain/state`); a program of this app writes only `amm/…`
/// and has no way to record a proof it fetched. Nothing is recorded; `takeReply` stops first.
fn finishCatchup(proven: u64) !void {
    if (proven == 0) return;
    return error.CatchupCannotRecordProofs;
}

/// Whether the topic has transactions not yet proven (the chain state's `unproven` index joined to
/// the topic's `applied`, the overlay's).
fn unsettled(st: *ov.state.State, topic: []const u8) !bool {
    for (try st.ch.map("unproven").prefixed("")) |kv| {
        if (kv.key.len == 32 and try st.isApplied(topic, kv.key[0..32].*)) return true;
    }
    return false;
}

/// A proofs-by-block reply for the block at `height`: checked against the
/// header → how many held unproven transactions it proves, or null when the
/// reply is missing or does not check (ask the next peer). STOP: recording
/// them is not possible here (`finishCatchup`): a reply that proves any is
/// an error of the pass, `CatchupCannotRecordProofs`.
fn takeReply(a: Allocator, st: *ov.state.State, height: u32, reply: []const u8) !?u64 {
    const bump = (proofs.decodeReply(a, reply) catch return null) orelse return null;
    const root = switch (try proofs.rootOf(a, bump)) {
        .root => |r| r,
        .bad => return null,
    };
    const hdr = (try st.ch.chain().at(height)) orelse return null;
    if (!eql(u8, &root, &(try w.header.Header.parse(&hdr.raw)).merkle_root)) return null;
    const p = w.merkle.MerklePath.parse(a, bump) catch return null;
    if (p.block_height != height) return null;
    var n: u64 = 0;
    for (try proofs.leaves(a, p)) |t| {
        if ((try views.status(st, t)) != .unproven) continue;
        n += 1;
    }
    if (n > 0) return error.CatchupCannotRecordProofs;
    return n;
}

// ---------------------------------------------------------------- the app: amm.swap/1 (the marketplace relay)

/// The SDK's dag-cbor and the wallet library's are the same encoding; values cross by bytes.
fn toSdk(a: Allocator, v: Value) !scbor.Value {
    return scbor.decode(a, try cbor.encode(a, v));
}

fn fromSdk(a: Allocator, v: scbor.Value) !Value {
    return cbor.decode(a, try scbor.encode(a, v));
}

/// The installed app record (the head `amm/app`'s root, `kind: "app"`), or null.
fn appRecord(a: Allocator) !?Value {
    const c = (try vm.head(a, relay.app_head)) orelse return null;
    const v = try vm.store().getValue(a, c);
    return if (eql(u8, v.getText("kind") orelse "", "app")) v else null;
}

/// The app's head as this program keeps it: the installed app record and its
/// `state`, or (no app record: an instance wired by its genesis) the root as
/// the state record itself.
const AppHead = struct {
    record: ?Value,
    state: ?Value,

    fn load(a: Allocator) !AppHead {
        const c = (try vm.head(a, relay.app_head)) orelse return .{ .record = null, .state = null };
        const s = vm.store();
        const v = try s.getValue(a, c);
        if (eql(u8, v.getText("kind") orelse "", "app")) {
            const st = if (v.getCid("state")) |sc| try s.getValue(a, sc) else null;
            return .{ .record = v, .state = st };
        }
        if (eql(u8, v.getText("kind") orelse "", relay.state_kind)) return .{ .record = null, .state = v };
        return error.NotTheAppsHead;
    }

    /// Keep the book's state under the head: the app record with `state` replaced (APPS.md §1), or the state as the root.
    fn save(h: *AppHead, a: Allocator, book: *relay.Book) !void {
        const s = vm.store();
        const st = try book.state(a);
        const sc = try s.putValue(a, st);
        if (h.record) |rec| {
            var es: std.ArrayList(cbor.Entry) = .empty;
            for (rec.map) |e| if (!eql(u8, e.key, "state")) try es.append(a, e);
            try es.append(a, .{ .key = "state", .value = .{ .cid = sc } });
            const root: Value = .{ .map = es.items };
            try vm.advance(relay.app_head, try s.putValue(a, root));
            h.record = root;
        } else try vm.advance(relay.app_head, sc);
        h.state = st;
    }
};

/// The manifest the dispatch reads `provides` from: the installed app record,
/// else (wired by the genesis) `{kind: "app", name: "amm", provides}` with
/// `provides` from genesis `defaults.ammProvides` (the manifest's, as JSON).
fn manifest(a: Allocator, in: Value) !scbor.Value {
    if (try appRecord(a)) |rec| return toSdk(a, rec);
    const d = in.get("defaults") orelse return sk.report("no app record (head amm/app) and no defaults.ammProvides");
    const text = d.getText("ammProvides") orelse return sk.report("no app record (head amm/app) and no defaults.ammProvides");
    const provides = dagjson.decode(a, text) catch return sk.report("defaults.ammProvides is not JSON");
    var m = scbor.MapBuilder.init(a);
    try m.put("kind", scbor.string("app"));
    try m.put("name", scbor.string(relay.app_name));
    try m.put("provides", provides);
    return m.value();
}

/// How the function was reached, and what it did that the caller's answer depends on.
const Ctx = struct {
    in: Value,
    mode: enum { route, message, call },
    /// A message's request (answered by the relay thread when the record settles).
    request: ?relay.Request = null,
    /// A route call's own program record (launching the relay thread).
    arg: ?Value = null,
    /// The answer waits on a thread (the route answers {wait: true}).
    waiting: bool = false,
    /// The relay thread was launched for this call: a message's answer comes from it.
    launched: bool = false,
};
var ctx: Ctx = undefined;

const fns = [_]app.Function{
    .{ .name = relay.fn_submit, .run = submitFn },
    .{ .name = relay.fn_status, .run = statusFn },
    .{ .name = relay.fn_terms, .run = termsFn },
    .{ .name = relay.fn_pool_submit, .run = poolSubmitFn },
    .{ .name = relay.fn_pool_status, .run = poolStatusFn },
    .{ .name = relay.fn_liquidity_submit, .run = liquiditySubmitFn },
    .{ .name = relay.fn_liquidity_status, .run = liquidityStatusFn },
};

fn isAppFn(name: []const u8) bool {
    for (fns) |f| if (eql(u8, f.name, name)) return true;
    return false;
}

/// A function's failure with the VM import's message when it has one.
fn failure(a: Allocator, e: anyerror) anyerror {
    if (e == error.Reported or e == error.OutOfMemory) return e;
    return sk.report(std.fmt.allocPrint(a, "{s}{s}{s}", .{ @errorName(e), if (vm.lastError().len > 0) ": " else "", vm.lastError() }) catch "failed");
}

fn submitFn(c: *app.Call) anyerror!scbor.Value {
    return submit(c, .swap) catch |e| failure(c.a, e);
}

fn statusFn(c: *app.Call) anyerror!scbor.Value {
    return recordStatus(c, .swap) catch |e| failure(c.a, e);
}

fn poolSubmitFn(c: *app.Call) anyerror!scbor.Value {
    return submit(c, .pool) catch |e| failure(c.a, e);
}

fn poolStatusFn(c: *app.Call) anyerror!scbor.Value {
    return recordStatus(c, .pool) catch |e| failure(c.a, e);
}

fn liquiditySubmitFn(c: *app.Call) anyerror!scbor.Value {
    return submit(c, .liquidity) catch |e| failure(c.a, e);
}

fn liquidityStatusFn(c: *app.Call) anyerror!scbor.Value {
    return recordStatus(c, .liquidity) catch |e| failure(c.a, e);
}

/// amm.swap.terms {} → {commissionPkh: bytes(20) | null}: what the page names
/// as Swap's commissionPkh before it builds the swap.
fn termsFn(c: *app.Call) anyerror!scbor.Value {
    return toSdk(c.a, relay.terms(c.a, commissionConfig(c.a, ctx.in) catch |e| return failure(c.a, e)) catch |e| return failure(c.a, e));
}

/// amm.swap.status / amm.pool.status / amm.liquidity.status {id} → the record (or the error `not_found`).
fn recordStatus(c: *app.Call, kind: relay.Kind) !scbor.Value {
    const a = c.a;
    const h = try AppHead.load(a);
    var book = try relay.Book.load(a, vm.store(), h.state);
    return switch (try relay.statusOf(a, &book, kind, try fromSdk(a, c.args))) {
        .found => |r| toSdk(a, try r.answer(a)),
        .not_found => sk.report("not_found"),
        .bad_id => sk.report("bad_id: 64 hex characters"),
    };
}

/// This program's own record, to launch the relay thread as: a route call's
/// matched entry (an installed route), a step's thread's program, else the
/// genesis's `programs["amm-p2p"]`.
fn selfProgram(a: Allocator, in: Value) ![]const u8 {
    if (ctx.arg) |arg| if (arg.get("match")) |m| if (m.getCid("program")) |p| return p;
    if (in.getCid("thread")) |t| if ((try vm.store().getValue(a, t)).getCid("program")) |p| return p;
    if (in.get("programs")) |ps| if (ps.getCid("amm-p2p")) |p| return p;
    return error.NoProgramRecord;
}

var env_call: *app.Call = undefined;

/// The validator's peer from the last-seen map (its heartbeats), live within the offline threshold.
fn envPeer(_: *anyopaque, a: Allocator, identity: [33]u8) anyerror!?[]const u8 {
    const cfg = try config(a, ctx.in);
    const now: u64 = ctx.in.getUint("now") orelse ctx.in.getUint("at") orelse 0;
    var st = try State.load(a, vm.store());
    const seen = (try liveness.live(&st.live, identity, now, cfg.offline_s * 1000)) orelse return null;
    return try libp2p.peerIdText(a, seen.peer_id);
}

/// A transaction this instance holds (the chain state's: the spent pool's source, when an add's BEEF does not carry it).
fn envHeld(_: *anyopaque, a: Allocator, txid: [32]u8) anyerror!?[]const u8 {
    return (try loadState(a, ctx.in)).ch.txRaw(txid);
}

/// The relay thread: this program on {kind: "amm-swap-relay" | "amm-pool-relay" | "amm-liquidity-relay", id}.
fn envLaunch(_: *anyopaque, a: Allocator, kind: relay.Kind, id: [32]u8) anyerror![]const u8 {
    const targs = try vm.store().putValue(a, .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "kind", .value = .{ .text = kind.relayKind() } },
        .{ .key = "id", .value = .{ .bytes = try a.dupe(u8, &id) } },
    }) });
    return env_call.launch(try selfProgram(a, ctx.in), targs);
}

/// amm.swap.submit {funding, swap, pool, validator, expires} (relay.zig
/// `submit`) and amm.pool.submit {funding, deploy, validator, expires}
/// (relay.zig `submitDeploy`) and amm.liquidity.submit {funding, add, pool,
/// validator, expires} (relay.zig `submitAdd`): check, record `pending` under the app's head,
/// launch the relay thread → the record. The same request again answers its
/// record (a `/call` waits on its relay while it is pending); one that timed
/// out or failed in transport is relayed again.
fn submit(c: *app.Call, kind: relay.Kind) !scbor.Value {
    const a = c.a;
    const in = ctx.in;
    var h = try AppHead.load(a);
    var book = try relay.Book.load(a, vm.store(), h.state);
    env_call = c;
    const env: relay.Env = .{
        .book = &book,
        .now = in.getUint("now") orelse in.getUint("at") orelse 0,
        .commission = try commissionConfig(a, in),
        .request = ctx.request,
        .ctx = &signer_dummy,
        .peerFn = envPeer,
        .launchFn = envLaunch,
        .heldFn = envHeld,
    };
    const args = try fromSdk(a, c.args);
    const submitted = switch (kind) {
        .swap => try relay.submit(a, env, args),
        .pool => try relay.submitDeploy(a, env, args),
        .liquidity => try relay.submitAdd(a, env, args),
    };
    switch (submitted) {
        .refused => |r| return sk.report(try relay.refusalText(a, r)),
        .settled => |rec| return toSdk(a, try rec.answer(a)),
        .pending => |rec| {
            // The relay is under way: a route waits on its thread (at rest already: answered from the record).
            if (ctx.mode == .route) if (rec.thread) |t| {
                if (c.awaitRecord(t)) {
                    ctx.waiting = true;
                } else |_| {}
            };
            return toSdk(a, try rec.answer(a));
        },
        .launched => |rec| {
            try h.save(a, &book);
            ctx.launched = true;
            ctx.waiting = true;
            return toSdk(a, try rec.answer(a));
        },
    }
}

fn dispatch(a: Allocator, in: Value, name: []const u8, args: ?scbor.Value, sender: ?[]const u8) !app.Outcome {
    return app.run(a, try toSdk(a, in), relay.app_name, try manifest(a, in), &fns, name, args, sender);
}

/// An in-VM call of amm.swap.*: the result, or the call's error.
fn appCall(a: Allocator, in: Value, func: []const u8, arg: Value) !Value {
    ctx = .{ .in = in, .mode = .call };
    return switch (try dispatch(a, in, func, try toSdk(a, arg), null)) {
        .ok => |v| fromSdk(a, v),
        .err => |f| {
            std.Io.File.stderr().writeStreamingAll(sk.io(), try std.fmt.allocPrint(a, "{s}: {s}: {s}\n", .{ func, @tagName(f.code), f.message })) catch {};
            return error.CallFailed;
        },
    };
}

fn routeAnswer(a: Allocator, status_code: u16, body: scbor.Value) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "status", .value = .{ .uint = status_code } },
        .{ .key = "type", .value = .{ .text = "application/json" } },
        .{ .key = "body", .value = .{ .bytes = try dagjson.encode(a, body) } },
    }) };
}

fn failureValue(a: Allocator, f: app.Failure) !scbor.Value {
    var e = scbor.MapBuilder.init(a);
    try e.put("code", scbor.string(@tagName(f.code)));
    try e.put("message", scbor.string(f.message));
    return e.value();
}

/// The `/call` route (APPS.md §4; the SDK's `app` route, with one addition:
/// `amm.swap.submit`, `amm.pool.submit` and `amm.liquidity.submit` wait on the relay thread). The body is `{fn, args}`
/// (JSON / dag-json, or dag-cbor as application/cbor); the answer `{fn,
/// result}` (200) or `{fn, error}` (400, 403, 404, 409, 500). A submit that
/// launched (or found) its relay answers `{wait: true}`; called again with
/// `resolved` when the relay thread comes to rest, it answers the record.
fn appRoute(a: Allocator, in: Value, arg: Value) !Value {
    ctx = .{ .in = in, .mode = .route, .arg = arg };
    const caller = arg.getBytes("caller");
    var name: []const u8 = "";
    var call_args: ?scbor.Value = null;
    const outcome: app.Outcome = blk: {
        if (!eql(u8, arg.getText("method") orelse "", "POST")) break :blk .{ .err = .{ .code = .@"bad-request", .message = "POST {fn, args}" } };
        if (!app.admitted(try toSdk(a, in), caller, relay.app_name)) break :blk .{ .err = .{ .code = .@"not-admitted", .message = "the caller is not admitted to box amm" } };
        const raw = arg.getBytes("body") orelse "";
        const ct = arg.getText("contentType") orelse "";
        const body = (if (eql(u8, ct, "application/cbor")) scbor.decode(a, raw) else dagjson.decode(a, raw)) catch
            break :blk .{ .err = .{ .code = .@"bad-request", .message = "the body is not {fn, args} (JSON, or dag-cbor as application/cbor)" } };
        if (body != .map) break :blk .{ .err = .{ .code = .@"bad-request", .message = "the body is not {fn, args}" } };
        name = scbor.Value.str(body.get("fn")) orelse break :blk .{ .err = .{ .code = .@"bad-request", .message = "fn is not text" } };
        call_args = body.get("args");
        if (arg.get("resolved") != null and relay.Kind.ofSubmit(name) != null) {
            const kind = relay.Kind.ofSubmit(name).?;
            // Called again: the relay thread this request waited on has come to rest. The record, as it stands.
            const sargs = try fromSdk(a, call_args orelse .null);
            const id = switch (kind) {
                .swap => relay.idOf(sargs.getBytes("swap") orelse ""),
                .pool => relay.deployId(a, sargs.getBytes("deploy") orelse "") orelse break :blk .{ .err = .{ .code = .failed, .message = "not_found" } },
                .liquidity => relay.addId(a, sargs.getBytes("add") orelse "") orelse break :blk .{ .err = .{ .code = .failed, .message = "not_found" } },
            };
            const h = try AppHead.load(a);
            var book = try relay.Book.load(a, vm.store(), h.state);
            const rec = (try book.getKind(a, kind, id)) orelse break :blk .{ .err = .{ .code = .failed, .message = "not_found" } };
            break :blk .{ .ok = try toSdk(a, try rec.answer(a)) };
        }
        break :blk try dispatch(a, in, name, call_args, caller);
    };
    if (outcome == .ok and ctx.waiting and arg.get("resolved") == null)
        return .{ .map = try a.dupe(cbor.Entry, &.{.{ .key = "wait", .value = .{ .boolean = true } }}) };
    var ans = scbor.MapBuilder.init(a);
    try ans.put("fn", scbor.string(name));
    var code: u16 = 200;
    switch (outcome) {
        .ok => |v| try ans.put("result", v),
        .err => |f| {
            code = f.code.status();
            try ans.put("error", try failureValue(a, f));
        },
    }
    return routeAnswer(a, code, ans.value());
}

/// Whether the address book reaches `key` (an answer goes out only then).
fn reachable(a: Allocator, key: []const u8) !bool {
    const s = vm.store();
    const root = (try vm.head(a, "peers")) orelse return false;
    const book = try s.getValue(a, root);
    for (book.getArray("peers") orelse return false) |e| {
        const p = try s.getValue(a, e.getCid("peer") orelse continue);
        if (p.getBytes("key")) |k| if (eql(u8, k, key)) return true;
    }
    return false;
}

fn resultRecord(a: Allocator, op: []const u8, fields: []const cbor.Entry) !Value {
    var es: std.ArrayList(cbor.Entry) = .empty;
    try es.appendSlice(a, &.{
        .{ .key = "kind", .value = .{ .text = "amm-p2p-result" } },
        .{ .key = "op", .value = .{ .text = op } },
    });
    try es.appendSlice(a, fields);
    return .{ .map = es.items };
}

/// Answer a message's sender in the app's box, when the address book reaches it (APPS.md §4).
fn answerSender(a: Allocator, sender: ?[]const u8, answer: Value) !bool {
    const s = sender orelse return false;
    if (!try reachable(a, s)) return false;
    _ = try emit(a, s, relay.app_name, answer);
    return true;
}

/// A message `{fn, args}` in box `amm`: the call, answered to the sender now
/// — or, for a submit that launched the relay, by the relay thread when the
/// record settles.
fn appMessage(a: Allocator, in: Value, args: Value, body: Value) !void {
    const message = args.getCid("message") orelse return error.BadInput;
    const sender = args.getBytes("sender");
    ctx = .{ .in = in, .mode = .message, .request = if (sender) |sd| .{ .message = message, .sender = sd } else null };
    const sbody = try toSdk(a, body);
    const name = scbor.Value.str(sbody.get("fn"));
    const outcome: app.Outcome = if (name) |n|
        try dispatch(a, in, n, sbody.get("args"), sender)
    else
        .{ .err = .{ .code = .@"bad-request", .message = "fn is not text" } };
    const func = name orelse "";
    if (outcome == .ok and ctx.launched) {
        _ = try vm.finish(a, vm.store(), try resultRecord(a, func, &.{.{ .key = "relaying", .value = .{ .boolean = true } }}));
        return;
    }
    const answer = switch (outcome) {
        .ok => |v| try relay.answerMessage(a, func, message, try fromSdk(a, v)),
        .err => |f| try relay.errorMessage(a, func, message, @tagName(f.code), f.message),
    };
    const sent = try answerSender(a, sender, answer);
    _ = try vm.finish(a, vm.store(), try resultRecord(a, func, &.{
        .{ .key = "answer", .value = answer },
        .{ .key = "sent", .value = .{ .boolean = sent } },
    }));
}

// ---------------------------------------------------------------- the relay thread

var signer_dummy: u8 = 0;

fn signerIdentity(_: *anyopaque, a: Allocator) anyerror![33]u8 {
    return wire.publicKeyResult(try walletCall(a, try wire.identityKeyFrame(a)));
}

fn signerSign(_: *anyopaque, a: Allocator, hash: [32]u8) anyerror![]const u8 {
    return wire.signatureResult(try walletCall(a, try wire.createSignatureFrame(a, 2, relay.envelope_protocol, relay.envelope_key_id, .anyone, hash)));
}

/// This instance's identity, signing the package through the `wallet` import (BRC-169's message signature).
fn signer() relay.Signer {
    return .{ .ctx = &signer_dummy, .identityFn = signerIdentity, .signFn = signerSign };
}

/// One step of the relay thread {kind: "amm-swap-relay" | "amm-pool-relay",
/// id}: launched, or stepped again by the libp2p provider's answer to its
/// dial or by the deadline at `expires` (relay.zig `advance`).
fn relayStep(a: Allocator, in: Value, args: Value, kind: relay.Kind) !void {
    const s = vm.store();
    const idb = args.getBytes("id") orelse return error.BadInput;
    if (idb.len != 32) return error.BadInput;
    const id = idb[0..32].*;
    const now: u64 = in.getUint("at") orelse in.getUint("now") orelse return error.BadInput;
    var h = try AppHead.load(a);
    var book = try relay.Book.load(a, s, h.state);
    var rec = (try book.getKind(a, kind, id)) orelse {
        _ = try vm.finish(a, s, try resultRecord(a, "relay", &.{.{ .key = "missing", .value = .{ .boolean = true } }}));
        return;
    };

    var input: relay.Input = .start;
    var pkg: ?[]const u8 = null;
    if (in.get("reply")) |r| if (r == .map) {
        const reply_to = r.getCid("replyTo") orelse "";
        if (rec.dial == null or !eql(u8, reply_to, rec.dial.?)) {
            // Not this dial's answer (a late one): rest again.
            if (rec.dial) |d| try vm.awaitRecord(d);
            try vm.deadline(@intCast(rec.expires));
            _ = try vm.finish(a, s, try resultRecord(a, "relay", &.{.{ .key = "late", .value = .{ .boolean = true } }}));
            return;
        }
        const body = try s.getValue(a, r.getCid("body") orelse return error.BadInput);
        input = .{ .answer = libp2p.dialAnswer(r.getText("box") orelse "", body) catch .{ .closed = "BadAnswer" } };
        if (input.answer == .opened and rec.status == .pending) pkg = try relay.packageFor(a, signer(), rec);
    };
    if (input == .start and (in.getBool("woke") orelse false)) input = .woke;

    const fx = try relay.advance(a, &rec, input, now, pkg);
    const lp = try providerKey(a, "libp2p");
    for (fx.emits, 0..) |req, i| {
        const m = try emit(a, lp, req.box, req.body);
        if (fx.dial) |di| if (di == i) {
            rec.dial = m;
        };
    }
    try book.put(a, rec);
    try h.save(a, &book);
    if (fx.rest) {
        try vm.awaitRecord(rec.dial orelse return error.NoDial);
        try vm.deadline(@intCast(rec.expires));
    }
    var sent = false;
    if (fx.done) if (rec.request) |q| {
        sent = try answerSender(a, q.sender, try relay.answerMessage(a, kind.submitFn(), q.message, try rec.answer(a)));
    };
    _ = try vm.finish(a, s, try resultRecord(a, "relay", &.{
        .{ .key = "relay", .value = .{ .text = kind.relayKind() } },
        .{ .key = "id", .value = .{ .text = try a.dupe(u8, &relay.idText(id)) } },
        .{ .key = "status", .value = .{ .text = @tagName(rec.status) } },
        .{ .key = "answered", .value = .{ .boolean = sent } },
    }));
}
