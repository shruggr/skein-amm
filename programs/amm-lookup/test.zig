//! amm-lookup, natively (0.49.0: the recognizer): the hooks (admitted, spent, rejected) over the
//! real fixtures (src/fixtures/vectors.zig, gen/vectors), the events they emit, the summaries they
//! store, the fees that are ours, the queries. The hooks are called directly, as skein-overlay's
//! engine calls them; a step's events are committed to an in-memory event index (`Index`) only
//! when the step ends (`Env.commit`), as the kernel indexes a step's emits when its update is
//! written: a hook never reads its own step's emits.
const std = @import("std");
const w = @import("chain");
const lookup = @import("lookup");
const pool = @import("pool");
const vec = @import("vectors");
const idx = @import("src/index.zig");

const bsvz = w.bsvz;
const cbor = w.cbor;
const Value = cbor.Value;
const Entry = cbor.Entry;
const Allocator = std.mem.Allocator;
const testing = std.testing;

fn unhex(a: Allocator, h: []const u8) []u8 {
    const out = a.alloc(u8, h.len / 2) catch @panic("OOM");
    _ = std.fmt.hexToBytes(out, h) catch @panic("bad hex");
    return out;
}

fn cat(a: Allocator, parts: []const []const u8) ![]u8 {
    return std.mem.concat(a, u8, parts);
}

fn key33(hex: []const u8) [33]u8 {
    var k: [33]u8 = undefined;
    _ = std.fmt.hexToBytes(&k, hex) catch unreachable;
    return k;
}

/// A synthetic (invalid, first-byte-only) compressed key: good enough for
/// pool.parse, which only checks the prefix byte (used for LP keys, which no
/// check derives from).
fn key(tag: u8) [33]u8 {
    var k: [33]u8 = .{0x02} ** 33;
    k[1] = tag;
    return k;
}

const Fixtures = struct {
    txs: std.AutoHashMap([32]u8, bsvz.transaction.Transaction),
    raws: std.StringHashMap([]const u8),
    id: [32]u8,

    fn init(a: Allocator) !Fixtures {
        var f: Fixtures = .{ .txs = .init(a), .raws = .init(a), .id = undefined };
        inline for (.{ "fund", "token_deploy", "pool_deploy", "swap_bsv_in", "swap_tokens_in", "close", "rescind", "close_rescinded", "pool_deploy_unaffirmed", "pool_deploy_forged", "pool_deploy_own_key", "swap_bad_next" }) |name| {
            const bytes = unhex(a, @field(vec, name));
            try f.raws.put(name, bytes);
            try f.txs.put(w.beef.txidOf(bytes), try bsvz.transaction.Transaction.parse(a, bytes));
        }
        f.id = w.beef.txidOf(f.raws.get("token_deploy").?);
        return f;
    }
    fn raw(self: Fixtures, name: []const u8) []const u8 {
        return self.raws.get(name).?;
    }
    fn tx(self: Fixtures, name: []const u8) bsvz.transaction.Transaction {
        return self.txs.get(w.beef.txidOf(self.raws.get(name).?)).?;
    }
    fn txid(self: Fixtures, name: []const u8) [32]u8 {
        return w.beef.txidOf(self.raws.get(name).?);
    }
    /// Every fixture transaction held as a bitcoin-tx block, as the overlay
    /// holds a submission and its ancestry: what `admitted` reads the
    /// previous coins' sources from.
    fn hold(self: Fixtures, a: Allocator, s: w.store.Store) !void {
        var it = self.raws.valueIterator();
        while (it.next()) |r| _ = try s.putBitcoin(a, .tx, r.*);
    }
};

/// `lookup.Tx`, as a hook receives it (its CID does not matter to this
/// service — it never calls `Service.tx` itself — but must not be a
/// dangling stack pointer).
fn ltx(a: Allocator, txid: [32]u8, t: bsvz.transaction.Transaction) !lookup.Tx {
    const cid = w.store.hashCid(.tx, txid);
    return .{ .cid = try a.dupe(u8, cid[0..]), .txid = txid, .tx = t };
}

fn serviceAndTopic(a: Allocator, id: [32]u8) !struct { service: []const u8, topic: []const u8, token: []const u8 } {
    const token = try std.fmt.allocPrint(a, "{s}_0", .{&w.header.toHex(id)});
    try testing.expectEqualStrings(try std.fmt.allocPrint(a, "tm_color_{s}_0", .{&w.header.toHex(id)}), try idx.topicOf(a, token));
    return .{ .service = idx.service_name, .topic = try idx.topicOf(a, token), .token = token };
}

/// A query naming the token `token`, with `rest`.
fn query(a: Allocator, token: []const u8, rest: []const Entry) !Value {
    return .{ .map = try std.mem.concat(a, Entry, &.{ &.{.{ .key = "tokenId", .value = .{ .text = token } }}, rest }) };
}

fn opStr(a: Allocator, txid: [32]u8, vout: u32) ![]const u8 {
    return std.fmt.allocPrint(a, "{s}_{d}", .{ &w.header.toHex(txid), vout });
}

/// `pool_deploy`'s output-0 script, with the state's TokenReserve and keys
/// overwritten and the prefix amount set to the new reserve: a spend's
/// continuation (or an unrelated pool), sharing the real,
/// gen/main.go-compiled Pool template so `pool.parse` still succeeds. The
/// Nonce and Affirmation are kept, unless `attest` names others (0.13.0).
fn withState(a: Allocator, id: [32]u8, script: []const u8, token_reserve: u64, lp: [33]u8, validator: [33]u8, identity: [33]u8) ![]u8 {
    return withStateAttested(a, id, script, token_reserve, lp, validator, identity, null);
}

const Attest = struct { nonce: [32]u8, affirmation: []const u8 };

fn withStateAttested(a: Allocator, id: [32]u8, script: []const u8, token_reserve: u64, lp: [33]u8, validator: [33]u8, identity: [33]u8, attest: ?Attest) ![]u8 {
    const lock = pool.brc162.decode(script).?.lock;
    const p = (try pool.parse(lock)).?;
    var st: [8 + 99 + 32]u8 = undefined;
    std.mem.writeInt(u64, st[0..8], token_reserve, .little);
    @memcpy(st[8..][0..33], &lp);
    @memcpy(st[8 + 33 ..][0..33], &validator);
    @memcpy(st[8 + 66 ..][0..33], &identity);
    @memcpy(st[8 + 99 ..][0..32], if (attest) |t| &t.nonce else &p.nonce);
    const af = if (attest) |t| t.affirmation else p.affirmation;
    var buf: [10]u8 = undefined;
    return cat(a, &.{ &.{0x20}, &id, pool.brc162.pushAmount(&buf, token_reserve), &.{0x6d}, lock[0 .. p.code_len + 1], &st, &.{@intCast(af.len)}, af });
}

