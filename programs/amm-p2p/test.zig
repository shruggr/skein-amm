//! amm-p2p natively: the proofs-by-block direct call (its serving side), the names, the direct
//! call's shapes, amm.swap/1 terms, the affirmation, the commission key, the reads — over an
//! in-memory store (0.44.0: no relay; 0.44.1: no catch-up pass). The VM wiring (main.zig) is wasm32-wasi only.
const std = @import("std");
const w = @import("chain");
const wire = @import("wallet").wire;
const names = @import("src/names.zig");
const stream = @import("src/stream.zig");
const proofs = @import("src/proofs.zig");
/// js-libp2p's peer ID of the generator point's key (skein src/host/p2p.ts `peerIdOf` of the private key 1).
const PEER_ID_OF_G = "16Uiu2HAm3cuhhRL2msUuLF62KRSfneFDx94RsuouyW25Ho42cFMq";
const views = @import("src/views.zig");

const bsvz = w.bsvz;
const cbor = w.cbor;
const Value = cbor.Value;
const Allocator = std.mem.Allocator;
const testing = std.testing;
const eql = std.mem.eql;

comptime {
    _ = &views.held;
}

fn expectTag(o: anytype, comptime tag: []const u8, reason: ?[]const u8) !void {
    if (!std.mem.eql(u8, @tagName(o), tag)) {
        std.debug.print("expected {s}, got {any}\n", .{ tag, o });
        return error.TestUnexpectedResult;
    }
    if (comptime std.mem.eql(u8, tag, "accept")) return;
    if (reason) |r| {
        const got = @field(o, tag);
        if (!std.mem.eql(u8, got, r)) {
            std.debug.print("expected {s} {s}, got {s}\n", .{ tag, r, got });
            return error.TestUnexpectedResult;
        }
    }
}

/// A value as the host stores it and a program reads it back (dag-cbor).
fn readBack(a: Allocator, v: Value) Value {
    return cbor.decode(a, cbor.encode(a, v) catch @panic("OOM")) catch @panic("bad cbor");
}

// ================================================================ proofs by block

fn parent(l: [32]u8, r: [32]u8) [32]u8 {
    return w.store.dblSha256(&(l ++ r));
}

const PE = std.meta.Elem(std.meta.Elem(@FieldType(w.merkle.MerklePath, "path")));

/// The BUMP of a four-transaction block (t0..t3) for the leaves `which`.
fn bump4(a: Allocator, height: u32, t: [4][32]u8, which: []const usize) ![]const u8 {
    var merged: ?w.merkle.MerklePath = null;
    for (which) |i| {
        const sib = i ^ 1;
        const l0 = try a.alloc(PE, 2);
        const me: PE = .{ .offset = i, .hash = .{ .bytes = t[i] }, .txid = true };
        const other: PE = .{ .offset = sib, .hash = .{ .bytes = t[sib] } };
        l0[0] = if (i < sib) me else other;
        l0[1] = if (i < sib) other else me;
        const up = if (i < 2) parent(t[2], t[3]) else parent(t[0], t[1]);
        const l1 = try a.dupe(PE, &.{.{ .offset = (i >> 1) ^ 1, .hash = .{ .bytes = up } }});
        const levels = try a.dupe([]PE, &.{ l0, l1 });
        const p: w.merkle.MerklePath = .{ .block_height = height, .path = levels };
        if (merged) |*m| try m.combine(&p, a) else merged = p;
    }
    return merged.?.bytes(a);
}

const FakeChain = struct {
    hash: [32]u8,
    root: [32]u8,
    height: u32,
    topic: std.AutoHashMap([32]u8, void),
    paths: std.AutoHashMap([32]u8, w.merkle.MerklePath),

    fn self(ctx: *anyopaque) *FakeChain {
        return @ptrCast(@alignCast(ctx));
    }
    fn heightOf(ctx: *anyopaque, h: [32]u8) anyerror!?u32 {
        return if (std.mem.eql(u8, &h, &self(ctx).hash)) self(ctx).height else null;
    }
    fn inTopic(ctx: *anyopaque, _: []const u8, t: [32]u8) anyerror!bool {
        return self(ctx).topic.contains(t);
    }
    fn provenAt(ctx: *anyopaque, a: Allocator, h: u32) anyerror![]const [32]u8 {
        if (h != self(ctx).height) return &.{};
        var out: std.ArrayList([32]u8) = .empty;
        var it = self(ctx).paths.keyIterator();
        while (it.next()) |k| try out.append(a, k.*);
        std.mem.sort([32]u8, out.items, {}, struct {
            fn lt(_: void, x: [32]u8, y: [32]u8) bool {
                return std.mem.order(u8, &x, &y) == .lt;
            }
        }.lt);
        return out.items;
    }
    fn proofFor(ctx: *anyopaque, _: Allocator, t: [32]u8) anyerror!?w.merkle.MerklePath {
        return self(ctx).paths.get(t);
    }
    fn held(f: *FakeChain) proofs.Held {
        return .{ .ctx = f, .heightOfFn = heightOf, .provenAtFn = provenAt, .inTopicFn = inTopic, .proofForFn = proofFor };
    }
};

test "proofs by block: the direct call's request and reply; one BUMP per block" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const t: [4][32]u8 = .{ .{1} ** 32, .{2} ** 32, .{3} ** 32, .{4} ** 32 };
    const root = parent(parent(t[0], t[1]), parent(t[2], t[3]));
    var f: FakeChain = .{ .hash = .{0xbb} ** 32, .root = root, .height = 100, .topic = .init(a), .paths = .init(a) };
    for ([_]usize{ 1, 2 }) |i| try f.paths.put(t[i], try w.merkle.MerklePath.parse(a, try bump4(a, 100, t, &.{i})));
    try f.topic.put(t[1], {});

    // The whole block (no topic): both paths merged into one BUMP proving both.
    const reply = try proofs.serve(a, f.held(), try proofs.encodeRequest(a, .{ .block_hash = f.hash }));
    const b = (try proofs.decodeReply(a, reply)).?;
    const p = try w.merkle.MerklePath.parse(a, b);
    for ([_]usize{ 1, 2 }) |i| try testing.expectEqualSlices(u8, &root, &(w.beef.rootFor(a, p, t[i]).?));
    try testing.expectEqualSlices(u8, &root, &(switch (try proofs.rootOf(a, b)) {
        .root => |r| r,
        .bad => return error.TestUnexpectedResult,
    }));

    // Restricted to the topic: only t1.
    const rt = (try proofs.decodeReply(a, try proofs.serve(a, f.held(), try proofs.encodeRequest(a, .{ .block_hash = f.hash, .topic = "tm_color_" ++ "ab" ** 32 ++ "_0" })))).?;
    const pt = try w.merkle.MerklePath.parse(a, rt);
    try testing.expect(w.beef.bumpHas(pt, t[1]));
    try testing.expect(!w.beef.bumpHas(pt, t[3]));

    // An unknown block, or a malformed request: missing.
    try testing.expect((try proofs.decodeReply(a, try proofs.serve(a, f.held(), try proofs.encodeRequest(a, .{ .block_hash = .{0xcc} ** 32 })))) == null);
    try testing.expect((try proofs.decodeReply(a, try proofs.serve(a, f.held(), "junk"))) == null);
}