/// `script` with the state's ValidatorPubKey replaced.
fn withValidator(a: Allocator, script: []const u8, validator: [33]u8) ![]u8 {
    const c = try a.dupe(u8, script);
    const lock = pool.brc162.decode(script).?.lock;
    const p = (try pool.parse(lock)).?;
    @memcpy(c[script.len - lock.len + p.code_len + 1 + 8 + 33 ..][0..33], &validator);
    return c;
}

/// `t` with output `i`'s script replaced (same txid: the hooks take it as given).
fn withOutput(a: Allocator, t: bsvz.transaction.Transaction, i: usize, script: []const u8) !bsvz.transaction.Transaction {
    const outs = try a.dupe(bsvz.transaction.Output, t.outputs);
    outs[i].locking_script = bsvz.script.Script.init(script);
    var r = t;
    r.outputs = outs;
    return r;
}

const Built = struct { raw: []const u8, txid: [32]u8, tx: bsvz.transaction.Transaction };

/// A transaction spending `prev`'s output `prev_vout` with `unlocking` (a
/// pool spend's method index is its last push: OP_0 Swap, OP_1 Close, OP_2 Rescind),
/// with one output (`script`, `sats`), and a second (`second`, 1 sat) when given.
/// Unsigned: nothing here runs the interpreter.
fn buildSpend(a: Allocator, prev: bsvz.transaction.Transaction, prev_vout: u32, unlocking: []const u8, script: []const u8, sats: i64, second: ?[]const u8) !Built {
    var b = bsvz.transaction.Builder.init(a);
    try b.addInputFromTx(&prev, prev_vout);
    try b.addOutput(.{ .satoshis = sats, .locking_script = bsvz.script.Script.init(script) });
    if (second) |s2| try b.addOutput(.{ .satoshis = 1, .locking_script = bsvz.script.Script.init(s2) });
    var tx = try b.build();
    @constCast(tx.inputs)[0].unlocking_script = bsvz.script.Script.init(unlocking);
    const raw = try tx.serialize(a);
    return .{ .raw = raw, .txid = w.beef.txidOf(raw), .tx = try bsvz.transaction.Transaction.parse(a, raw) };
}

// ---------------------------------------------------------------- the event index (the tests')

/// What the kernel's event index would hold: every committed emit, in log order.
const Index = struct {
    var a: Allocator = undefined;
    var names: std.ArrayList([]const u8) = .empty;
    var records: std.ArrayList(Value) = .empty;

    fn reset(al: Allocator) void {
        a = al;
        names = .empty;
        records = .empty;
    }

    fn put(name: []const u8, rec: Value) !void {
        try names.append(a, name);
        try records.append(a, rec);
    }

    fn item(al: Allocator, name: []const u8, rec: Value) !Value {
        return .{ .map = try al.dupe(Entry, &.{ .{ .key = "name", .value = .{ .text = name } }, .{ .key = "record", .value = rec } }) };
    }

    /// `{name | prefix, latest?}`, as skein's `events` answers it: in name order, then log order.
    fn read(al: Allocator, q: Value) anyerror!Value {
        const latest = q.getBool("latest") orelse false;
        var out: std.ArrayList(Value) = .empty;
        if (q.getText("name")) |n| {
            var last: ?Value = null;
            for (names.items, records.items) |x, r| if (std.mem.eql(u8, x, n)) {
                if (latest) last = r else try out.append(al, try item(al, x, r));
            };
            if (last) |r| try out.append(al, try item(al, n, r));
            return .{ .array = out.items };
        }
        const p = q.getText("prefix") orelse return error.BadQuery;
        var seen: std.ArrayList([]const u8) = .empty;
        for (names.items) |x| if (std.mem.startsWith(u8, x, p)) {
            for (seen.items) |s| {
                if (std.mem.eql(u8, s, x)) break;
            } else try seen.append(al, x);
        };
        std.mem.sort([]const u8, seen.items, {}, struct {
            fn f(_: void, l: []const u8, r: []const u8) bool {
                return std.mem.order(u8, l, r) == .lt;
            }
        }.f);
        for (seen.items) |n| {
            const one = try read(al, .{ .map = try al.dupe(Entry, &.{ .{ .key = "name", .value = .{ .text = n } }, .{ .key = "latest", .value = .{ .boolean = latest } } }) });
            try out.appendSlice(al, one.array);
        }
        return .{ .array = out.items };
    }

    var dummy: u8 = 0;
    /// The kernel as a lookup's hook has it (skein-overlay 0.27.0 `lookup.Kernel`): an emit enters
    /// the index at once — a step sees its own events (skein 0.8.0) — and is refused as the kernel
    /// refuses it ("an event's 'kind' and 'app' are the kernel's to set").
    fn kernel() lookup.Kernel {
        return .{ .ctx = &dummy, .emitFn = emitK, .eventsFn = eventsK };
    }
    fn emitK(_: *anyopaque, _: Allocator, ev: Value) anyerror!void {
        if (ev.get("kind") != null or ev.get("app") != null) return error.EmitRefused;
        try put(ev.getText("event") orelse return error.BadEvent, ev);
    }
    fn eventsK(_: *anyopaque, al: Allocator, q: Value) anyerror!Value {
        return read(al, q);
    }

    fn count(prefix: []const u8) usize {
        var n: usize = 0;
        for (names.items) |x| n += @intFromBool(std.mem.startsWith(u8, x, prefix));
        return n;
    }
};

// ---------------------------------------------------------------- the wallet's `file`, stood in for

/// The wallet app's `file` as the named call reaches it: recorded (basket, arg); `refuse` makes it
/// fail, as a refused call does.
/// The i-th `file` call, read back: the outpoint, the derivation, the basket (the wallet reads the
/// satoshis from the chain state itself).
const Got = struct { txid: [32]u8, vout: u32, basket: []const u8, protocol_level: u64, protocol: []const u8, key_id: []const u8, counterparty: []const u8 };
fn got(i: usize) !Got {
    const c = Filed.calls.items[i];
    const op = c.arg.getText("outpoint").?;
    const dot = std.mem.indexOfScalar(u8, op, '.').?;
    const pr = c.arg.getArray("protocol").?;
    try testing.expect(c.arg.get("satoshis") == null);
    return .{
        .txid = try w.header.fromHex(op[0..dot]),
        .vout = try std.fmt.parseInt(u32, op[dot + 1 ..], 10),
        .basket = c.basket,
        .protocol_level = pr[0].uint,
        .protocol = pr[1].text,
        .key_id = c.arg.getText("keyID").?,
        .counterparty = c.arg.getText("counterparty").?,
    };
}

const Filed = struct {
    var refuse: bool = false;
    var a: Allocator = undefined;
    var calls: std.ArrayList(struct { basket: []const u8, arg: Value }) = .empty;
    fn file(_: Allocator, basket: []const u8, arg: Value) anyerror!void {
        if (refuse) return error.CallRefused;
        try calls.append(a, .{ .basket = basket, .arg = arg });
    }
};

// ---------------------------------------------------------------- the environment

const Env = struct {
    ms: w.store.MemStore,
    f: Fixtures,
    service: []const u8,
    topic: []const u8,
    token: []const u8,

    fn init(a: Allocator) !*Env {
        const e = try a.create(Env);
        e.ms = w.store.MemStore.init(testing.allocator);
        e.f = try Fixtures.init(a);
        try e.f.hold(a, e.ms.store());
        const st = try serviceAndTopic(a, e.f.id);
        e.service = st.service;
        e.topic = st.topic;
        e.token = st.token;
        Index.reset(a);
        Filed.a = a;
        Filed.calls = .empty;
        Filed.refuse = false;
        idx.app = "amm";
        idx.self_identity = null;
        idx.filer = Filed.file;
        idx.now_ms = 1000;
        return e;
    }
    fn deinit(self: *Env) void {
        self.ms.deinit();
    }
    fn svc(self: *Env, a: Allocator) !lookup.Service {
        var s = try lookup.Service.load(a, self.ms.store(), self.service, idx.spec.maps, null);
        s.kernel = Index.kernel();
        s.app = "amm";
        return s;
    }
    fn chain(self: *Env, a: Allocator) !lookup.Chain {
        return lookup.Chain.load(a, self.ms.store(), null, .regtest);
    }
    fn q(self: *Env, a: Allocator, rest: []const Entry) !Value {
        return query(a, self.token, rest);
    }
    /// The step ends (skein 0.8.0: a step sees its own events, so nothing waits for its end).
    fn commit(_: *Env, _: *lookup.Service) !void {}
    /// One admission step: `admitted` on fixture `name` (or a variant `t` of it), then `spent` for
    /// each previous coin `prev` it consumed, then the commit.
    fn admit(self: *Env, a: Allocator, s: *lookup.Service, name: []const u8, t: ?bsvz.transaction.Transaction, outs: []const u32, prev: []const lookup.Outpoint) !void {
        const lt = try ltx(a, self.f.txid(name), t orelse self.f.tx(name));
        try idx.admitted(a, s, self.topic, lt, outs, &.{});
        for (prev) |p| try idx.spent(a, s, self.topic, p, lt);
        try self.commit(s);
    }
    fn reject(self: *Env, a: Allocator, s: *lookup.Service, name: []const u8, t: ?bsvz.transaction.Transaction) !void {
        try idx.rejected(a, s, self.topic, try ltx(a, self.f.txid(name), t orelse self.f.tx(name)));
        try self.commit(s);
    }
    fn op(self: *Env, name: []const u8, vout: u32) lookup.Outpoint {
        return .{ .txid = self.f.txid(name), .vout = vout };
    }
    fn live(self: *Env, a: Allocator, s: *lookup.Service) ![]const Value {
        var ch = try self.chain(a);
        return (try idx.answer(a, s, &ch, try self.q(a, &.{}))).freeform.array;
    }
    /// deploy → a buy (swap_bsv_in) → a sell (swap_tokens_in), each its own step.
    fn threeHops(self: *Env, a: Allocator, s: *lookup.Service) !void {
        try self.admit(a, s, "pool_deploy", null, &.{ 0, 1, 2 }, &.{});
        try self.admit(a, s, "swap_bsv_in", null, &.{ 0, 1 }, &.{self.op("pool_deploy", 0)});
        try self.admit(a, s, "swap_tokens_in", null, &.{ 0, 2, 3, 4 }, &.{ self.op("swap_bsv_in", 0), self.op("swap_bsv_in", 1) });
    }
};

fn latestRow(a: Allocator, name: []const u8) !?Value {
    const ans = try Index.read(a, .{ .map = try a.dupe(Entry, &.{ .{ .key = "name", .value = .{ .text = name } }, .{ .key = "latest", .value = .{ .boolean = true } } }) });
    return if (ans.array.len == 0) null else ans.array[ans.array.len - 1].get("record");
}

/// The Swaps a pool's counters hold (0.50.0: the token's counters are gone): its standing swap hops
/// whose amounts the counters add up to — checked against the counters' satsIn + tokensIn.
fn tradesOf(a: Allocator, svc: *lookup.Service, tp: []const u8, pid: []const u8) !u64 {
    var n: u64 = 0;
    var ins: u64 = 0;
    for (try idx.standing(a, svc, pid)) |h| if (std.mem.eql(u8, h.getText("type") orelse "", "swap")) {
        n += 1;
        ins += h.getUint("amountIn").?;
    };
    const op = try idx.parseOutpoint(pid);
    const c = try idx.poolCounters(a, svc, tp, &w.store.outpointKey(op.txid, op.vout));
    try testing.expectEqual(ins, c.sats_in + c.tokens_in);
    return n;
}

fn poolIdOfEnv(a: Allocator, e: *Env) ![]const u8 {
    return opStr(a, e.f.txid("pool_deploy"), 0);
}

// ---------------------------------------------------------------- tests

test "names: a query's tokenId <txid>, <txid>_0 or <txid>.0 is the topic tm_color_<txid>_0, <txid>_<vout> is tm_color_<txid>_<vout>; other ids are refused; the event names" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const hex = "0102030405060708091011121314151617181920212223242526272829303132";
    try testing.expectEqualStrings("tm_color_" ++ hex ++ "_0", try idx.topicOf(a, hex));
    try testing.expectEqualStrings("tm_color_" ++ hex ++ "_0", try idx.topicOf(a, hex ++ "_0"));
    try testing.expectEqualStrings("tm_color_" ++ hex ++ "_0", try idx.topicOf(a, hex ++ ".0"));
    try testing.expectEqualStrings("tm_color_" ++ hex ++ "_17", try idx.topicOf(a, hex ++ "_17"));
    try testing.expectError(error.BadQuery, idx.topicOf(a, hex ++ "_017"));
    try testing.expectError(error.BadQuery, idx.topicOf(a, "abcd_0"));
    try testing.expect(idx.tokenIdOf("tm_color") == null);
    try testing.expect(idx.tokenIdOf("tm_mandala_" ++ hex ++ "_0") == null);
    try testing.expectEqualStrings("ls_amm", idx.spec.index.?);
    // 0.49.0: the summaries only.
    try testing.expectEqual(@as(usize, 1), idx.maps.len);
    idx.app = "amm";
    const txid = try w.header.fromHex(hex);
    try testing.expectEqualStrings("amm/pool/" ++ hex ++ "_3", try idx.poolName(a, hex ++ "_3"));
    try testing.expectEqualStrings("amm/output/" ++ hex ++ ".3", try idx.outputName(a, txid, 3));
    try testing.expectEqualStrings("amm/hop", try idx.hopName(a));
    try testing.expectEqualStrings("amm/commission/" ++ hex ++ ".4", try idx.commissionName(a, txid, 4));
}