test "names: tm_color_<txid>_0 and tm_color_<txid>_0-live (BRC-207), parsed here" {
    const o = "tm_color_" ++ "ab" ** 32 ++ "_0";
    const t = names.parse(o).?;
    try testing.expectEqual(names.Kind.overlay, t.kind);
    try testing.expectEqualSlices(u8, &([_]u8{0xab} ** 32), &t.id);
    const l = names.parse(o ++ "-live").?;
    try testing.expectEqual(names.Kind.live, l.kind);
    try testing.expectEqualStrings(o, l.overlay);
    var buf: [96]u8 = undefined;
    var fba = std.heap.FixedBufferAllocator.init(&buf);
    try testing.expectEqualStrings(o ++ "-live", try names.live(fba.allocator(), o));
    // Not ours: the engine's own suffixes, the old names, upper case, a suffix, a short id.
    try testing.expect(names.parse(o ++ "-proof") == null);
    try testing.expect(names.parse(o ++ "-admit") == null);
    try testing.expect(names.parse("tm_amm_" ++ "ab" ** 32) == null);
    try testing.expect(names.parse(o ++ "_live") == null);
    try testing.expect(names.parse("tm_color_" ++ "AB" ** 32 ++ "_0") == null);
    try testing.expect(names.parse(o ++ "_0") == null);
    // The old `tm_<txid>_0` is no topic (skein-1color 0.9.0: no alias), nor its `-live`; nor the
    // discovery topic, nor the bare `tm_<txid>`.
    try testing.expect(names.parse("tm_" ++ "ab" ** 32 ++ "_0") == null);
    try testing.expect(names.parse("tm_" ++ "ab" ** 32 ++ "_0-live") == null);
    try testing.expect(names.parse("tm_color") == null);
    try testing.expect(names.parse("tm_color_" ++ "ab" ** 32) == null);
    try testing.expect(names.parse("tm_" ++ "ab" ** 32) == null);
    try testing.expect(names.parse("tm_" ++ "ab" ** 32 ++ "-live") == null);
    try testing.expect(names.parse("tm_abcd-live") == null);
    // The names before BRC-207's wire names (skein-1color 0.10.0, 2026-10-21): no alias.
    try testing.expect(names.parse("tm_mandala_" ++ "ab" ** 32 ++ "_0") == null);
    try testing.expect(names.parse("tm_mandala_" ++ "ab" ** 32 ++ "_0-live") == null);
    try testing.expectEqualStrings("/amm/proofs/1.0.0", names.proofs_protocol);
}

test "the peer ID of a compressed secp256k1 key: the identity multihash of its protobuf PublicKey (js-libp2p's, 16Uiu2…)" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var k: [33]u8 = undefined;
    _ = try std.fmt.hexToBytes(&k, "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798");
    const id = try stream.peerIdOf(a, k);
    try testing.expectEqual(@as(usize, 39), id.len);
    try testing.expectEqualSlices(u8, &.{ 0x00, 0x25, 0x08, 0x02, 0x12, 0x21 }, id[0..6]);
    try testing.expectEqualSlices(u8, &k, id[6..]);
    try testing.expectEqualStrings(PEER_ID_OF_G, try stream.peerIdText(a, id));
}

test "the direct call's shapes (stream.zig, 0.44.0): the handler's argument, the direct call's answer (0.44.1: no provider bodies)" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const pid = "\x00\x25\x08\x02\x12\x21\x02" ++ "\x55" ** 32;
    // What the front door passes a route's handler.
    const arg: Value = .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "transport", .value = .{ .text = "libp2p" } },
        .{ .key = "topic", .value = .{ .text = "tm_x-live" } },
        .{ .key = "from", .value = .{ .bytes = pid } },
        .{ .key = "key", .value = .{ .bytes = pid[6..] } },
        .{ .key = "seqno", .value = .{ .bytes = "\x00\x00\x00\x00\x00\x00\x00\x07" } },
        .{ .key = "signature", .value = .{ .bytes = "sig" } },
        .{ .key = "body", .value = .{ .bytes = "body" } },
    }) };
    const in = try stream.inbound(arg);
    try testing.expectEqualSlices(u8, pid, in.from);
    try testing.expectEqualStrings("tm_x-live", in.topic.?);
    try testing.expectError(error.NotLibp2p, stream.inbound(.{ .map = &.{} }));

    const da = readBack(a, try stream.directAnswer(a, "reply"));
    try testing.expectEqualSlices(u8, "reply", da.getBytes("body").?);

    // The peer ID as text (base58btc), as amm.pool.terms answers it.
    try testing.expect((try stream.peerIdText(a, pid)).len > 0);
}

// ================================================================ amm.swap/1 (0.44.0: terms only; the host relays)

const iface = @import("src/interface.zig");
const app = @import("app");
const sk = @import("sk");
const scbor = @import("sdk_cbor");
const dagjson = @import("dagjson");
const message = @import("message");
const manifest_json = @import("app_manifest").json;
const color = @import("1color");
const pool_lib = @import("pool");
const vec = @import("vectors");
const Transaction = bsvz.transaction.Transaction;
const Script = bsvz.script.Script;

fn unhex(a: Allocator, h: []const u8) []u8 {
    const out = a.alloc(u8, h.len / 2) catch @panic("OOM");
    _ = std.fmt.hexToBytes(out, h) catch @panic("bad hex");
    return out;
}

fn p2pkhScript(a: Allocator, pkh: []const u8) ![]u8 {
    return std.mem.concat(a, u8, &.{ &.{ 0x76, 0xa9, 0x14 }, pkh, &.{ 0x88, 0xac } });
}

fn pushData(a: Allocator, data: []const u8) ![]u8 {
    if (data.len == 0) return a.dupe(u8, &.{0});
    if (data.len <= 0x4b) return std.mem.concat(a, u8, &.{ &.{@intCast(data.len)}, data });
    if (data.len <= 0xff) return std.mem.concat(a, u8, &.{ &.{ 0x4c, @intCast(data.len) }, data });
    var n: [2]u8 = undefined;
    std.mem.writeInt(u16, &n, @intCast(data.len), .little);
    return std.mem.concat(a, u8, &.{ &.{0x4d}, &n, data });
}

/// A minimal script number push (Rúnar's): OP_0, OP_1..OP_16, else little-endian with a sign byte when needed.
fn pushNum(a: Allocator, n: u64) ![]u8 {
    if (n == 0) return a.dupe(u8, &.{0});
    if (n <= 16) return a.dupe(u8, &.{@intCast(0x50 + n)});
    var b: [9]u8 = undefined;
    var len: usize = 0;
    var x = n;
    while (x > 0) : (x >>= 8) {
        b[len] = @intCast(x & 0xff);
        len += 1;
    }
    if (b[len - 1] & 0x80 != 0) {
        b[len] = 0;
        len += 1;
    }
    return pushData(a, b[0..len]);
}

fn hash160(b: []const u8) [20]u8 {
    return bsvz.crypto.hash.hash160(b).bytes;
}

/// Sign input `i` (P2PKH, ALL|FORKID) as go-sdk's p2pkh.Unlock does: `<DER ‖ 0x41> <pubkey>`.
fn signP2pkh(a: Allocator, tx: *Transaction, i: usize, src: []const u8, sats: u64, priv: [32]u8) !void {
    const k = try bsvz.primitives.ec.PrivateKey.fromBytes(priv);
    const pre = try bsvz.transaction.sighash.formatPreimage(a, tx, i, Script.init(src), @intCast(sats), 0x41);
    const sig = try k.signDigest(bsvz.crypto.hash.hash256(pre).bytes);
    const pubkey = (try k.publicKey()).toCompressedSec1();
    const ins = try a.dupe(bsvz.transaction.Input, tx.inputs);
    ins[i].unlocking_script = Script.init(try std.mem.concat(a, u8, &.{ try pushData(a, try std.mem.concat(a, u8, &.{ sig.asSlice(), &.{0x41} })), try pushData(a, &pubkey) }));
    tx.inputs = ins;
}