test "deploy: an affirmed pool deploy is emitted as its hop under amm/pool/<poolId>, amm/output/<op> and amm/hop; the live list answers it; a re-admission emits nothing; another token, another topic nothing" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const e = try Env.init(a);
    defer e.deinit();
    var svc = try e.svc(a);
    var ch = try e.chain(a);
    const pd = e.f.txid("pool_deploy");
    const pid = try poolIdOfEnv(a, e);

    try e.admit(a, &svc, "pool_deploy", null, &.{ 0, 1, 2 }, &.{});
    try e.admit(a, &svc, "pool_deploy", null, &.{ 0, 1, 2 }, &.{}); // a dupe hook call
    try testing.expectEqual(@as(usize, 3), Index.names.items.len);
    const row = (try latestRow(a, try idx.poolName(a, pid))).?;
    try testing.expectEqualStrings("deploy", row.getText("type").?);
    try testing.expectEqualStrings(&w.header.toHex(pd), row.getText("txid").?);
    try testing.expectEqualStrings(e.token, row.getText("tokenId").?);
    try testing.expectEqualStrings(pid, row.getText("poolId").?);
    try testing.expectEqualStrings(pid, row.getText("outpoint").?);
    try testing.expectEqual(@as(u64, vec.pool0.bsv), row.getUint("bsvReserve").?);
    try testing.expectEqual(@as(u64, vec.pool0.tokens), row.getUint("tokenReserve").?);
    try testing.expectEqualStrings(vec.lp, row.getText("lpPubKey").?);
    try testing.expectEqualStrings(vec.identity, row.getText("validatorIdentityKey").?);
    try testing.expectEqual(@as(u64, 1000), row.getUint("at").?);
    try testing.expectEqual(@as(u64, 0), row.getUint("swaps").?);
    try testing.expectEqualStrings(vec.nonce, row.getText("keyId").?);
    // The same row under the output's name and the instance-wide name.
    try testing.expectEqualStrings("deploy", (try latestRow(a, try idx.outputName(a, pd, 0))).?.getText("type").?);
    try testing.expectEqualStrings(pid, (try latestRow(a, "amm/hop")).?.getText("poolId").?);

    // {tokenId}: THE pool row (0.48.0 shape: 12 fields, `lifetime` last).
    const all = try e.live(a, &svc);
    try testing.expectEqual(@as(usize, 1), all.len);
    try testing.expectEqual(@as(usize, 12), all[0].map.len);
    try testing.expectEqualStrings("poolId", all[0].map[0].key);
    try testing.expectEqualStrings("outpoint", all[0].map[1].key);
    try testing.expectEqualStrings("lifetime", all[0].map[11].key);
    try testing.expect(all[0].get("lifetime").?.get("lastTradeAt").? == .null);
    // Another token: nothing of it.
    const other = try query(a, "0102030405060708091011121314151617181920212223242526272829303132_0", &.{});
    try testing.expectEqual(@as(usize, 0), (try idx.answer(a, &svc, &ch, other)).freeform.array.len);
    try testing.expectError(error.BadQuery, idx.answer(a, &svc, &ch, .{ .map = &.{} }));
    // Another topic's call: nothing.
    const before = Index.names.items.len;
    try idx.admitted(a, &svc, "tm_demo", try ltx(a, pd, e.f.tx("pool_deploy")), &.{0}, &.{});
    try testing.expectEqual(before, Index.names.items.len);
}

test "listing: pools that fail the pool checks, and deploys that do not attest themselves, are not emitted" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const e = try Env.init(a);
    defer e.deinit();
    var svc = try e.svc(a);
    const pd = e.f.tx("pool_deploy");
    const pd_script = pd.outputs[0].locking_script.bytes;
    var buf: [10]u8 = undefined;
    const low = try cat(a, &.{ &.{0x20}, &e.f.id, pool.brc162.pushAmount(&buf, vec.pool0.tokens - 1), &.{0x6d}, pool.brc162.decode(pd_script).?.lock });
    try e.admit(a, &svc, "pool_deploy", try withOutput(a, pd, 0, low), &.{ 0, 1, 2 }, &.{});
    for ([_][]const u8{ "pool_deploy_unaffirmed", "pool_deploy_forged", "pool_deploy_own_key" }) |nm| try e.admit(a, &svc, nm, null, &.{ 0, 1, 2 }, &.{});
    try testing.expectEqual(@as(usize, 0), Index.names.items.len);
}

test "swap: a Swap's continuation of a listed pool is its next hop — the pool id copied, swaps + 1, the trade, keyId the spent outpoint, feeVouts; the counters; a continuation naming another next key is not listed" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const e = try Env.init(a);
    defer e.deinit();
    var svc = try e.svc(a);
    const pid = try poolIdOfEnv(a, e);
    const sw = e.f.txid("swap_bsv_in");

    try e.admit(a, &svc, "pool_deploy", null, &.{ 0, 1, 2 }, &.{});
    try e.admit(a, &svc, "swap_bsv_in", null, &.{ 0, 1 }, &.{e.op("pool_deploy", 0)});
    const row = (try latestRow(a, try idx.poolName(a, pid))).?;
    try testing.expectEqualStrings("swap", row.getText("type").?);
    try testing.expectEqualStrings(try opStr(a, sw, 0), row.getText("outpoint").?);
    try testing.expectEqual(@as(u64, 1), row.getUint("swaps").?);
    try testing.expectEqual(@as(u64, vec.pool1.bsv), row.getUint("bsvReserve").?);
    try testing.expectEqual(@as(u64, vec.pool1.tokens), row.getUint("tokenReserve").?);
    try testing.expectEqualStrings(pid, row.getText("keyId").?);
    try testing.expectEqualStrings("buy", row.getText("side").?);
    try testing.expectEqual(@as(u64, vec.swap_bsv_in_tokens_out), row.getUint("amountOut").?);
    try testing.expectEqual(@as(u64, vec.pool0.tokens - vec.pool1.tokens), row.getUint("amountOut").?);
    try testing.expectEqualStrings(vec.commission_pkh, row.getText("commissionPkh").?);
    const fv = row.get("feeVouts").?;
    try testing.expectEqual(@as(u64, 2), fv.getUint("lp").?);
    try testing.expectEqual(@as(u64, 3), fv.getUint("validator").?);
    try testing.expectEqual(@as(u64, 4), fv.getUint("commission").?);
    inline for (.{ "feeLp", "feeValidator", "feeCommission", "amountIn", "payoutPkh" }) |k| try testing.expect(row.get(k) != null);
    // No close: the pool continued.
    try testing.expectEqual(@as(usize, 2), Index.count("amm/pool/"));
    // The live list: the continuation; the counters on its row.
    const all = try e.live(a, &svc);
    try testing.expectEqual(@as(usize, 1), all.len);
    try testing.expectEqualStrings(try opStr(a, sw, 0), all[0].getText("outpoint").?);
    try testing.expectEqual(row.getUint("amountIn").?, all[0].get("lifetime").?.getUint("satsIn").?);
    try testing.expectEqual(@as(u64, 1000), all[0].get("lifetime").?.getUint("lastTradeAt").?);
    const tp = try idx.tokenPrefix(a, e.token);
    try testing.expectEqual(@as(u64, 1), try tradesOf(a, &svc, tp, pid));
    // A re-admission of the Swap: counted once.
    try e.admit(a, &svc, "swap_bsv_in", null, &.{ 0, 1 }, &.{});
    try testing.expectEqual(@as(u64, 1), try tradesOf(a, &svc, tp, pid));

    // A continuation naming another next key (swap_bad_next): not listed; the deploy closes.
    const e2 = try Env.init(a);
    defer e2.deinit();
    var s2 = try e2.svc(a);
    try e2.admit(a, &s2, "pool_deploy", null, &.{ 0, 1, 2 }, &.{});
    try e2.admit(a, &s2, "swap_bad_next", null, &.{ 0, 1 }, &.{e2.op("pool_deploy", 0)});
    try testing.expectEqualStrings("close", (try latestRow(a, try idx.poolName(a, pid))).?.getText("type").?);
    try testing.expectEqual(@as(usize, 0), (try e2.live(a, &s2)).len);
}

test "close and rescind: the LP's Close is the pool's close hop (no output), the validator's Rescind a rescind hop out of the live list; rejected, each is reverted and the pool it spent is live again" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    for ([_]struct { name: []const u8, outs: []const u32, kind: []const u8 }{ .{ .name = "close", .outs = &.{1}, .kind = "close" }, .{ .name = "rescind", .outs = &.{0}, .kind = "rescind" } }) |c| {
        const e = try Env.init(a);
        defer e.deinit();
        var svc = try e.svc(a);
        const pid = try poolIdOfEnv(a, e);
        const sw2 = e.f.txid("swap_tokens_in");
        try e.threeHops(a, &svc);
        try e.admit(a, &svc, c.name, null, c.outs, &.{e.op("swap_tokens_in", 0)});
        const end = (try latestRow(a, try idx.poolName(a, pid))).?;
        try testing.expectEqualStrings(c.kind, end.getText("type").?);
        try testing.expectEqualStrings(&w.header.toHex(e.f.txid(c.name)), end.getText("txid").?);
        try testing.expectEqual(std.mem.eql(u8, c.kind, "rescind"), end.get("outpoint") != null);
        try testing.expectEqual(@as(usize, 0), (try e.live(a, &svc)).len);
        var ch = try e.chain(a);
        const ids = try e.q(a, &.{.{ .key = "poolIds", .value = .{ .array = try a.dupe(Value, &.{.{ .text = pid }}) } }});
        const pos = (try idx.answer(a, &svc, &ch, ids)).freeform.array[0];
        try testing.expectEqualStrings(if (std.mem.eql(u8, c.kind, "rescind")) "rescinded" else "closed", pos.getText("status").?);

        try e.reject(a, &svc, c.name, null);
        const rv = (try latestRow(a, try idx.poolName(a, pid))).?;
        try testing.expectEqualStrings("revert", rv.getText("type").?);
        try testing.expectEqualStrings(try opStr(a, sw2, 0), rv.getText("outpoint").?);
        const back = try e.live(a, &svc);
        try testing.expectEqual(@as(usize, 1), back.len);
        try testing.expectEqualStrings(try opStr(a, sw2, 0), back[0].getText("outpoint").?);
    }
}

test "rejected: a rejected Swap's rows are reverted under the same names — its output {reverted}, its pool's revert row with the pool it spent given back — and its counts unwound; a rejected deploy's revert has no pool; re-admitted, it counts again" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const e = try Env.init(a);
    defer e.deinit();
    var svc = try e.svc(a);
    const pid = try poolIdOfEnv(a, e);
    const sw = e.f.txid("swap_bsv_in");
    const tp = try idx.tokenPrefix(a, e.token);
    try e.admit(a, &svc, "pool_deploy", null, &.{ 0, 1, 2 }, &.{});
    try e.admit(a, &svc, "swap_bsv_in", null, &.{ 0, 1 }, &.{e.op("pool_deploy", 0)});
    try e.reject(a, &svc, "swap_bsv_in", null);
    const out = (try latestRow(a, try idx.outputName(a, sw, 0))).?;
    try testing.expect(out.getBool("reverted").?);
    try testing.expectEqualStrings(pid, out.getText("poolId").?);
    const rv = (try latestRow(a, try idx.poolName(a, pid))).?;
    try testing.expectEqualStrings("revert", rv.getText("type").?);
    try testing.expectEqualStrings(&w.header.toHex(sw), rv.getText("txid").?);
    try testing.expectEqualStrings(pid, rv.getText("outpoint").?);
    try testing.expectEqual(@as(u64, 0), rv.getUint("swaps").?);
    try testing.expectEqualStrings("revert", (try latestRow(a, "amm/hop")).?.getText("type").?);
    try testing.expectEqual(@as(u64, 0), try tradesOf(a, &svc, tp, pid));
    const all = try e.live(a, &svc);
    try testing.expectEqual(@as(usize, 1), all.len);
    try testing.expectEqualStrings(pid, all[0].getText("outpoint").?);
    try testing.expect(all[0].get("lifetime").?.get("lastTradeAt").? == .null);
    // A second rejection: nothing more.
    const before = Index.names.items.len;
    try e.reject(a, &svc, "swap_bsv_in", null);
    try testing.expectEqual(before, Index.names.items.len);
    // Re-admitted (a proof after the rejection): listed and counted again.
    try e.admit(a, &svc, "swap_bsv_in", null, &.{ 0, 1 }, &.{e.op("pool_deploy", 0)});
    try testing.expectEqual(@as(u64, 1), try tradesOf(a, &svc, tp, pid));
    try testing.expectEqualStrings(try opStr(a, sw, 0), (try e.live(a, &svc))[0].getText("outpoint").?);
    // The deploy rejected: the pool is gone, its revert names only the pool.
    try e.reject(a, &svc, "swap_bsv_in", null);
    try e.reject(a, &svc, "pool_deploy", null);
    const gone = (try latestRow(a, try idx.poolName(a, pid))).?;
    try testing.expectEqualStrings("revert", gone.getText("type").?);
    try testing.expect(gone.get("outpoint") == null);
    try testing.expectEqual(@as(usize, 0), (try e.live(a, &svc)).len);
}