/// A pair as the page builds it (docs/notes.md 2026-10-02, "Swap funding and
/// signing"), derived from the fixtures: the funding transaction spends the
/// taker's fund:1 and pays one exact output (the swap's input: amount in +
/// miner fee) to the taker, plus change; the swap is `swap_bsv_in` with its
/// other input replaced by that output, no change (`_changeAmount = 0`), its
/// commission (output 4, the pool's 10 bps of 20,000: 20 sats) and the call's
/// commissionPkh both paying `commission` (default: the relay's), the
/// preimage the pool call pushes recomputed, the validator's slot empty, and
/// the funding input signed by the taker. `fee` is the swap's miner fee (a
/// different fee makes a different swap). The fixtures carry no funding
/// parent of this shape (their swap spends fund:1 directly), so it is built
/// here.
// --- the dispatch, by the app's manifest (app/etc/app.json), with the functions over memory ---

const TestEnv = struct {
    commission: iface.Commission = ours,
    market: iface.market.Market = .{},
    seen_writes: ?bool = null,
};
var test_env: TestEnv = undefined;

/// This exchange's identity in the tests (0.49.0: the terms answer the identity each leg's
/// commission key derives from): the fixtures' validator identity.
const our_identity: [33]u8 = blk: {
    @setEvalBranchQuota(10_000);
    var k: [33]u8 = undefined;
    _ = std.fmt.hexToBytes(&k, vec.identity) catch unreachable;
    break :blk k;
};
const ours: iface.Commission = .{ .identity = our_identity };

fn fromSdk(a: Allocator, v: scbor.Value) !Value {
    return cbor.decode(a, try scbor.encode(a, v));
}
fn testTerms(c: *app.Call) anyerror!scbor.Value {
    test_env.seen_writes = c.writes;
    return toSdkV(c.a, try iface.terms(c.a, test_env.commission, test_env.market));
}
fn toSdkV(a: Allocator, v: Value) !scbor.Value {
    return scbor.decode(a, try cbor.encode(a, v));
}
const test_fns = [_]app.Function{
    .{ .name = iface.fn_terms, .run = testTerms },
};

fn expectErr(o: app.Outcome, code: app.Code, msg_prefix: []const u8) !void {
    switch (o) {
        .ok => return error.TestExpectedError,
        .err => |f| {
            if (f.code != code or !std.mem.startsWith(u8, f.message, msg_prefix)) std.debug.print("expected {s} {s}, got {s} {s}\n", .{ @tagName(code), msg_prefix, @tagName(f.code), f.message });
            try testing.expectEqual(code, f.code);
            try testing.expect(std.mem.startsWith(u8, f.message, msg_prefix));
        },
    }
}

test "amm.swap/1 by the manifest (0.44.0, David Case 2026-11-06: the host relays, the instance logs facts, never attempts): terms only — no submit, no status; no /swaps read, no `swaps` filter, no `expires` anywhere in the manifest" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    test_env = .{};
    const m = try dagjson.decode(a, manifest_json);
    const in: scbor.Value = .{ .map = &.{} };
    try testing.expect((try app.declOf(a, m, "amm.swap.submit")) == null);
    try testing.expect((try app.declOf(a, m, "amm.swap.status")) == null);
    try expectErr(try app.run(a, in, iface.app_name, m, &test_fns, "amm.swap.submit", .{ .map = &.{} }, null), .@"unknown-fn", "amm.swap.submit");
    try testing.expect(m.get("filters").?.get("swaps") == null);
    for (m.get("routes").?.array) |r| try testing.expect(!eql(u8, scbor.Value.str(r.get("address")) orelse "", "/swaps"));
    try testing.expect(std.mem.indexOf(u8, manifest_json, "\"expires\"") == null);
    try testing.expect(std.mem.indexOf(u8, manifest_json, "amm-p2p.swaps") == null);

    // terms: this exchange's identity (0.49.0: the commission key is per swap, derived from it), read-only as declared; none, null; no args taken.
    try testing.expect(!(try app.declOf(a, m, iface.fn_terms)).?.get("writes").?.bool);
    const t1 = try app.run(a, in, iface.app_name, m, &test_fns, iface.fn_terms, .{ .map = &.{} }, null);
    try testing.expect(t1 == .ok);
    try testing.expectEqual(@as(?bool, false), test_env.seen_writes);
    try testing.expectEqualSlices(u8, &our_identity, (try fromSdk(a, t1.ok)).getBytes("identityKey").?);
    try testing.expect((try fromSdk(a, t1.ok)).get("commissionPkh") == null);
    test_env.commission = .{};
    const t2 = try app.run(a, in, iface.app_name, m, &test_fns, iface.fn_terms, null, null);
    try testing.expect(t2 == .ok);
    try testing.expect((try fromSdk(a, t2.ok)).get("identityKey").? == .null);
    test_env.commission = ours;
    // 0.16.0: the market's minimum beside it, 0 when none is set; the record's when set.
    try testing.expectEqual(@as(u64, 0), (try fromSdk(a, t1.ok)).getUint("minCommissionBps").?);
    test_env.market = .{ .min_commission_bps = 1 };
    const t3 = try app.run(a, in, iface.app_name, m, &test_fns, iface.fn_terms, null, null);
    try testing.expectEqual(@as(u64, 1), (try fromSdk(a, t3.ok)).getUint("minCommissionBps").?);
    test_env.market = .{};
    var targs = scbor.MapBuilder.init(a);
    try targs.put("pool", scbor.string("00" ** 32 ++ "_0"));
    try expectErr(try app.run(a, in, iface.app_name, m, &test_fns, iface.fn_terms, targs.value(), null), .@"bad-args", "args.pool: not in the shape");
}

test "the app's root head is <app>/app (skein-sdk 0.3.0 app.headOf)" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    try std.testing.expectEqualStrings(try app.headOf(arena.allocator(), iface.app_name), iface.app_head);
}

// ================================================================ the affirmation (amm.pool/1, 0.11.0)

test "affirm (0.13.0): the args {pool, satoshis} become the validator's body; its answer becomes {validatorPubKey, nonce, signature}, or the refusal; the manifest declares amm.pool.affirm (no writes) and no amm.pool.submit or status" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const pd = try Transaction.parse(a, unhex(a, vec.pool_deploy_unaffirmed));
    const script = pd.outputs[0].locking_script.bytes;
    const args: Value = .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "pool", .value = .{ .bytes = script } },
        .{ .key = "satoshis", .value = .{ .uint = vec.pool0.bsv } },
    }) };
    const body = (try iface.affirmBody(a, args)).?;
    try testing.expectEqualSlices(u8, script, body.getBytes("pool").?);
    try testing.expectEqual(@as(u64, vec.pool0.bsv), body.getUint("satoshis").?);
    try testing.expect(body.get("keyInput") == null);
    // Missing args: nothing to ask.
    try testing.expect((try iface.affirmBody(a, .{ .map = args.map[0..1] })) == null);
    // The package's nonce is the body's.
    try testing.expectEqualSlices(u8, &try iface.affirmNonce(a, body), &try iface.affirmNonce(a, body));
    const other = (try iface.affirmBody(a, .{ .map = try a.dupe(cbor.Entry, &.{ args.map[0], .{ .key = "satoshis", .value = .{ .uint = 1 } } }) })).?;
    try testing.expect(!std.mem.eql(u8, &try iface.affirmNonce(a, body), &try iface.affirmNonce(a, other)));

    // The validator's answer: {verdict, body: {ok: true, validatorPubKey, nonce, signature}} →
    // {validatorPubKey, nonce, signature}.
    const sig = unhex(a, vec.affirmation);
    const key = unhex(a, vec.pool0.validator);
    const nonce = unhex(a, vec.nonce);
    const frame = try cbor.encode(a, .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "ok", .value = .{ .boolean = true } },
        .{ .key = "validatorPubKey", .value = .{ .bytes = key } },
        .{ .key = "nonce", .value = .{ .bytes = nonce } },
        .{ .key = "signature", .value = .{ .bytes = sig } },
    }) });
    const ans: Value = .{ .map = try a.dupe(cbor.Entry, &.{ .{ .key = "verdict", .value = .{ .text = "accept" } }, .{ .key = "body", .value = .{ .bytes = frame } } }) };
    const got = iface.affirmAnswer(a, ans);
    try testing.expectEqualSlices(u8, sig, got.ok.getBytes("signature").?);
    try testing.expectEqualSlices(u8, key, got.ok.getBytes("validatorPubKey").?);
    try testing.expectEqualSlices(u8, nonce, got.ok.getBytes("nonce").?);
    try testing.expectEqual(@as(usize, 3), got.ok.map.len);
    // Affirmed without the key or the nonce: a bad reply.
    const short = try cbor.encode(a, .{ .map = try a.dupe(cbor.Entry, &.{ .{ .key = "ok", .value = .{ .boolean = true } }, .{ .key = "signature", .value = .{ .bytes = sig } } }) });
    try testing.expectEqualStrings("bad_reply", iface.affirmAnswer(a, .{ .map = try a.dupe(cbor.Entry, &.{.{ .key = "body", .value = .{ .bytes = short } }}) }).refused.reason);
    // A refusal: its reason and detail, as the function's error text.
    const nframe = try cbor.encode(a, .{ .map = try a.dupe(cbor.Entry, &.{ .{ .key = "ok", .value = .{ .boolean = false } }, .{ .key = "reason", .value = .{ .text = "fees_unacceptable" } }, .{ .key = "detail", .value = .{ .text = "lpFeeBps: the terms are 30" } } }) });
    const no = iface.affirmAnswer(a, .{ .map = try a.dupe(cbor.Entry, &.{.{ .key = "body", .value = .{ .bytes = nframe } }}) });
    try testing.expectEqualStrings("fees_unacceptable: lpFeeBps: the terms are 30", try iface.refusalText(a, no.refused));
    try testing.expectEqualStrings("bad_reply", iface.affirmAnswer(a, .{ .map = &.{} }).refused.reason);

    // The manifest: amm.pool.affirm, no writes, its args; amm.pool.submit and status gone.
    const m = try dagjson.decode(a, manifest_json);
    const decl = (try app.declOf(a, m, iface.fn_pool_affirm)).?;
    try testing.expect(!decl.get("writes").?.bool);
    const dargs = decl.get("args").?;
    for ([_][]const u8{ "pool", "satoshis" }) |k| try testing.expect(dargs.get(k) != null);
    try testing.expect(dargs.get("keyInput") == null);
    for ([_][]const u8{ "validatorPubKey", "nonce", "signature" }) |k| try testing.expect(decl.get("answer").?.get(k) != null);
    try testing.expect((try app.declOf(a, m, "amm.pool.submit")) == null);
    try testing.expect((try app.declOf(a, m, "amm.pool.status")) == null);
    try testing.expect((try app.declOf(a, m, iface.fn_pool_terms)) != null);
}

test "market and validator are the engine's (0.6.0, shruggr/skein#120), root's switch (0.6.2, David 2026-10-07): no validate route, no start or stop, no config.amm.ammP2p.market; config.overlay.market / .validator absent, both off until root turns one on" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const m = try dagjson.decode(a, manifest_json);
    for (m.get("routes").?.array) |row| {
        try testing.expect(!std.mem.eql(u8, scbor.Value.str(row.get("address")) orelse "", "validate"));
    }
    try testing.expect(m.get("start") == null and m.get("stop") == null);
    const config = m.get("config").?;
    if (if (config.get("amm")) |c| c.get("ammP2p") else null) |p2p| {
        try testing.expect(p2p.get("market") == null and p2p.get("heartbeatSeconds") == null and p2p.get("offlineSeconds") == null);
    }
    // "this shouldn't have been a config in the manifest. This should be a setting that the user is
    // configuring": root turns a role on at install (--config) or by the engine's switch.
    const ov_cfg = config.get("overlay").?;
    try testing.expect(ov_cfg.get("market") == null and ov_cfg.get("validator") == null);
}

test "the routes, filters and roles (shruggr/skein#143, 0.7.0; 0.17.0 the token app): no dispatch, reads or senders; the box amm is the engine's (derived), amm-p2p's box amm/amm-p2p; no register, no pages, no requests; the reads read routes" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const m = try dagjson.decode(a, manifest_json);
    try testing.expect(m.get("dispatch") == null and m.get("reads") == null);
    const gated = m.get("roles").?.get("root").?.array;
    // 0.17.0 (shruggr/skein-amm#4): no register, registerLookup, deregisterLookup — a token instance's
    // topic is its config's at birth; rescind, the validator's (0.13.0); terms, the validator's (0.15.0:
    // the runtime record); market, the market's (0.16.0: the runtime record).
    // 0.49.0: no reindex (skein-overlay 0.25.0 has none), no lookupVersion; no rescind (the
    // wallet-funded Rescind is gone from the validator).
    try testing.expectEqual(@as(usize, 2), gated.len);
    for ([_][]const u8{ "terms", "market" }, gated) |x, g| try testing.expectEqualStrings(x, scbor.Value.str(g).?);
    try testing.expect(m.get("lookupVersion") == null);
    const filters = m.get("filters").?;
    try testing.expect(filters.get("page") == null and filters.get("requests") == null);
    try testing.expectEqualStrings("color-lookup.tokens", scbor.Value.str(filters.get("tokens")).?);
    try testing.expectEqualStrings("amm-p2p.spends", scbor.Value.str(filters.get("spends")).?);
    var seen: u8 = 0;
    var seen_tokens = false;
    for (m.get("routes").?.array) |r| {
        try testing.expect(r.get("sender") == null and r.get("program") == null);
        const t = scbor.Value.str(r.get("transport")) orelse "mailbox";
        const addr = scbor.Value.str(r.get("address")).?;
        const h = scbor.Value.str(r.get("handler"));
        // No route of the app's own on its box `amm` ("" or "amm"): the engine's, derived.
        if (std.mem.eql(u8, t, "mailbox")) try testing.expect(addr.len > 0 and !std.mem.eql(u8, addr, "amm"));
        if (std.mem.eql(u8, t, "mailbox") and std.mem.eql(u8, addr, "amm-p2p")) {
            try testing.expectEqualStrings("amm-p2p", h.?);
            seen |= 1;
        }
        // 0.17.0: no register box, no pages, no holders' requests (the config app's).
        try testing.expect(!(std.mem.eql(u8, t, "mailbox") and (std.mem.eql(u8, addr, "register") or std.mem.eql(u8, addr, "requests"))));
        try testing.expect(!(std.mem.eql(u8, t, "http") and (std.mem.eql(u8, addr, "/") or std.mem.eql(u8, addr, "/requests"))));
        if (std.mem.eql(u8, t, "http") and std.mem.eql(u8, addr, "/call")) {
            try testing.expectEqualStrings("amm-p2p.call", h.?);
            try testing.expectEqualStrings("kernel.brc104", scbor.Value.str(r.get("filters").?.array[0]).?);
            seen |= 2;
        }
        // 0.49.0: no reindex row; no libp2p receive row (skein-overlay 0.25.1, D10: the host submits a fetched BEEF as a submit message).
        try testing.expect(!std.mem.eql(u8, addr, "reindex"));
        try testing.expect(!std.mem.eql(u8, addr, "/skein/overlay/beef/1.0.0"));
        // 0.9.0: the validator's box (root's rescind, 0.13.0) and the read /spends.
        if (std.mem.eql(u8, t, "mailbox") and std.mem.eql(u8, addr, "validator")) {
            try testing.expectEqualStrings("amm-validator", h.?);
            seen |= 4;
        }
        if (std.mem.eql(u8, t, "http") and std.mem.eql(u8, addr, "/spends")) {
            try testing.expect(h == null);
            seen |= 8;
        }
        // No addLiquidity (0.9.0), no delivered deploy (0.11.0).
        try testing.expect(!std.mem.eql(u8, addr, "/amm-validator/1/addLiquidity"));
        try testing.expect(!std.mem.eql(u8, addr, "/amm-validator/1/deploy"));
        // The token list is /color/tokens (0.10.0, skein-1color 0.10.0); no /mandala/tokens.
        if (std.mem.eql(u8, t, "http") and std.mem.eql(u8, addr, "/color/tokens")) {
            try testing.expectEqualStrings("tokens", scbor.Value.str(r.get("filters").?.array[0]).?);
            seen_tokens = true;
        }
        try testing.expect(!std.mem.eql(u8, addr, "/mandala/tokens"));
    }
    try testing.expectEqual(@as(u8, 15), seen);
    try testing.expect(seen_tokens);
    try testing.expectEqualStrings("amm/amm-p2p", names.own_box);
}