test "positions {poolIds}: per pool its state, LP key and the LP fees of its standing Swaps (sats from buys, tokens from sells); closed after the Close; an unknown id closed with zeros; the dropped queries refused" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const e = try Env.init(a);
    defer e.deinit();
    var svc = try e.svc(a);
    var ch = try e.chain(a);
    const pid = try poolIdOfEnv(a, e);
    try e.threeHops(a, &svc);
    const buy = (try latestRow(a, try idx.outputName(a, e.f.txid("swap_bsv_in"), 0))).?;
    const sell = (try latestRow(a, try idx.outputName(a, e.f.txid("swap_tokens_in"), 0))).?;
    const unknown = "a0" ** 32 ++ "_0";
    const ids = try e.q(a, &.{.{ .key = "poolIds", .value = .{ .array = try a.dupe(Value, &.{ .{ .text = pid }, .{ .text = unknown } }) } }});
    const ans = (try idx.answer(a, &svc, &ch, ids)).freeform.array;
    try testing.expectEqualStrings("live", ans[0].getText("status").?);
    try testing.expectEqualStrings(vec.lp, ans[0].getText("lpPubKey").?);
    try testing.expectEqualStrings(try opStr(a, e.f.txid("swap_tokens_in"), 0), ans[0].getText("outpoint").?);
    try testing.expectEqual(buy.getUint("feeLp").?, ans[0].getUint("sats").?);
    try testing.expectEqual(sell.getUint("feeLp").?, ans[0].getUint("tokens").?);
    try testing.expectEqual(@as(u64, 2), ans[0].getUint("swaps").?);
    try testing.expect(ans[0].get("current").?.get("lifetime") != null);
    try testing.expectEqualStrings("closed", ans[1].getText("status").?);
    try testing.expect(ans[1].get("lpPubKey").? == .null);
    try testing.expectEqual(@as(u64, 0), ans[1].getUint("swaps").?);
    try e.admit(a, &svc, "close", null, &.{1}, &.{e.op("swap_tokens_in", 0)});
    try testing.expectEqualStrings("closed", (try idx.answer(a, &svc, &ch, ids)).freeform.array[0].getText("status").?);
    // 0.49.0: the queries nothing reads are gone.
    for ([_][]const u8{ "outpoint", "validatorIdentityKey", "validatorIdentityKeys", "commissionPkhs" }) |k|
        try testing.expectError(error.BadQuery, idx.answer(a, &svc, &ch, try e.q(a, &.{.{ .key = k, .value = .{ .text = "x" } }})));
}

/// `t` (a Swap spending `spent` at input 0) with its commission paid to `pkh` instead: the call's
/// commissionPkh push and the commission output's script replaced (same txid: the hooks take it as
/// given).
fn withCommission(a: Allocator, t: bsvz.transaction.Transaction, pkh: [20]u8) !bsvz.transaction.Transaction {
    const old = unhex(a, vec.commission_pkh);
    const ul = try a.dupe(u8, t.inputs[0].unlocking_script.bytes);
    const at = std.mem.indexOf(u8, ul, &([_]u8{0x14} ++ old[0..20].*)).?;
    @memcpy(ul[at + 1 ..][0..20], &pkh);
    const ins = try a.dupe(bsvz.transaction.Input, t.inputs);
    ins[0].unlocking_script = bsvz.script.Script.init(ul);
    var r = t;
    r.inputs = ins;
    const call = (try pool.parseSwapCall(a, ul)).?;
    const cont = pool.poolOfScript(t.outputs[0].locking_script.bytes).?;
    const l = try pool.feeLayout(call, cont);
    const script = try pool.payoutScript(a, cont.asset_id, l.commission, pkh, call.bsv_in);
    return withOutput(a, r, l.commission_at.?, script);
}

test "fees that are ours: the validator fee when the spent pool names this instance — filed in `bsv` for a buy (key [1, amm pool] under the spent pool's keyId), in `tokens` for a sell with the tally; not ours, nothing filed" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    {
        // Not ours: another identity.
        const e = try Env.init(a);
        defer e.deinit();
        var svc = try e.svc(a);
        idx.self_identity = key(7);
        try e.threeHops(a, &svc);
        try testing.expectEqual(@as(usize, 0), Filed.calls.items.len);
        try testing.expectEqual(@as(u64, 0), try idx.tally(a, &svc, try idx.tokenPrefix(a, e.token)));
    }
    const e = try Env.init(a);
    defer e.deinit();
    var svc = try e.svc(a);
    idx.self_identity = key33(vec.identity);
    const tp = try idx.tokenPrefix(a, e.token);
    try e.threeHops(a, &svc);
    try testing.expectEqual(@as(usize, 2), Filed.calls.items.len);
    const b = try got(0);
    const bsw = e.f.tx("swap_bsv_in");
    try testing.expectEqualSlices(u8, &e.f.txid("swap_bsv_in"), &b.txid);
    try testing.expectEqual(@as(u32, 3), b.vout);
    try testing.expectEqualStrings("bsv", b.basket);
    try testing.expectEqualStrings("amm pool", b.protocol);
    try testing.expectEqual(@as(u64, 1), b.protocol_level);
    try testing.expectEqualStrings(vec.nonce, b.key_id); // the deploy's key: its nonce
    try testing.expectEqualStrings("anyone", b.counterparty);
    _ = bsw;
    const s = try got(1);
    try testing.expectEqualStrings("tokens", s.basket);
    try testing.expectEqualStrings(try opStr(a, e.f.txid("pool_deploy"), 0), s.key_id); // the grandparent: the outpoint before the one the sell spends
    const sell = (try latestRow(a, try idx.outputName(a, e.f.txid("swap_tokens_in"), 0))).?;
    const fee = sell.getUint("feeValidator").?;
    try testing.expectEqual(fee, try idx.tally(a, &svc, tp));
    try testing.expectEqual(@as(u64, sell.get("feeVouts").?.getUint("validator").?), s.vout);
    // Not a commission of ours (the fixture's commission pays another key): no flag.
    try testing.expectEqual(@as(usize, 0), Index.count("amm/commission/"));

    // Our token fee output spent: it leaves the tally; that spend rejected: it is back.
    var bld = bsvz.transaction.Builder.init(a);
    const stx = e.f.tx("swap_tokens_in");
    try bld.addInputFromTx(&stx, s.vout);
    try bld.addOutput(.{ .satoshis = 1, .locking_script = stx.outputs[s.vout].locking_script });
    const sp = try bld.build();
    const raw = try sp.serialize(a);
    const sp_l = try ltx(a, w.beef.txidOf(raw), sp);
    try idx.admitted(a, &svc, e.topic, sp_l, &.{0}, &.{});
    try idx.spent(a, &svc, e.topic, .{ .txid = e.f.txid("swap_tokens_in"), .vout = s.vout }, sp_l);
    try e.commit(&svc);
    try testing.expectEqual(@as(u64, 0), try idx.tally(a, &svc, tp));
    try idx.rejected(a, &svc, e.topic, sp_l);
    try e.commit(&svc);
    try testing.expectEqual(fee, try idx.tally(a, &svc, tp));
    // The sell rejected: its fee is gone from the tally.
    try e.reject(a, &svc, "swap_tokens_in", null);
    try testing.expectEqual(@as(u64, 0), try idx.tally(a, &svc, tp));
}