// ================================================================ the reads (0.9.0)

const reads = @import("src/reads.zig");

test "spends (0.9.0; 0.47.0 one lookup): an outpoint's spender, no pool following — {outpoint, spentBy?}" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var ms = w.store.MemStore.init(testing.allocator);
    defer ms.deinit();
    var ch = try w.state.State.load(a, ms.store(), null, .regtest);
    for ([_][]const u8{ vec.fund, vec.token_deploy, vec.pool_deploy, vec.swap_bsv_in, vec.swap_tokens_in }) |h| {
        const raw = unhex(a, h);
        _ = try ch.putTx(w.beef.txidOf(raw), raw);
    }
    const pd = w.beef.txidOf(unhex(a, vec.pool_deploy));
    // The deploy's pool output: its spender only (the first Swap), not the pool's current output.
    const at_deploy = try reads.spendOf(&ch, .{ .txid = pd, .vout = 0 });
    try testing.expectEqualSlices(u8, &w.beef.txidOf(unhex(a, vec.swap_bsv_in)), &at_deploy.spent_by.?);
    const json = try reads.spendJson(a, at_deploy);
    try testing.expectEqualStrings(try std.fmt.allocPrint(a, "{{\"outpoint\":\"{s}.0\",\"spentBy\":\"{s}\"}}", .{ &w.header.toHex(pd), &w.header.toHex(w.beef.txidOf(unhex(a, vec.swap_bsv_in))) }), json);
    // An output nobody spent (the LP's token change): no spentBy.
    const change = try reads.spendOf(&ch, .{ .txid = pd, .vout = 2 });
    try testing.expect(change.spent_by == null);
    try testing.expectEqualStrings(try std.fmt.allocPrint(a, "{{\"outpoint\":\"{s}.2\"}}", .{&w.header.toHex(pd)}), try reads.spendJson(a, change));
    // The query.
    const q = reads.queryOutpoint("outpoint=" ++ "ab" ** 32 ++ ".4").?;
    try testing.expectEqual(@as(u32, 4), q.vout);
    try testing.expect(reads.queryOutpoint("x=1") == null);
}

test "amm.pool.terms (0.15.0): the record root set, with this validator's identity and peer; none set, no answer (\"no validator here\"); no fee numbers in the manifest" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const terms = @import("terms");
    const id: [33]u8 = .{2} ++ .{0x11} ** 32;
    const rec = try terms.record(a, .{ .lp_fee_bps = 30, .validator_fee_bps = 5, .commission_bps = 25 });
    const ans = (try iface.poolTerms(a, &id, PEER_ID_OF_G, terms.fromRecord(rec))).?;
    try testing.expectEqualSlices(u8, &id, ans.getBytes("validator").?);
    try testing.expectEqualStrings(PEER_ID_OF_G, ans.getText("peerId").?);
    try testing.expectEqual(@as(u64, 30), ans.getUint("lpFeeBps").?);
    try testing.expectEqual(@as(u64, 5), ans.getUint("validatorFeeBps").?);
    try testing.expectEqual(@as(u64, 25), ans.getUint("commissionBps").?);
    // No peer known: no peerId.
    try testing.expect((try iface.poolTerms(a, &id, null, terms.fromRecord(rec))).?.get("peerId") == null);
    // Unset (no head) or cleared (the record null): no answer; main.zig reports "no validator here".
    try testing.expect((try iface.poolTerms(a, &id, PEER_ID_OF_G, null)) == null);
    try testing.expect((try iface.poolTerms(a, &id, PEER_ID_OF_G, terms.fromRecord(.null))) == null);
    try testing.expectEqualStrings("no validator here", terms.no_validator);
    // The manifest ships no terms: config.amm has no ammValidator; amm.pool.terms is still declared.
    const m = try dagjson.decode(a, manifest_json);
    try testing.expect(m.get("config").?.get("amm") == null); // 0.16.0: no config.amm at all (commission gone too)
    try testing.expect((try app.declOf(a, m, iface.fn_pool_terms)) != null);
}

test "market terms (0.16.0, David 2026-10-24): a runtime record root sets — fn market gated by roles.root in the box amm/amm-p2p, the head amm/market; set, clear, refused shapes; unset 0; no config.amm.commission" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const market = iface.market;

    // The manifest: `market` is root's; the box amm-p2p routes to this program; no commission config.
    const m = try dagjson.decode(a, manifest_json);
    const gated = for (m.get("roles").?.get("root").?.array) |g| {
        if (eql(u8, scbor.Value.str(g).?, market.fn_market)) break true;
    } else false;
    try testing.expect(gated);
    try testing.expect(m.get("config").?.get("amm") == null);
    try testing.expectEqualStrings("amm/amm-p2p", names.own_box);
    try testing.expectEqualStrings("amm/market", try market.headName(a, iface.app_name));

    // Set: {minCommissionBps}, an integer 0 to 10 000.
    const set: Value = .{ .map = &.{.{ .key = "minCommissionBps", .value = .{ .uint = 25 } }} };
    const p = market.parseArgs(set);
    try testing.expect(p == .set);
    try testing.expectEqual(@as(u64, 25), p.set.min_commission_bps);
    try testing.expect(market.parseArgs(.{ .map = &.{.{ .key = "minCommissionBps", .value = .{ .uint = 10_000 } }} }) == .set);
    // Clear: args null, or none.
    try testing.expect(market.parseArgs(.null) == .clear);
    try testing.expect(market.parseArgs(null) == .clear);
    // Refused: over 10 000, negative, text, another key, missing, not a map.
    const bads = [_]Value{
        .{ .map = &.{.{ .key = "minCommissionBps", .value = .{ .uint = 10_001 } }} },
        .{ .map = &.{.{ .key = "minCommissionBps", .value = .{ .nint = 0 } }} },
        .{ .map = &.{.{ .key = "minCommissionBps", .value = .{ .text = "1" } }} },
        .{ .map = &.{ .{ .key = "minCommissionBps", .value = .{ .uint = 1 } }, .{ .key = "tokenId", .value = .{ .text = "x" } } } },
        .{ .map = &.{} },
        .{ .uint = 1 },
    };
    for (bads) |b| try testing.expect(market.parseArgs(b) == .bad);

    // The record round trip; cleared (null), no head or a bad record: 0.
    const rec = try market.record(a, p.set);
    try testing.expectEqualSlices(u8, try cbor.encode(a, set), try cbor.encode(a, rec));
    try testing.expectEqual(@as(u64, 25), market.fromRecord(rec).min_commission_bps);
    try testing.expectEqual(@as(u64, 0), market.fromRecord(.null).min_commission_bps);
    try testing.expectEqual(@as(u64, 0), market.fromRecord(null).min_commission_bps);

}