test "fees that are ours: a commission paying our [1, amm commission] key keyed by the spent pool outpoint — flagged under amm/commission/<op>, filed, a token commission in the tally; a commission to that key for ANOTHER outpoint is not ours; rejected, the flag reverted and the tally back" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const e = try Env.init(a);
    defer e.deinit();
    var svc = try e.svc(a);
    const me = key33(vec.own_key);
    idx.self_identity = me; // not the validator: only the commission can be ours
    const tp = try idx.tokenPrefix(a, e.token);
    const sb = e.f.txid("swap_bsv_in");
    const ours = try pool.commissionPkh(a, me, sb, 0);
    const sell = try withCommission(a, e.f.tx("swap_tokens_in"), ours);
    try e.admit(a, &svc, "pool_deploy", null, &.{ 0, 1, 2 }, &.{});
    try e.admit(a, &svc, "swap_bsv_in", null, &.{ 0, 1 }, &.{e.op("pool_deploy", 0)});
    try e.admit(a, &svc, "swap_tokens_in", sell, &.{ 0, 2, 3, 4 }, &.{e.op("swap_bsv_in", 0)});
    const row = (try latestRow(a, try idx.outputName(a, e.f.txid("swap_tokens_in"), 0))).?;
    const cv: u32 = @intCast(row.get("feeVouts").?.getUint("commission").?);
    const flag = (try latestRow(a, try idx.commissionName(a, e.f.txid("swap_tokens_in"), cv))).?;
    try testing.expectEqualStrings("commission", flag.getText("type").?);
    try testing.expectEqualStrings("token", flag.getText("asset").?);
    try testing.expectEqual(row.getUint("feeCommission").?, flag.getUint("amount").?);
    try testing.expectEqualStrings(&w.header.toHex(e.f.txid("swap_tokens_in")), flag.getText("swap").?);
    try testing.expectEqual(@as(usize, 1), Filed.calls.items.len);
    const f = try got(0);
    try testing.expectEqualStrings("amm commission", f.protocol);
    try testing.expectEqualStrings(try opStr(a, sb, 0), f.key_id);
    try testing.expectEqualStrings("tokens", f.basket);
    try testing.expectEqual(cv, f.vout);
    try testing.expectEqual(row.getUint("feeCommission").?, try idx.tally(a, &svc, tp));
    try e.reject(a, &svc, "swap_tokens_in", sell);
    try testing.expect((try latestRow(a, try idx.commissionName(a, e.f.txid("swap_tokens_in"), cv))).?.getBool("reverted").?);
    try testing.expectEqual(@as(u64, 0), try idx.tally(a, &svc, tp));

    // Our key, but keyed by another outpoint: not ours.
    const e2 = try Env.init(a);
    defer e2.deinit();
    var s2 = try e2.svc(a);
    idx.self_identity = me;
    const wrong = try withCommission(a, e2.f.tx("swap_tokens_in"), try pool.commissionPkh(a, me, sb, 1));
    try e2.admit(a, &s2, "pool_deploy", null, &.{ 0, 1, 2 }, &.{});
    try e2.admit(a, &s2, "swap_bsv_in", null, &.{ 0, 1 }, &.{e2.op("pool_deploy", 0)});
    try e2.admit(a, &s2, "swap_tokens_in", wrong, &.{ 0, 2, 3, 4 }, &.{e2.op("swap_bsv_in", 0)});
    try testing.expectEqual(@as(usize, 0), Index.count("amm/commission/"));
    try testing.expectEqual(@as(usize, 0), Filed.calls.items.len);
}

/// The topic's own fact that `op` is spent by `by` (skein-overlay `<app>/topic/output/<op>`), as the
/// engine emits it at `by`'s admission (after the hooks: in the index from the next step on).
fn topicSpent(a: Allocator, op: lookup.Outpoint, by: [32]u8) !void {
    const name = try std.fmt.allocPrint(a, "amm/topic/output/{s}.{d}", .{ &w.header.toHex(op.txid), op.vout });
    try Index.put(name, .{ .map = try a.dupe(Entry, &.{
        .{ .key = "event", .value = .{ .text = name } },
        .{ .key = "state", .value = .{ .text = "spent" } },
        .{ .key = "spentBy", .value = .{ .text = try a.dupe(u8, &w.header.toHex(by)) } },
    }) });
}

test "order rule (skein-overlay 0.26.0): a proven B spending the pool output a held unmined A spent — B's admitted + spent heard BEFORE A's rejected — the pool continues at B's output, its trade counted once; A's rejection (nothing given back) unwinds only A; a fee output of ours spent by C then by a proven D leaves the tally once" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const e = try Env.init(a);
    defer e.deinit();
    var svc = try e.svc(a);
    idx.self_identity = key33(vec.identity);
    const pid = try poolIdOfEnv(a, e);
    const tp = try idx.tokenPrefix(a, e.token);
    try e.admit(a, &svc, "pool_deploy", null, &.{ 0, 1, 2 }, &.{});
    try e.admit(a, &svc, "swap_bsv_in", null, &.{ 0, 1 }, &.{e.op("pool_deploy", 0)});
    // A and B: two sells spending the buy's pool output (one transaction body under two txids: the
    // hooks take a transaction as given).
    const sell = e.f.tx("swap_tokens_in");
    const ta: [32]u8 = .{0xa1} ** 32;
    const tb: [32]u8 = .{0xb2} ** 32;
    const la = try ltx(a, ta, sell);
    const lb = try ltx(a, tb, sell);
    const xp = e.op("swap_bsv_in", 0);
    // A, held unmined: admitted; the topic's fact (its step's) says the pool output is spent by A.
    try idx.admitted(a, &svc, e.topic, la, &.{ 0, 2, 3, 4 }, &.{});
    try idx.spent(a, &svc, e.topic, xp, la);
    try e.commit(&svc);
    try topicSpent(a, xp, ta);
    const fee = (try latestRow(a, try idx.outputName(a, ta, 0))).?.getUint("feeValidator").?;
    try testing.expectEqual(fee, try idx.tally(a, &svc, tp));
    // B, proven, heard before A's rejection: its continuation of the same pool output lists.
    try idx.admitted(a, &svc, e.topic, lb, &.{ 0, 2, 3, 4 }, &.{});
    try idx.spent(a, &svc, e.topic, xp, lb);
    try e.commit(&svc);
    try testing.expectEqual(2 * fee, try idx.tally(a, &svc, tp));
    try testing.expectEqual(@as(u64, 3), try tradesOf(a, &svc, tp, pid));
    // A rejected (double spent, by B): nothing given back — B consumes the pool output.
    svc.given_back = &.{};
    try idx.rejected(a, &svc, e.topic, la);
    try e.commit(&svc);
    try testing.expectEqual(fee, try idx.tally(a, &svc, tp)); // B's fee only
    try testing.expectEqual(@as(u64, 2), try tradesOf(a, &svc, tp, pid)); // buy + B
    const rv = (try latestRow(a, try idx.poolName(a, pid))).?;
    try testing.expectEqualStrings("revert", rv.getText("type").?);
    try testing.expectEqualStrings(try opStr(a, tb, 0), rv.getText("outpoint").?); // the pool as it stands: B's
    const all = try e.live(a, &svc);
    try testing.expectEqual(@as(usize, 1), all.len);
    try testing.expectEqualStrings(try opStr(a, tb, 0), all[0].getText("outpoint").?);
    try testing.expectEqual(@as(usize, 3), (try idx.standing(a, &svc, pid)).len); // deploy, buy, B
    try testing.expect((try latestRow(a, try idx.outputName(a, ta, 0))).?.getBool("reverted").?);
    try testing.expect((try latestRow(a, try idx.outputName(a, tb, 0))).?.get("reverted") == null);

    // B's fee output (ours) spent by C, then by a proven D: it leaves the tally once; C's rejection
    // (nothing given back: D consumes it) does not bring it back.
    const fv: u32 = @intCast((try latestRow(a, try idx.outputName(a, tb, 0))).?.get("feeVouts").?.getUint("validator").?);
    var bld = bsvz.transaction.Builder.init(a);
    try bld.addInputFromTx(&sell, fv);
    try bld.addOutput(.{ .satoshis = 1, .locking_script = sell.outputs[fv].locking_script });
    const body = try bld.build();
    @constCast(body.inputs)[0].previous_outpoint.txid.bytes = tb; // it spends B's output (B's txid is the test's own)
    const fx: lookup.Outpoint = .{ .txid = tb, .vout = fv };
    const lc = try ltx(a, .{0xc3} ** 32, body);
    const ld = try ltx(a, .{0xd4} ** 32, body);
    try idx.spent(a, &svc, e.topic, fx, lc);
    try e.commit(&svc);
    try topicSpent(a, fx, lc.txid);
    try testing.expectEqual(@as(u64, 0), try idx.tally(a, &svc, tp));
    try idx.spent(a, &svc, e.topic, fx, ld);
    try e.commit(&svc);
    try testing.expectEqual(@as(u64, 0), try idx.tally(a, &svc, tp));
    try idx.rejected(a, &svc, e.topic, lc);
    try e.commit(&svc);
    try testing.expectEqual(@as(u64, 0), try idx.tally(a, &svc, tp));
    // Had C's coin been given back (no winner), it would be ours again.
    svc.given_back = null;
    try idx.rejected(a, &svc, e.topic, lc);
    try e.commit(&svc);
    try testing.expectEqual(fee, try idx.tally(a, &svc, tp));
}