test "the commission key (0.49.0, per swap): BRC-43 level 1 `amm commission`, keyed by the spent pool outpoint, counterparty anyone — the instance's own signer key for it is the one anyone derives from the identity (src/pool.zig `commissionKey`), the client's pinned vector" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const kd_mod = bsvz.primitives.key_deriver;
    try testing.expectEqual(@as(u8, 1), iface.commission_protocol.level);
    try testing.expectEqualStrings("amm commission", iface.commission_protocol.name);
    const proto: kd_mod.Protocol = .{ .security_level = iface.commission_protocol.level, .name = iface.commission_protocol.name };
    const root = try bsvz.primitives.ec.PrivateKey.fromBytes(.{0x07} ** 32);
    const identity = (try root.publicKey()).toCompressedSec1();
    const txid = try w.header.fromHex("aa11bb22cc33dd44ee55ff660011223344556677889900112233445566778899");
    var buf: [64 + 1 + 10]u8 = undefined;
    const key_id = pool_lib.keyId(&buf, txid, 0);
    try testing.expectEqualStrings("aa11bb22cc33dd44ee55ff660011223344556677889900112233445566778899_0", key_id);
    // The instance's signer (forSelf, counterparty anyone) and anyone's, from the identity alone.
    const mine = (try kd_mod.KeyDeriver.init(root).derivePublicKey(a, proto, key_id, .{ .type_ = .anyone }, true)).toCompressedSec1();
    try testing.expectEqualSlices(u8, &mine, &try pool_lib.commissionKey(a, identity, txid, 0));
    // The client's vector (web/ui test/keys.test.ts): PrivateKey(1).
    var g: [33]u8 = undefined;
    _ = try std.fmt.hexToBytes(&g, "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798");
    try testing.expectEqualStrings("3596b7bd46071cdfc8bf21ff720dad8b7ae1f677", &std.fmt.bytesToHex(try pool_lib.commissionPkh(a, g, txid, 0), .lower));
    // amm.swap.terms answers the identity, no key hash.
    const t = try iface.terms(a, .{ .identity = identity }, .{});
    try testing.expectEqualSlices(u8, &identity, t.getBytes("identityKey").?);
    try testing.expect(t.get("commissionPkh") == null);
}

test {
    _ = iface.market;
}

// ================================================================ the public read API (0.34.0)

const lookup_mod = @import("lookup");
const amm_idx = @import("amm_lookup");
const ov_engine = @import("skein_overlay");

fn ltxOf(a: Allocator, raw: []const u8) !lookup_mod.Tx {
    const txid = w.beef.txidOf(raw);
    const cid = w.store.hashCid(.tx, txid);
    return .{ .cid = try a.dupe(u8, cid[0..]), .txid = txid, .tx = try Transaction.parse(a, raw) };
}

fn mineHeader(prev: [32]u8, merkle_root: [32]u8, time: u32) [80]u8 {
    var h = w.header.Header{ .version = 1, .prev_hash = prev, .merkle_root = merkle_root, .time = time, .bits = 0x207fffff, .nonce = 0 };
    while (true) : (h.nonce += 1) {
        const raw = h.serialize();
        if (w.header.powOk(&raw)) return raw;
    }
}

/// A BUMP for a block holding one transaction: its root is the txid.
fn soloPath(a: Allocator, height: u32, txid: [32]u8) ![]const u8 {
    var path: std.ArrayList(u8) = .empty;
    try path.appendSlice(a, &.{ 0xfd, 0, 0, 0x01, 0x01, 0x00, 0x02 });
    std.mem.writeInt(u16, path.items[1..3], @intCast(height), .little);
    try path.appendSlice(a, &txid);
    return path.items;
}

/// No fee of ours is filed here (`self_identity` null): a filing would be a test failure.
fn noFile(_: Allocator, _: []const u8, _: Value) anyerror!void {
    return error.UnexpectedFiling;
}

/// The kernel's event index as the lookup reads it (0.49.0): what the hooks' steps emitted.
const Events = struct {
    var a: Allocator = undefined;
    var event_names: std.ArrayList([]const u8) = .empty;
    var records: std.ArrayList(Value) = .empty;

    fn reset(al: Allocator) void {
        a = al;
        event_names = .empty;
        records = .empty;
        amm_idx.app = "amm";
        amm_idx.self_identity = null;
        amm_idx.filer = noFile;
    }
    /// A step's end: nothing to do (skein 0.8.0: a step sees its own events; an emit enters at once).
    fn commit(_: *lookup_mod.Service) !void {}
    var dummy: u8 = 0;
    /// The kernel the lookup raises and reads with (skein-overlay 0.27.0): an emit enters at once,
    /// refused as the kernel refuses an event's `kind` / `app`.
    fn kernel() lookup_mod.Kernel {
        return .{ .ctx = &dummy, .emitFn = emitK, .eventsFn = eventsK };
    }
    fn emitK(_: *anyopaque, _: Allocator, ev: Value) anyerror!void {
        if (ev.get("kind") != null or ev.get("app") != null) return error.EmitRefused;
        try event_names.append(a, ev.getText("event").?);
        try records.append(a, ev);
    }
    fn eventsK(_: *anyopaque, al: Allocator, q: Value) anyerror!Value {
        return read(al, q);
    }
    fn item(al: Allocator, n: []const u8, r: Value) !Value {
        return .{ .map = try al.dupe(cbor.Entry, &.{ .{ .key = "name", .value = .{ .text = n } }, .{ .key = "record", .value = r } }) };
    }
    fn read(al: Allocator, q: Value) anyerror!Value {
        const latest = q.getBool("latest") orelse false;
        var out: std.ArrayList(Value) = .empty;
        if (q.getText("name")) |n| {
            var last: ?Value = null;
            for (event_names.items, records.items) |x, r| if (eql(u8, x, n)) {
                if (latest) last = r else try out.append(al, try item(al, x, r));
            };
            if (last) |r| try out.append(al, try item(al, n, r));
            return .{ .array = out.items };
        }
        const p = q.getText("prefix").?;
        var seen: std.ArrayList([]const u8) = .empty;
        for (event_names.items) |x| if (std.mem.startsWith(u8, x, p)) {
            for (seen.items) |y| {
                if (eql(u8, y, x)) break;
            } else try seen.append(al, x);
        };
        std.mem.sort([]const u8, seen.items, {}, struct {
            fn f(_: void, l: []const u8, r: []const u8) bool {
                return std.mem.order(u8, l, r) == .lt;
            }
        }.f);
        for (seen.items) |n| try out.appendSlice(al, (try read(al, .{ .map = try al.dupe(cbor.Entry, &.{ .{ .key = "name", .value = .{ .text = n } }, .{ .key = "latest", .value = .{ .boolean = latest } } }) })).array);
        return .{ .array = out.items };
    }
};

/// The fixtures' pool and its two Swaps, admitted by the pool lookup's hooks (as amm-lookup's own
/// tests do) over a store holding them; the chain state holding them too, `fund` proven in block 1.
const ReadEnv = struct {
    ms: w.store.MemStore,
    svc: lookup_mod.Service,
    ch: w.state.State,
    token: []const u8,
    topic: []const u8,

    fn init(a: Allocator) !*ReadEnv {
        const e = try a.create(ReadEnv);
        e.ms = w.store.MemStore.init(testing.allocator);
        const s = e.ms.store();
        const id = w.beef.txidOf(unhex(a, vec.token_deploy));
        e.token = try std.fmt.allocPrint(a, "{s}_0", .{&w.header.toHex(id)});
        e.topic = try amm_idx.topicOf(a, e.token);
        e.svc = try reads.loadLookup(a, s, null);
        e.ch = try w.state.State.load(a, s, null, .regtest);
        const all = [_][]const u8{ vec.fund, vec.token_deploy, vec.pool_deploy, vec.swap_bsv_in, vec.swap_tokens_in };
        var held = std.AutoHashMap([32]u8, void).init(a);
        for (all) |h| {
            const raw = unhex(a, h);
            _ = try s.putBitcoin(a, .tx, raw);
            _ = try e.ch.putTx(w.beef.txidOf(raw), raw);
            try held.put(w.beef.txidOf(raw), {});
        }
        // Mined, one block each: every fixture transaction spending one not held (the funding ones),
        // so the chain state's BEEFs reach proven transactions.
        var prev = w.header.hash(&w.chain.Network.regtest.genesis());
        var height: u32 = 0;
        for (all) |h| {
            const t = try Transaction.parse(a, unhex(a, h));
            var roots = false;
            for (t.inputs) |in| roots = roots or !held.contains(in.previous_outpoint.txid.bytes);
            if (!roots) continue;
            const txid = w.beef.txidOf(unhex(a, h));
            height += 1;
            const hd = mineHeader(prev, txid, 1_700_000_000 + height * 600);
            _ = try e.ch.addHeaders(&.{&hd});
            prev = w.header.hash(&hd);
            try e.ch.putProof(txid, try w.merkle.MerklePath.parse(a, try soloPath(a, height, txid)));
        }
        const pd = unhex(a, vec.pool_deploy);
        const s1 = unhex(a, vec.swap_bsv_in);
        const s2 = unhex(a, vec.swap_tokens_in);
        Events.reset(a);
        e.svc.kernel = Events.kernel();
        e.svc.app = "amm";
        try amm_idx.admitted(a, &e.svc, e.topic, try ltxOf(a, pd), &.{ 0, 1, 2 }, &.{0});
        try Events.commit(&e.svc);
        try amm_idx.admitted(a, &e.svc, e.topic, try ltxOf(a, s1), &.{ 0, 1 }, &.{0});
        try amm_idx.spent(a, &e.svc, e.topic, .{ .txid = w.beef.txidOf(pd), .vout = 0 }, try ltxOf(a, s1));
        try Events.commit(&e.svc);
        try amm_idx.admitted(a, &e.svc, e.topic, try ltxOf(a, s2), &.{ 0, 2, 3, 4 }, &.{ 0, 1 });
        try amm_idx.spent(a, &e.svc, e.topic, .{ .txid = w.beef.txidOf(s1), .vout = 0 }, try ltxOf(a, s2));
        try Events.commit(&e.svc);
        return e;
    }
    fn deinit(self: *ReadEnv) void {
        self.ms.deinit();
    }
};

const unknown_lp = "02" ++ "a0" ** 32;
/// A pool id no pool has, and the fixtures' (its deploy's outpoint, `<txid>_0`; 0.40.0).
const unknown_pool = "a0" ** 32 ++ "_0";
fn poolText(a: Allocator) ![]const u8 {
    return std.fmt.allocPrint(a, "{s}_0", .{&w.header.toHex(w.beef.txidOf(unhex(a, vec.pool_deploy)))});
}

fn jsonOf(a: Allocator, text: []const u8) !std.json.Value {
    return std.json.parseFromSliceLeaky(std.json.Value, a, text, .{});
}

test "read API (0.34.0): the routes and their filters in the manifest — plain reads, no handler; no /pools (the host's)" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const m = try dagjson.decode(a, manifest_json);
    const want = [_][2][]const u8{
        .{ "/positions", "positions" },
        .{ "/fees/tokens", "feeTokens" },
        .{ "/terms", "readTerms" },
        .{ "/spends", "spends" },
        .{ "/tx", "tx" },
    };
    // 0.49.0: gone — no reader (`/fees/validator`, `/fees/commission`) or the host's folds over the
    // pool events (`/trades`, `/candles`, `/stats`, `/pools/at`, `/pools/history`).
    for ([_][]const u8{ "/fees/validator", "/fees/commission", "/trades", "/candles", "/stats", "/pools/at", "/pools/history" }) |gone|
        for (m.get("routes").?.array) |r| try testing.expect(!eql(u8, scbor.Value.str(r.get("address")).?, gone));
    for ([_][]const u8{ "validatorFees", "commissionFees", "trades", "candles", "stats", "poolsAt", "poolsHistory" }) |gone| try testing.expect(m.get("filters").?.get(gone) == null);
    for (want) |x| {
        try testing.expectEqualStrings(try std.fmt.allocPrint(a, "amm-p2p.{s}", .{x[1]}), scbor.Value.str(m.get("filters").?.get(x[1])).?);
        var found = false;
        for (m.get("routes").?.array) |r| {
            if (!eql(u8, scbor.Value.str(r.get("address")).?, x[0])) continue;
            try testing.expectEqualStrings("http", scbor.Value.str(r.get("transport")).?);
            try testing.expect(r.get("handler") == null);
            try testing.expectEqualStrings(x[1], scbor.Value.str(r.get("filters").?.array[0]).?);
            found = true;
        }
        try testing.expect(found);
    }
    for (m.get("routes").?.array) |r| try testing.expect(!eql(u8, scbor.Value.str(r.get("address")).?, "/pools"));
    // 0.37.0: the token's 1Color reads (color-lookup.token: /color/<tokenId>/holders) under one prefix row.
    try testing.expectEqualStrings("color-lookup.token", scbor.Value.str(m.get("filters").?.get("colorToken")).?);
    var prefix_row = false;
    for (m.get("routes").?.array) |r| if (eql(u8, scbor.Value.str(r.get("address")).?, "/color/")) {
        try testing.expect(r.get("handler") == null);
        try testing.expectEqualStrings("colorToken", scbor.Value.str(r.get("filters").?.array[0]).?);
        prefix_row = true;
    };
    try testing.expect(prefix_row);
}

test "read API (0.34.0): list parameters — comma-separated, percent-decoded, in order; absent is null" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const l = (try reads.queryList(a, "?x=1&lp=02aa%2C03bb,,04cc", "lp")).?;
    try testing.expectEqual(@as(usize, 3), l.len);
    try testing.expectEqualStrings("02aa", l[0]);
    try testing.expectEqualStrings("03bb", l[1]);
    try testing.expectEqualStrings("04cc", l[2]);
    try testing.expect((try reads.queryList(a, "lpx=1", "lp")) == null);
    try testing.expectEqual(@as(usize, 0), (try reads.queryList(a, "lp=", "lp")).?.len);
}