test "F3 (David 2026-11-09): a refused filing is an error like any other — the admission hook fails (no skip, no event of it)" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const e = try Env.init(a);
    defer e.deinit();
    var svc = try e.svc(a);
    idx.self_identity = key33(vec.identity);
    try e.admit(a, &svc, "pool_deploy", null, &.{ 0, 1, 2 }, &.{});
    Filed.refuse = true;
    try testing.expectError(error.CallRefused, idx.admitted(a, &svc, e.topic, try ltx(a, e.f.txid("swap_bsv_in"), e.f.tx("swap_bsv_in")), &.{ 0, 1 }, &.{}));
    Filed.refuse = false;
}

test "audit (0.50.0): a BEEF carrying a pool deploy and a Swap on it, admitted in ONE overlay step — the Swap's hop is recognised, its validator fee filed, the tally right (skein 0.8.0: a step sees its own events; on 0.49.0 the hop was lost)" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const e = try Env.init(a);
    defer e.deinit();
    var svc = try e.svc(a);
    idx.self_identity = key33(vec.identity);
    const pid = try poolIdOfEnv(a, e);
    const tp = try idx.tokenPrefix(a, e.token);
    // One step: the deploy's hooks, then the buy's, then the sell's.
    try idx.admitted(a, &svc, e.topic, try ltx(a, e.f.txid("pool_deploy"), e.f.tx("pool_deploy")), &.{ 0, 1, 2 }, &.{});
    const buy = try ltx(a, e.f.txid("swap_bsv_in"), e.f.tx("swap_bsv_in"));
    try idx.admitted(a, &svc, e.topic, buy, &.{ 0, 1 }, &.{});
    try idx.spent(a, &svc, e.topic, e.op("pool_deploy", 0), buy);
    const sell = try ltx(a, e.f.txid("swap_tokens_in"), e.f.tx("swap_tokens_in"));
    try idx.admitted(a, &svc, e.topic, sell, &.{ 0, 2, 3, 4 }, &.{});
    try idx.spent(a, &svc, e.topic, e.op("swap_bsv_in", 0), sell);
    const hops = try idx.standing(a, &svc, pid);
    try testing.expectEqual(@as(usize, 3), hops.len);
    for ([_][]const u8{ "deploy", "swap", "swap" }, hops) |k, h| try testing.expectEqualStrings(k, h.getText("type").?);
    try testing.expectEqual(@as(u64, 2), try tradesOf(a, &svc, tp, pid));
    try testing.expectEqual(@as(usize, 2), Filed.calls.items.len); // the buy's fee under bsv, the sell's under tokens
    try testing.expectEqualStrings("bsv", Filed.calls.items[0].basket);
    try testing.expectEqualStrings("tokens", Filed.calls.items[1].basket);
    try testing.expectEqual(hops[2].getUint("feeValidator").?, try idx.tally(a, &svc, tp));
    // No close: each spender continued the pool.
    try testing.expectEqual(@as(usize, 3), Index.count("amm/pool/"));
    try testing.expectEqualStrings(try opStr(a, e.f.txid("swap_tokens_in"), 0), (try e.live(a, &svc))[0].getText("outpoint").?);
}

/// The chain app's fact about an output (skein-chain 0.10.0 `chain/output/<txid>.<vout>`).
fn chainOutput(a: Allocator, txid: [32]u8, vout: u32, state: []const u8, by: [32]u8) !void {
    const name = try std.fmt.allocPrint(a, "chain/output/{s}.{d}", .{ &w.header.toHex(txid), vout });
    try Index.put(name, .{ .map = try a.dupe(Entry, &.{
        .{ .key = "event", .value = .{ .text = name } },
        .{ .key = "state", .value = .{ .text = state } },
        .{ .key = "spentBy", .value = .{ .text = try a.dupe(u8, &w.header.toHex(by)) } },
    }) });
}

test "audit (0.50.0): the answers read spent-ness from the chain — a pool output a transaction the topic never admitted spent (no `spent` hook fires) is not live, and is closed in {poolIds}; rolled back, it is live again" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const e = try Env.init(a);
    defer e.deinit();
    var svc = try e.svc(a);
    var ch = try e.chain(a);
    const pd = e.f.txid("pool_deploy");
    const pid = try poolIdOfEnv(a, e);
    try e.admit(a, &svc, "pool_deploy", null, &.{ 0, 1, 2 }, &.{});
    try testing.expectEqual(@as(usize, 1), (try e.live(a, &svc)).len);
    const other: [32]u8 = .{0x99} ** 32;
    try chainOutput(a, pd, 0, "spent", other);
    try testing.expectEqual(@as(usize, 0), (try e.live(a, &svc)).len);
    const ids = try e.q(a, &.{.{ .key = "poolIds", .value = .{ .array = try a.dupe(Value, &.{.{ .text = pid }}) } }});
    try testing.expectEqualStrings("closed", (try idx.answer(a, &svc, &ch, ids)).freeform.array[0].getText("status").?);
    try chainOutput(a, pd, 0, "rolledBack", other);
    try testing.expectEqual(@as(usize, 1), (try e.live(a, &svc)).len);
    try testing.expectEqualStrings("live", (try idx.answer(a, &svc, &ch, ids)).freeform.array[0].getText("status").?);
}