test "read API (0.34.0; 0.40.0 by pool id): P4 /positions — the lookup's {poolIds} answer, one record per id in query order, an unknown id closed with zeros; a bad id refused; (0.49.0) P5, P6 gone" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const e = try ReadEnv.init(a);
    defer e.deinit();
    const s2 = w.beef.txidOf(unhex(a, vec.swap_tokens_in));

    const v = try reads.lookupAnswer(a, &e.svc, &e.ch, e.token, "poolIds", &.{ unknown_pool, try poolText(a) });
    const j = try jsonOf(a, try reads.valueJson(a, v));
    try testing.expectEqual(@as(usize, 2), j.array.items.len);
    const first = j.array.items[0].object;
    try testing.expectEqualStrings(unknown_pool, first.get("poolId").?.string);
    try testing.expect(first.get("lpPubKey").? == .null);
    try testing.expectEqualStrings("closed", first.get("status").?.string);
    try testing.expectEqual(@as(i64, 0), first.get("sats").?.integer);
    const mine = j.array.items[1].object;
    try testing.expectEqualStrings(try poolText(a), mine.get("poolId").?.string);
    try testing.expectEqualStrings(vec.lp, mine.get("lpPubKey").?.string);
    try testing.expectEqualStrings("live", mine.get("status").?.string);
    try testing.expectEqualStrings(try std.fmt.allocPrint(a, "{s}_0", .{&w.header.toHex(s2)}), mine.get("outpoint").?.string);
    try testing.expectEqualStrings(vec.lp, mine.get("current").?.object.get("lpPubKey").?.string);
    try testing.expectEqualStrings(try poolText(a), mine.get("current").?.object.get("poolId").?.string);
    try testing.expectEqual(@as(i64, 2), mine.get("current").?.object.get("swaps").?.integer);
    try testing.expectEqual(@as(i64, 2), mine.get("swaps").?.integer);

    try testing.expectError(error.BadQuery, reads.lookupAnswer(a, &e.svc, &e.ch, e.token, "validatorIdentityKeys", &.{vec.identity}));
    try testing.expectError(error.BadQuery, reads.lookupAnswer(a, &e.svc, &e.ch, e.token, "commissionPkhs", &.{"55" ** 20}));

    // 0.49.0: /fees/tokens — the token fee tally (none of ours here).
    const tj = try jsonOf(a, try reads.tallyJson(a, e.token, try amm_idx.tally(a, &e.svc, try amm_idx.tokenPrefix(a, e.token))));
    try testing.expectEqualStrings(e.token, tj.object.get("tokenId").?.string);
    try testing.expectEqual(@as(i64, 0), tj.object.get("units").?.integer);

    try testing.expectError(error.BadQuery, reads.lookupAnswer(a, &e.svc, &e.ch, e.token, "poolIds", &.{"02zz"}));
}

test "read API (0.34.0): P8 /spends list form — one record per outpoint in query order" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const e = try ReadEnv.init(a);
    defer e.deinit();
    const pd = w.beef.txidOf(unhex(a, vec.pool_deploy));
    const s2 = w.beef.txidOf(unhex(a, vec.swap_tokens_in));
    const ss = [_]reads.Spend{
        try reads.spendOf(&e.ch, .{ .txid = s2, .vout = 0 }),
        try reads.spendOf(&e.ch, .{ .txid = pd, .vout = 0 }),
    };
    const j = try jsonOf(a, try reads.spendsJson(a, &ss));
    try testing.expectEqual(@as(usize, 2), j.array.items.len);
    try testing.expect(j.array.items[0].object.get("spentBy") == null);
    try testing.expect(j.array.items[1].object.get("spentBy") != null);
    // 0.47.0: no pool following.
    for (j.array.items) |x| try testing.expectEqual(@as(usize, if (x.object.get("spentBy") == null) 1 else 2), x.object.count());
}

test "read API (0.34.0): P9 /tx — the transaction's BEEF; immutable only when proven, else the short cache; unknown none" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const e = try ReadEnv.init(a);
    defer e.deinit();
    const fund = w.beef.txidOf(unhex(a, vec.fund));
    const s2 = w.beef.txidOf(unhex(a, vec.swap_tokens_in));
    try testing.expectEqual(reads.Cache.imm, try reads.txCache(&e.ch, fund));
    try testing.expectEqual(reads.Cache.state, try reads.txCache(&e.ch, s2));
    try testing.expect((try e.ch.beefOf(s2)) != null);
    try testing.expect((try e.ch.beefOf(.{0x77} ** 32)) == null);
    try testing.expect(reads.parseTxid(&w.header.toHex(s2)) != null);
    try testing.expect(reads.parseTxid("abc") == null);
    const h = try reads.httpCached(a, 200, "application/octet-stream", .imm, "x");
    try testing.expectEqualStrings("public, max-age=31536000, immutable", h.get("headers").?.getText("cache-control").?);
    try testing.expectEqualStrings("application/octet-stream", h.getText("type").?);
    try testing.expectEqualStrings("public, max-age=5", reads.Cache.state.header());
}

test "read API (0.34.0): P7 /terms — the validator's terms with its identity and peer, and the market's; no terms, validator null" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const id = unhex(a, vec.identity);
    const j = try jsonOf(a, try reads.termsJson(a, .{ .identity = id, .peer_id = PEER_ID_OF_G, .lp_fee_bps = 30, .validator_fee_bps = 5, .commission_bps = 25, .min_leg_sats = 10_000 }, id, 10));
    const v = j.object.get("validator").?.object;
    try testing.expectEqualStrings(vec.identity, v.get("identity").?.string);
    try testing.expectEqualStrings(PEER_ID_OF_G, v.get("peerId").?.string);
    try testing.expectEqual(@as(i64, 30), v.get("lpFeeBps").?.integer);
    try testing.expectEqual(@as(i64, 5), v.get("validatorFeeBps").?.integer);
    try testing.expectEqual(@as(i64, 25), v.get("commissionBps").?.integer);
    try testing.expectEqual(@as(i64, 10_000), v.get("minLegSats").?.integer); // 0.40.0
    const m = j.object.get("market").?.object;
    // 0.49.0: the market's identity (each leg's commission key derives from it), no commissionPkh.
    try testing.expectEqualStrings(vec.identity, m.get("identityKey").?.string);
    try testing.expect(m.get("commissionPkh") == null);
    try testing.expectEqual(@as(i64, 10), m.get("minCommissionBps").?.integer);
    const none = try jsonOf(a, try reads.termsJson(a, null, null, 0));
    try testing.expect(none.object.get("market").?.object.get("identityKey").? == .null);
    try testing.expect(none.object.get("validator").? == .null);
    try testing.expectEqual(@as(i64, 0), none.object.get("market").?.object.get("minCommissionBps").?.integer);
}

fn httpBody(v: Value) []const u8 {
    return v.getBytes("body").?;
}

fn httpStatus(v: Value) u64 {
    return v.getUint("status").?;
}

test "spends (0.40.0, VERIFY): /spends answers spentBy for a plain sats output a Swap spent — the taker's funding output fund:1, spent by swap_bsv_in, held by the chain state as the overlay's admission leaves it" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const e = try ReadEnv.init(a);
    defer e.deinit();
    const fund = w.beef.txidOf(unhex(a, vec.fund));
    const s1 = w.beef.txidOf(unhex(a, vec.swap_bsv_in));
    // fund:1 is a plain P2PKH output (the taker's sats), no token prefix, not a pool.
    const ft = try Transaction.parse(a, unhex(a, vec.fund));
    try testing.expectEqual(@as(usize, 25), ft.outputs[1].locking_script.bytes.len);
    const sp = try reads.spendOf(&e.ch, .{ .txid = fund, .vout = 1 });
    try testing.expectEqualSlices(u8, &s1, &sp.spent_by.?);
    const j = (try jsonOf(a, try reads.spendJson(a, sp))).object;
    try testing.expectEqualStrings(&w.header.toHex(s1), j.get("spentBy").?.string);
    // An output nobody spent (fund:3): no spentBy.
    try testing.expect((try reads.spendOf(&e.ch, .{ .txid = fund, .vout = 3 })).spent_by == null);
}

