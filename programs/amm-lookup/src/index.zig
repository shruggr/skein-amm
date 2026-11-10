//! The AMM pool RECOGNIZER (served as `ls_amm_<assetId>`, 0.21.0; 0.49.0: a recognizer that emits
//! events, agent-env plans/research/plan-pass-recognizers-2026-11-09.md, David 2026-11-09 "INDEXES vs
//! SUMMARIES"). It is called through skein-overlay's lookup hooks (`admitted`, `spent`, `rejected`,
//! in the step that admits or unwinds a transaction) and recognizes what an admitted transaction
//! is to the AMM: a pool's deploy, a Swap's continuation, the LP's Close, the validator's Rescind.
//! It keeps NO index of its own: what it recognizes it EMITS as named events (the kernel indexes
//! them by name; the latest under a name is the current state, the list is the history), and it
//! reads earlier facts back from that index in its step (skein `events`). It STORES only running
//! summaries: the counters (0.47.0) and the token fee tally (0.49.0).
//!
//! The listing (skein-amm 0.13.0, David Case 2026-10-22; docs/AMM.md "The listing"): a pool output
//! is listed when
//!   (a) it attests itself (pool.zig `affirmed`): not rescinded, its ValidatorPubKey the
//!       ValidatorIdentity's child keyed by its Nonce (derived here, not trusted), and its
//!       Affirmation that key's signature of the pool's parameters (message v2) — a deploy; or
//!   (b) its transaction calls Swap on a listed pool of this token (the pool input's method, the
//!       unlocking script's last push) and its ValidatorPubKey is the identity's child keyed by
//!       that spent outpoint (pool.zig `nextKeyOf`) — a Swap's continuation.
//! A validator's Rescind's output (ValidatorPubKey cleared) is recorded as a `rescind` hop, never a
//! live listing. A listed pool's output spent with no listed continuation is the LP's Close.
//!
//! THE EVENTS (`<app>` the app the engine runs in, `amm` on a token instance; names of outpoints in
//! the overlay's own form `<txid hex>.<vout>`, a pool id `<txid hex>_<vout>`, the deploy outpoint):
//!
//!   <app>/pool/<poolId>            one per hop of the pool: THE HOP ROW (`hopRow`), or a revert row
//!   <app>/output/<txid>.<vout>     the hop row of the listed pool output it names; `{type: "revert",
//!                                  reverted: true, txid, poolId, at}` when its transaction is unwound
//!   <app>/hop                      every row of every `<app>/pool/…`, again (the instance-wide order)
//!   <app>/commission/<txid>.<vout> a Swap's commission output that pays THIS instance's commission
//!                                  key (the settled history flag): `{type: "commission", swap,
//!                                  poolId, asset: "bsv"|"token", amount, at}`; `{…, reverted: true}`
//!                                  when the Swap is unwound
//!
//! THE HOP ROW (text and numbers only): `{type: deploy|swap|close|rescind, txid, tokenId, poolId,
//! outpoint?, bsvReserve?, tokenReserve?, lpPubKey, liquidityFeeBps, validationFeeBps,
//! commissionBps, validatorIdentityKey, at, swaps, keyId?}` — `at` the admitting step's time in ms,
//! `swaps` the pool's Swap count (0 the deploy; a rescind's the next count), `keyId` the key id the
//! hop's ValidatorPubKey is derived under (a deploy's its nonce in hex, a Swap continuation's the
//! spent outpoint `<txid>_<vout>`; none for a close or a rescind); a Swap's adds `side, amountIn,
//! amountOut, feeLp, feeValidator, feeCommission, commissionPkh, payoutPkh, feeVouts: {lp?,
//! validator?, commission?}` (the fee outputs' indices). A revert row: `{type: "revert", txid: <the
//! unwound transaction>, tokenId, poolId, at}` and, when the pool is still there after the undo, the
//! pool's fields as they stand (`outpoint, bsvReserve, tokenReserve, lpPubKey, the three rates,
//! validatorIdentityKey, swaps, keyId?`).
//!
//! FEES THAT ARE OURS (David 2026-11-09, point 8, "OURS FOR TOKEN FEES"): at a Swap's admission, the
//! validator fee is ours when the spent pool names this instance as its validator; the commission is
//! ours when its output pays our `[1, "amm commission"]` key keyed by the pool outpoint the Swap
//! spends. Each is FILED with the wallet app (`fileWithWallet`: the seam) — a BSV fee (a buy's) in
//! the basket `bsv`, a token fee (a sell's) in the basket `tokens` — and a token fee is added to the
//! token tally (`counters`, `f ‖ tp`): a running total adjusted one output at a time in `admitted`,
//! `spent` and `rejected`.
const std = @import("std");
const w = @import("chain");
const lookup = @import("lookup");
const pool = @import("pool");
const color = @import("1color");

const bsv21 = pool.bsv21;
const names = color.name;
const cbor = w.cbor;
const Value = cbor.Value;
const Entry = cbor.Entry;
const Allocator = std.mem.Allocator;
const Service = lookup.Service;
const Chain = lookup.Chain;
const eql = std.mem.eql;

/// The program's state head: `<app>/ls_amm` (skein-overlay 0.11.0), whatever name the service is
/// served as (`ls_amm_<assetId>`). It holds the summaries only (0.49.0).
pub const service_name = "ls_amm";

/// The stored summaries (0.49.0: the index maps `pools`, `spentPools`, `history`, `closes`,
/// `trades` and `byCommission` are gone; what they found is in the events). `counters`: `p ‖ tp ‖
/// poolId` the pool's, `t ‖ tp` the token's, `v ‖ tp ‖ validator identity` the validator's (0.47.0),
/// `f ‖ tp` the token fee tally (0.49.0).
pub const maps = [_][]const u8{"counters"};

// ---------------------------------------------------------------- the step's context

/// The step's time (ms): the in-VM call input's `now`, set by main.zig. A row's `at`.
pub var now_ms: u64 = 0;

/// The app the engine runs in (the hook's `app`): every event name's first segment.
pub var app: []const u8 = "amm";

/// This instance's identity key (the call input's `self.identity`), for the fees that are ours;
/// null: no fee is ours.
pub var self_identity: ?[33]u8 = null;

/// A read of the kernel's event index (skein docs/VM.md "The event index"): `{name | prefix,
/// after?, limit?, latest?}` → `[{name, place, cid, record}]`, oldest first. main.zig sets it to
/// skein-sdk `sk.events`; the tests to an index of their own. In a step it sees what earlier steps
/// emitted, never this step's own emits.
pub const Reader = *const fn (Allocator, Value) anyerror!Value;
pub var reader: Reader = noEvents;

fn noEvents(_: Allocator, _: Value) anyerror!Value {
    return .{ .array = &.{} };
}

// ---------------------------------------------------------------- the wallet filing (THE SEAM)

/// The two baskets of an instance (David 2026-11-09 "BASKETS PER INSTANCE": one BSV, one token),
/// named in this one place. NOT FINAL names.
pub const basket_bsv = "bsv";
pub const basket_tokens = "tokens";

/// One output filed with the wallet app as ours: its outpoint, its derivation (the invoice:
/// protocol, key ID, counterparty), its basket, its satoshis.
pub const Filing = struct {
    txid: [32]u8,
    vout: u32,
    protocol_level: u8,
    protocol: []const u8,
    key_id: []const u8,
    counterparty: []const u8,
    basket: []const u8,
    satoshis: u64,
};

/// The wallet app's `file` (shruggr/skein-wallet 0.1.0): `{outpoint: "<txid>.<vout>", protocol:
/// [level, name], keyID, counterparty}` under the basket name, by the kernel's NAMED call
/// (`callAs(<wallet/app programs.wallet>, "file", <basket>, arg)`; the kernel checks the basket
/// against this app's approved `calls.wallet`). main.zig sets it; the tests set a stand-in.
pub const Filer = *const fn (a: Allocator, basket: []const u8, arg: Value) anyerror!void;
pub var filer: Filer = noFiler;

fn noFiler(_: Allocator, _: []const u8, _: Value) anyerror!void {
    return error.NoWalletFiler;
}

/// File `f` with the wallet app under its basket. THE ONE SEAM to the wallet.
/// RULE (F3, David 2026-11-09): no special handling for a refused filing. An app's protocols and
/// baskets are approved together at install, so a refusal means a broken install; admitting swaps
/// while earning unrecorded fees is worse than stopping. A refused or failed `file` is an error like
/// any other: it is returned and the hook's step fails (no skip, no `unfiled` event).
pub fn fileWithWallet(a: Allocator, f: Filing) !void {
    try filer(a, f.basket, .{ .map = try a.dupe(Entry, &.{
        .{ .key = "outpoint", .value = .{ .text = try std.fmt.allocPrint(a, "{s}.{d}", .{ &w.header.toHex(f.txid), f.vout }) } },
        .{ .key = "protocol", .value = .{ .array = try a.dupe(Value, &.{ .{ .uint = f.protocol_level }, .{ .text = f.protocol } }) } },
        .{ .key = "keyID", .value = .{ .text = f.key_id } },
        .{ .key = "counterparty", .value = .{ .text = f.counterparty } },
    }) });
}

// ---------------------------------------------------------------- names

fn cat(a: Allocator, parts: []const []const u8) ![]u8 {
    return std.mem.concat(a, u8, parts);
}

/// The topic a query's token id names (skein-1color's name.zig `tokenIdOfString`: `<txid>`,
/// `<txid>_<vout>` or `<txid>.<vout>`; the topic `tm_color_<txid>_<vout>` (BRC-207),
/// `tm_color_<txid>_0` for output 0 in any form).
pub fn topicOf(a: Allocator, token_id: []const u8) ![]const u8 {
    const id = names.tokenIdOfString(token_id) orelse return error.BadQuery;
    var buf: [names.max_topic_len]u8 = undefined;
    return a.dupe(u8, names.topicName(&buf, id));
}

/// The token a topic name carries, as the rules take it (skein-1color's token.zig); null for
/// a topic that is not a token's (`tm_color`, `tm_demo`).
pub fn tokenIdOf(topic: []const u8) ?bsv21.TokenId {
    return color.token.tokenIdOf(topic);
}

/// A token's id as the rows carry it (`<txid>_<vout>`, skein-1color `tokenIdText`), from its topic.
fn tokenTextOf(a: Allocator, topic: []const u8) ![]const u8 {
    const id = names.tokenIdOf(topic) orelse return error.BadTopic;
    var buf: [names.max_suffix_len]u8 = undefined;
    return a.dupe(u8, names.tokenIdText(&buf, id));
}

/// The key prefix of a topic's summaries: len ‖ topic (skein-sdk `store.nameKey`).
fn topicPrefix(a: Allocator, topic: []const u8) ![]u8 {
    return w.store.nameKey(a, topic, &.{});
}

/// The summaries' key prefix of a token, from its token id.
pub fn tokenPrefix(a: Allocator, token_id: []const u8) ![]u8 {
    return topicPrefix(a, try topicOf(a, token_id));
}

pub fn hexOf(a: Allocator, bytes: []const u8) ![]const u8 {
    const out = try a.alloc(u8, bytes.len * 2);
    _ = std.fmt.bufPrint(out, "{x}", .{bytes}) catch unreachable;
    return out;
}

fn outpointStr(a: Allocator, txid: [32]u8, vout: u32) ![]const u8 {
    return std.fmt.allocPrint(a, "{s}_{d}", .{ &w.header.toHex(txid), vout });
}

pub const ParsedOutpoint = struct { txid: [32]u8, vout: u32 };

/// `<txid>_<vout>` (a pool id's form).
pub fn parseOutpoint(text: []const u8) !ParsedOutpoint {
    const at = std.mem.indexOfScalar(u8, text, '_') orelse return error.BadQuery;
    return .{
        .txid = w.header.fromHex(text[0..at]) catch return error.BadQuery,
        .vout = std.fmt.parseInt(u32, text[at + 1 ..], 10) catch return error.BadQuery,
    };
}

/// A pool id's text (0.40.0): the deploy's outpoint, `<txid>_<vout>`, from its outpoint key.
pub fn poolIdText(a: Allocator, pid: []const u8) ![]const u8 {
    const op = try w.store.outpointOf(pid);
    return outpointStr(a, op.txid, op.vout);
}

/// `<app>/pool/<poolId>`.
pub fn poolName(a: Allocator, pid_text: []const u8) ![]const u8 {
    return std.fmt.allocPrint(a, "{s}/pool/{s}", .{ app, pid_text });
}

/// `<app>/output/<txid hex>.<vout>` (skein-overlay's `topicOutputName` form).
pub fn outputName(a: Allocator, txid: [32]u8, vout: u32) ![]const u8 {
    return std.fmt.allocPrint(a, "{s}/output/{s}.{d}", .{ app, &w.header.toHex(txid), vout });
}

/// `<app>/hop`.
pub fn hopName(a: Allocator) ![]const u8 {
    return std.fmt.allocPrint(a, "{s}/hop", .{app});
}

/// `<app>/commission/<txid hex>.<vout>`.
pub fn commissionName(a: Allocator, txid: [32]u8, vout: u32) ![]const u8 {
    return std.fmt.allocPrint(a, "{s}/commission/{s}.{d}", .{ app, &w.header.toHex(txid), vout });
}

// ---------------------------------------------------------------- reading the event index

/// The records under exactly `name`, oldest first.
pub fn recordsOf(a: Allocator, name: []const u8) ![]const Value {
    const ans = try reader(a, .{ .map = try a.dupe(Entry, &.{.{ .key = "name", .value = .{ .text = name } }}) });
    return recordsIn(a, ans);
}

fn recordsIn(a: Allocator, ans: Value) ![]const Value {
    if (ans != .array) return error.BadEvents;
    const out = try a.alloc(Value, ans.array.len);
    for (ans.array, out) |x, *o| o.* = x.get("record") orelse return error.BadEvents;
    return out;
}

/// The latest record under exactly `name`, or null.
pub fn latestOf(a: Allocator, name: []const u8) !?Value {
    const ans = try reader(a, .{ .map = try a.dupe(Entry, &.{
        .{ .key = "name", .value = .{ .text = name } },
        .{ .key = "latest", .value = .{ .boolean = true } },
    }) });
    const rs = try recordsIn(a, ans);
    return if (rs.len == 0) null else rs[rs.len - 1];
}

/// The latest record of every name under `prefix` (one per name, in name order).
fn latestUnder(a: Allocator, prefix: []const u8) ![]const Value {
    return recordsIn(a, try reader(a, .{ .map = try a.dupe(Entry, &.{
        .{ .key = "prefix", .value = .{ .text = prefix } },
        .{ .key = "latest", .value = .{ .boolean = true } },
    }) }));
}

fn isReverted(rec: Value) bool {
    return rec.getBool("reverted") orelse false;
}

/// The listed pool output's hop row at txid:vout (the latest `<app>/output/…`, not reverted), or null.
pub fn outputRow(a: Allocator, txid: [32]u8, vout: u32) !?Value {
    const r = (try latestOf(a, try outputName(a, txid, vout))) orelse return null;
    return if (isReverted(r)) null else r;
}

// ---------------------------------------------------------------- recognising a pool output

/// A pool output's fields, as pool.zig parses them, plus where it is.
pub const PoolFields = struct {
    txid: [32]u8,
    vout: u32,
    bsv_reserve: u64,
    token_reserve: u64,
    liquidity_fee_bps: i64,
    validation_fee_bps: i64,
    commission_bps: i64,
    lp: [33]u8,
    identity: [33]u8,
    /// The pool's id (0.40.0): its deploy's outpoint key; copied forward at each Swap.
    pool_id: [36]u8,
    /// The pool's Swaps: 0 at the deploy, +1 per Swap (0.40.0).
    swaps: u64 = 0,
    /// The key id its ValidatorPubKey is derived under (0.49.0); null for a rescind's.
    key_id: ?[]const u8 = null,
};

/// Whether output `vout`'s script is a pool: a BRC-162 value output whose lock parses as the
/// Pool template. Recognition only: whether it passes the pool checks is `judge`'s.
pub fn poolAt(txid: [32]u8, vout: u32, script: []const u8, satoshis: u64) ?PoolFields {
    const p = pool.poolOfScript(script) orelse return null;
    return .{
        .txid = txid,
        .vout = vout,
        .bsv_reserve = satoshis,
        .token_reserve = p.token_reserve,
        .liquidity_fee_bps = p.lp_fee_bps,
        .validation_fee_bps = p.validator_fee_bps,
        .commission_bps = p.commission_bps,
        .lp = p.lp,
        .identity = p.identity,
        .pool_id = w.store.outpointKey(txid, vout),
    };
}

/// The pool checks (`pool.check`) on the transaction's admitted outputs of this token. Null when
/// the pools in `tx` pass (or there are none); the violation otherwise.
pub fn judge(a: Allocator, id: bsv21.TokenId, tx: lookup.Tx, outputs_to_admit: []const u32) !?pool.Violation {
    var j_outs: std.ArrayList(bsv21.Indexed) = .empty;
    for (outputs_to_admit) |vout| {
        if (vout >= tx.tx.outputs.len) return error.BadHook;
        const t = (try bsv21.tokenOf(a, id, tx.txid, vout, tx.tx.outputs[vout].locking_script.bytes)) orelse continue;
        try j_outs.append(a, .{ .index = vout, .token = t });
    }
    return pool.check(.{ .ok = true, .inputs = &.{}, .outputs = j_outs.items });
}

fn hex33(rec: Value, k: []const u8) ![33]u8 {
    const t = rec.getText(k) orelse return error.BadEvent;
    var out: [33]u8 = undefined;
    if (t.len != 66) return error.BadEvent;
    _ = std.fmt.hexToBytes(&out, t) catch return error.BadEvent;
    return out;
}

fn intOf(v: ?Value) !i64 {
    const x = v orelse return error.BadEvent;
    return switch (x) {
        .uint => |n| std.math.cast(i64, n) orelse error.BadEvent,
        .nint => |n| -1 - (std.math.cast(i64, n) orelse return error.BadEvent),
        else => error.BadEvent,
    };
}

fn intValue(v: i64) Value {
    return if (v >= 0) .{ .uint = @intCast(v) } else .{ .nint = @intCast(-1 - v) };
}

/// A hop row with an outpoint, read back as the pool's fields.
pub fn fieldsOfRow(rec: Value) !PoolFields {
    const op = try parseOutpoint(rec.getText("outpoint") orelse return error.BadEvent);
    const pid = try parseOutpoint(rec.getText("poolId") orelse return error.BadEvent);
    return .{
        .txid = op.txid,
        .vout = op.vout,
        .bsv_reserve = rec.getUint("bsvReserve") orelse return error.BadEvent,
        .token_reserve = rec.getUint("tokenReserve") orelse return error.BadEvent,
        .liquidity_fee_bps = try intOf(rec.get("liquidityFeeBps")),
        .validation_fee_bps = try intOf(rec.get("validationFeeBps")),
        .commission_bps = try intOf(rec.get("commissionBps")),
        .lp = try hex33(rec, "lpPubKey"),
        .identity = try hex33(rec, "validatorIdentityKey"),
        .pool_id = w.store.outpointKey(pid.txid, pid.vout),
        .swaps = rec.getUint("swaps") orelse return error.BadEvent,
        .key_id = rec.getText("keyId"),
    };
}

fn kindOf(rec: Value) []const u8 {
    return rec.getText("type") orelse "";
}

/// The input of `tx` that spends txid:vout, or null.
fn inputSpending(tx: lookup.Tx, txid: [32]u8, vout: u32) ?usize {
    for (tx.tx.inputs, 0..) |in, i| {
        const op = in.previous_outpoint;
        if (op.index == vout and eql(u8, &op.txid.bytes, &txid)) return i;
    }
    return null;
}

/// Why output 0 of `tx` continues the listed pool `was` (its hop row, at was_txid:was_vout) — the
/// listing rule (b), or a validator's Rescind of it (same LpPubKey, the Rescind method) — or null.
/// The same test `admitted` lists by, so `spent` can tell a Close without reading what this step
/// emitted (the step's own emits are not in the index it reads, plan M2).
const Continuation = enum { swap, rescind };
fn continuationOf(a: Allocator, tx: lookup.Tx, was: Value, was_txid: [32]u8, was_vout: u32) !?Continuation {
    if (tx.tx.outputs.len == 0) return null;
    const p = pool.poolOfScript(tx.tx.outputs[0].locking_script.bytes) orelse return null;
    const i = inputSpending(tx, was_txid, was_vout) orelse return null;
    const method = pool.methodOf(tx.tx.inputs[i].unlocking_script.bytes) orelse return null;
    if (p.rescinded()) {
        if (method != .rescind) return null;
        if (!eql(u8, &p.lp, &try hex33(was, "lpPubKey"))) return null;
        return .rescind;
    }
    if (method != .swap) return null;
    if (eql(u8, kindOf(was), "rescind")) return null;
    if (!pool.nextKeyOf(a, p, try hex33(was, "validatorIdentityKey"), was_txid, was_vout)) return null;
    return .swap;
}

// ---------------------------------------------------------------- the rows and their events

/// THE HOP ROW's pool part: `poolId`, then (with an output) `outpoint`, `bsvReserve`,
/// `tokenReserve`, then `lpPubKey`, the three rates, `validatorIdentityKey`, `at`, `swaps`, `keyId?`.
fn poolPart(a: Allocator, es: *std.ArrayList(Entry), pf: PoolFields, with_output: bool, at: u64) !void {
    try es.append(a, .{ .key = "poolId", .value = .{ .text = try poolIdText(a, &pf.pool_id) } });
    if (with_output) try es.appendSlice(a, &.{
        .{ .key = "outpoint", .value = .{ .text = try outpointStr(a, pf.txid, pf.vout) } },
        .{ .key = "bsvReserve", .value = .{ .uint = pf.bsv_reserve } },
        .{ .key = "tokenReserve", .value = .{ .uint = pf.token_reserve } },
    });
    try es.appendSlice(a, &.{
        .{ .key = "lpPubKey", .value = .{ .text = try hexOf(a, &pf.lp) } },
        .{ .key = "liquidityFeeBps", .value = intValue(pf.liquidity_fee_bps) },
        .{ .key = "validationFeeBps", .value = intValue(pf.validation_fee_bps) },
        .{ .key = "commissionBps", .value = intValue(pf.commission_bps) },
        .{ .key = "validatorIdentityKey", .value = .{ .text = try hexOf(a, &pf.identity) } },
        .{ .key = "at", .value = .{ .uint = at } },
        .{ .key = "swaps", .value = .{ .uint = pf.swaps } },
    });
    if (with_output) if (pf.key_id) |k| try es.append(a, .{ .key = "keyId", .value = .{ .text = k } });
}

/// A hop row's head: `type`, `txid`, `tokenId`.
fn rowHead(a: Allocator, es: *std.ArrayList(Entry), kind: []const u8, txid: [32]u8, token: []const u8) !void {
    try es.appendSlice(a, &.{
        .{ .key = "type", .value = .{ .text = kind } },
        .{ .key = "txid", .value = .{ .text = try a.dupe(u8, &w.header.toHex(txid)) } },
        .{ .key = "tokenId", .value = .{ .text = token } },
    });
}

/// The trade part of a Swap's row (0.36.0 / 0.41.0 field names; 0.49.0 `feeVouts`).
const Trade = struct {
    bsv_in: bool,
    amount_in: u64,
    amount_out: u64,
    lp: u64,
    validator: u64,
    commission: u64,
    commission_pkh: [20]u8,
    payout_pkh: []const u8,
    lp_at: ?usize,
    validator_at: ?usize,
    commission_at: ?usize,
};

fn hopRow(a: Allocator, kind: []const u8, txid: [32]u8, token: []const u8, pf: PoolFields, with_output: bool, trade: ?Trade) !Value {
    var es: std.ArrayList(Entry) = .empty;
    try rowHead(a, &es, kind, txid, token);
    try poolPart(a, &es, pf, with_output, now_ms);
    if (trade) |t| {
        try es.appendSlice(a, &.{
            .{ .key = "side", .value = .{ .text = if (t.bsv_in) "buy" else "sell" } },
            .{ .key = "amountIn", .value = .{ .uint = t.amount_in } },
            .{ .key = "amountOut", .value = .{ .uint = t.amount_out } },
            .{ .key = "feeLp", .value = .{ .uint = t.lp } },
            .{ .key = "feeValidator", .value = .{ .uint = t.validator } },
            .{ .key = "feeCommission", .value = .{ .uint = t.commission } },
            .{ .key = "commissionPkh", .value = .{ .text = try hexOf(a, &t.commission_pkh) } },
            .{ .key = "payoutPkh", .value = .{ .text = try hexOf(a, t.payout_pkh) } },
        });
        var fv: std.ArrayList(Entry) = .empty;
        inline for (.{ .{ "lp", t.lp_at }, .{ "validator", t.validator_at }, .{ "commission", t.commission_at } }) |x|
            if (x[1]) |at| try fv.append(a, .{ .key = x[0], .value = .{ .uint = at } });
        try es.append(a, .{ .key = "feeVouts", .value = .{ .map = fv.items } });
    }
    return .{ .map = es.items };
}

/// A revert row (0.41.0): the unwound transaction, and the pool as it stands after the undo
/// (`live`, the hop row given back) or only its id when the pool is gone.
fn revertRow(a: Allocator, txid: [32]u8, token: []const u8, pid: [36]u8, live: ?Value) !Value {
    var es: std.ArrayList(Entry) = .empty;
    try rowHead(a, &es, "revert", txid, token);
    const rec = live orelse {
        try es.appendSlice(a, &.{
            .{ .key = "poolId", .value = .{ .text = try poolIdText(a, &pid) } },
            .{ .key = "at", .value = .{ .uint = now_ms } },
        });
        return .{ .map = es.items };
    };
    try poolPart(a, &es, try fieldsOfRow(rec), true, now_ms);
    return .{ .map = es.items };
}

/// `row` with the event name `name` in front (the emit's `event`).
fn named(a: Allocator, name: []const u8, row: Value) !Value {
    return .{ .map = try std.mem.concat(a, Entry, &.{ &.{.{ .key = "event", .value = .{ .text = name } }}, row.map }) };
}

/// Emit a pool's row under `<app>/pool/<poolId>` and `<app>/hop`, and (a listed output) under
/// `<app>/output/<outpoint>`.
fn emitHop(a: Allocator, svc: *Service, row: Value, output: ?ParsedOutpoint) !void {
    const pid = row.getText("poolId") orelse return error.BadEvent;
    try svc.emit(try named(a, try poolName(a, pid), row));
    if (output) |op| try svc.emit(try named(a, try outputName(a, op.txid, op.vout), row));
    try svc.emit(try named(a, try hopName(a), row));
}

// ---------------------------------------------------------------- the counters (0.47.0) and the tally (0.49.0)

/// A pool's counters (`counters` under `p ‖ tp ‖ poolId`).
pub const PoolCounters = struct {
    sats_in: u64 = 0,
    sats_out: u64 = 0,
    tokens_in: u64 = 0,
    tokens_out: u64 = 0,
    lp_sats: u64 = 0,
    lp_tokens: u64 = 0,
    validator_sats: u64 = 0,
    validator_tokens: u64 = 0,
    commission_sats: u64 = 0,
    commission_tokens: u64 = 0,
    last_trade_at: ?u64 = null,
};

/// A token's counters (`t ‖ tp`).
pub const TokenCounters = struct { volume_sats: u64 = 0, volume_tokens: u64 = 0, trades: u64 = 0 };

/// A validator's counters (`v ‖ tp ‖ identity`).
pub const ValidatorCounters = struct { swaps: u64 = 0, fee_sats: u64 = 0, fee_tokens: u64 = 0 };

const pool_fields = .{
    .{ "satsIn", "sats_in" },             .{ "satsOut", "sats_out" },
    .{ "tokensIn", "tokens_in" },         .{ "tokensOut", "tokens_out" },
    .{ "lpSats", "lp_sats" },             .{ "lpTokens", "lp_tokens" },
    .{ "validatorSats", "validator_sats" }, .{ "validatorTokens", "validator_tokens" },
    .{ "commissionSats", "commission_sats" }, .{ "commissionTokens", "commission_tokens" },
};
const token_fields = .{ .{ "volumeSats", "volume_sats" }, .{ "volumeTokens", "volume_tokens" }, .{ "trades", "trades" } };
const validator_fields = .{ .{ "swaps", "swaps" }, .{ "feeSats", "fee_sats" }, .{ "feeTokens", "fee_tokens" } };

fn poolCountersKey(a: Allocator, tp: []const u8, pid: []const u8) ![]u8 {
    return cat(a, &.{ "p", tp, pid });
}
fn tokenCountersKey(a: Allocator, tp: []const u8) ![]u8 {
    return cat(a, &.{ "t", tp });
}
fn validatorCountersKey(a: Allocator, tp: []const u8, identity: []const u8) ![]u8 {
    return cat(a, &.{ "v", tp, identity });
}
fn tallyKey(a: Allocator, tp: []const u8) ![]u8 {
    return cat(a, &.{ "f", tp });
}

fn readCounters(comptime T: type, comptime fields: anytype, a: Allocator, svc: *Service, key: []const u8) !T {
    var out: T = .{};
    const cid = (try svc.map("counters").link(key)) orelse return out;
    const rec = try svc.store.getValue(a, cid);
    inline for (fields) |f| @field(out, f[1]) = rec.getUint(f[0]) orelse 0;
    if (T == PoolCounters) out.last_trade_at = rec.getUint("lastTradeAt");
    return out;
}

fn writeCounters(comptime T: type, comptime fields: anytype, a: Allocator, svc: *Service, key: []const u8, kind: []const u8, v: T) !void {
    var es: std.ArrayList(Entry) = .empty;
    try es.append(a, .{ .key = "kind", .value = .{ .text = kind } });
    var any = false;
    inline for (fields) |f| {
        const n: u64 = @field(v, f[1]);
        any = any or n != 0;
        try es.append(a, .{ .key = f[0], .value = .{ .uint = n } });
    }
    if (T == PoolCounters) try es.append(a, .{ .key = "lastTradeAt", .value = if (v.last_trade_at) |t| .{ .uint = t } else .null });
    if (!any) {
        _ = try svc.map("counters").remove(key);
        return;
    }
    try svc.map("counters").putLink(key, try svc.store.putValue(a, .{ .map = es.items }));
}

pub fn poolCounters(a: Allocator, svc: *Service, tp: []const u8, pid: []const u8) !PoolCounters {
    return readCounters(PoolCounters, pool_fields, a, svc, try poolCountersKey(a, tp, pid));
}
pub fn tokenCounters(a: Allocator, svc: *Service, tp: []const u8) !TokenCounters {
    return readCounters(TokenCounters, token_fields, a, svc, try tokenCountersKey(a, tp));
}
pub fn validatorCounters(a: Allocator, svc: *Service, tp: []const u8, identity: []const u8) !ValidatorCounters {
    return readCounters(ValidatorCounters, validator_fields, a, svc, try validatorCountersKey(a, tp, identity));
}

/// The token fee tally (0.49.0, David 2026-11-09 "TOKEN TALLY"): the token units of the fee outputs
/// that are ours and unspent, a running number (`counters` under `f ‖ tp`, `{kind:
/// "amm-fee-tally", units}`). Adjusted with WRAPPING arithmetic: the hooks of an unwind may meet in
/// either order (a spender's rejection before or after its parent's), so an intermediate value may
/// pass below zero; the end value is right modulo 2^64.
pub fn tally(a: Allocator, svc: *Service, tp: []const u8) !u64 {
    const cid = (try svc.map("counters").link(try tallyKey(a, tp))) orelse return 0;
    return (try svc.store.getValue(a, cid)).getUint("units") orelse 0;
}

fn adjustTally(a: Allocator, svc: *Service, tp: []const u8, units: u64, add: bool) !void {
    if (units == 0) return;
    const now = try tally(a, svc, tp);
    const next = if (add) now +% units else now -% units;
    const key = try tallyKey(a, tp);
    if (next == 0) {
        _ = try svc.map("counters").remove(key);
        return;
    }
    try svc.map("counters").putLink(key, try svc.store.putValue(a, .{ .map = try a.dupe(Entry, &.{
        .{ .key = "kind", .value = .{ .text = "amm-fee-tally" } },
        .{ .key = "units", .value = .{ .uint = next } },
    }) }));
}

fn bump(x: *u64, n: u64, add: bool) void {
    x.* = if (add) std.math.add(u64, x.*, n) catch std.math.maxInt(u64) else x.* -| n;
}

/// A Swap's hop row counted in (`add`) or unwound: its pool's, its token's and its validator's
/// counters. `last_other` (an unwind): the pool's latest other Swap's time, for `lastTradeAt`.
fn countTrade(a: Allocator, svc: *Service, tp: []const u8, row: Value, add: bool) !void {
    const pid_op = try parseOutpoint(row.getText("poolId") orelse return error.BadEvent);
    const pid = w.store.outpointKey(pid_op.txid, pid_op.vout);
    const at = row.getUint("at") orelse return error.BadEvent;
    const in = row.getUint("amountIn") orelse return error.BadEvent;
    const out = row.getUint("amountOut") orelse return error.BadEvent;
    const lp = row.getUint("feeLp") orelse 0;
    const val = row.getUint("feeValidator") orelse 0;
    const com = row.getUint("feeCommission") orelse 0;
    const bsv_in = eql(u8, row.getText("side") orelse "", "buy");
    const pk = try poolCountersKey(a, tp, &pid);
    var p = try readCounters(PoolCounters, pool_fields, a, svc, pk);
    if (bsv_in) {
        bump(&p.sats_in, in, add);
        bump(&p.tokens_out, out, add);
        bump(&p.lp_sats, lp, add);
        bump(&p.validator_sats, val, add);
        bump(&p.commission_sats, com, add);
    } else {
        bump(&p.tokens_in, in, add);
        bump(&p.sats_out, out, add);
        bump(&p.lp_tokens, lp, add);
        bump(&p.validator_tokens, val, add);
        bump(&p.commission_tokens, com, add);
    }
    if (add) {
        p.last_trade_at = if (p.last_trade_at) |t| @max(t, at) else at;
    } else if (p.last_trade_at != null and p.last_trade_at.? == at) {
        p.last_trade_at = try lastSwapAt(a, try poolIdText(a, &pid), row.getText("txid") orelse "");
    }
    try writeCounters(PoolCounters, pool_fields, a, svc, pk, "amm-pool-counters", p);
    const tk = try tokenCountersKey(a, tp);
    var t = try readCounters(TokenCounters, token_fields, a, svc, tk);
    const v = volumeOf(row);
    bump(&t.volume_sats, v.sats, add);
    bump(&t.volume_tokens, v.tokens, add);
    bump(&t.trades, 1, add);
    try writeCounters(TokenCounters, token_fields, a, svc, tk, "amm-token-counters", t);
    const identity = try hex33(row, "validatorIdentityKey");
    const vk = try validatorCountersKey(a, tp, &identity);
    var vc = try readCounters(ValidatorCounters, validator_fields, a, svc, vk);
    bump(&vc.swaps, 1, add);
    if (bsv_in) bump(&vc.fee_sats, val, add) else bump(&vc.fee_tokens, val, add);
    try writeCounters(ValidatorCounters, validator_fields, a, svc, vk, "amm-validator-counters", vc);
}

/// A pool's counters as its row carries them (`lifetime`, 0.47.0).
fn lifetimeValue(a: Allocator, svc: *Service, tp: []const u8, pid: []const u8) !Value {
    const p = try poolCounters(a, svc, tp, pid);
    var es: std.ArrayList(Entry) = .empty;
    inline for (pool_fields) |f| try es.append(a, .{ .key = f[0], .value = .{ .uint = @field(p, f[1]) } });
    try es.append(a, .{ .key = "lastTradeAt", .value = if (p.last_trade_at) |t| .{ .uint = t } else .null });
    return .{ .map = es.items };
}

// ---------------------------------------------------------------- a pool's standing history

/// A pool's hops as they stand (oldest first): its `<app>/pool/<poolId>` rows with every hop a later
/// revert unwound taken out (a revert unwinds the hops of its `txid`). One exact-name read.
pub fn standing(a: Allocator, pid_text: []const u8) ![]const Value {
    var out: std.ArrayList(Value) = .empty;
    for (try recordsOf(a, try poolName(a, pid_text))) |r| {
        if (eql(u8, kindOf(r), "revert")) {
            const t = r.getText("txid") orelse "";
            var i: usize = out.items.len;
            while (i > 0) {
                i -= 1;
                if (eql(u8, out.items[i].getText("txid") orelse "", t)) _ = out.orderedRemove(i);
            }
            continue;
        }
        try out.append(a, r);
    }
    return out.items;
}

/// The latest standing Swap's time of a pool other than the Swap `skip` (its txid hex); null none.
fn lastSwapAt(a: Allocator, pid_text: []const u8, skip: []const u8) !?u64 {
    const hops = try standing(a, pid_text);
    var i: usize = hops.len;
    while (i > 0) {
        i -= 1;
        const h = hops[i];
        if (!eql(u8, kindOf(h), "swap") or eql(u8, h.getText("txid") orelse "", skip)) continue;
        return h.getUint("at");
    }
    return null;
}

// ---------------------------------------------------------------- the fees that are ours (0.49.0)

/// The fee outputs of the Swap whose continuation row is `row` (at `txid`) that are OURS: the
/// validator fee when the pool names this instance as its validator; the commission when its
/// `<app>/commission/<txid>.<vout>` flag stands.
const Ours = struct { vout: u32, units: u64, token: bool };

fn oursOfRow(a: Allocator, row: Value, txid: [32]u8, buf: *[2]Ours) ![]const Ours {
    var n: usize = 0;
    if (!eql(u8, kindOf(row), "swap")) return buf[0..0];
    const token = eql(u8, row.getText("side") orelse "", "sell");
    const fv = row.get("feeVouts") orelse return buf[0..0];
    if (fv.getUint("validator")) |v| if (self_identity) |me| if (eql(u8, &me, &try hex33(row, "validatorIdentityKey"))) {
        buf[n] = .{ .vout = @intCast(v), .units = row.getUint("feeValidator") orelse 0, .token = token };
        n += 1;
    };
    if (fv.getUint("commission")) |v| if (try latestOf(a, try commissionName(a, txid, @intCast(v)))) |c| if (!isReverted(c)) {
        buf[n] = .{ .vout = @intCast(v), .units = c.getUint("amount") orelse 0, .token = token };
        n += 1;
    };
    return buf[0..n];
}

/// When txid:vout is a token fee output of ours: its units; else null. Read from the Swap's own
/// continuation row (`<app>/output/<txid>.0`, an earlier step's) and its commission flag.
fn ourTokenFee(a: Allocator, txid: [32]u8, vout: u32) !?u64 {
    const row = (try outputRow(a, txid, 0)) orelse return null;
    var buf: [2]Ours = undefined;
    for (try oursOfRow(a, row, txid, &buf)) |o| if (o.vout == vout and o.token) return o.units;
    return null;
}

/// At a Swap's admission: its fee outputs that are ours — FILED with the wallet (the seam), a token
/// fee added to the tally, the commission flagged (`<app>/commission/<op>`). `was` the spent pool's
/// row (its `keyId` the validator fee key's id), `spent` its outpoint.
fn recognizeFees(a: Allocator, svc: *Service, tp: []const u8, tx: lookup.Tx, row: Value, was: Value, spent_op: ParsedOutpoint, t: Trade) !void {
    const me = self_identity orelse return;
    const basket = if (t.bsv_in) basket_bsv else basket_tokens;
    if (t.validator_at) |v| if (eql(u8, &me, &try hex33(was, "validatorIdentityKey"))) {
        try fileWithWallet(a, .{
            .txid = tx.txid,
            .vout = @intCast(v),
            .protocol_level = pool.validator_protocol.security_level,
            .protocol = pool.validator_protocol.name,
            .key_id = was.getText("keyId") orelse return error.BadEvent,
            .counterparty = "anyone",
            .basket = basket,
            .satoshis = @intCast(tx.tx.outputs[v].satoshis),
        });
        if (!t.bsv_in) try adjustTally(a, svc, tp, t.validator, true);
    };
    if (t.commission_at) |v| {
        const ours = try pool.commissionPkh(a, me, spent_op.txid, spent_op.vout);
        if (eql(u8, &ours, &t.commission_pkh)) {
            var buf: [64 + 1 + 10]u8 = undefined;
            try fileWithWallet(a, .{
                .txid = tx.txid,
                .vout = @intCast(v),
                .protocol_level = pool.commission_protocol.security_level,
                .protocol = pool.commission_protocol.name,
                .key_id = try a.dupe(u8, pool.keyId(&buf, spent_op.txid, spent_op.vout)),
                .counterparty = "anyone",
                .basket = basket,
                .satoshis = @intCast(tx.tx.outputs[v].satoshis),
            });
            if (!t.bsv_in) try adjustTally(a, svc, tp, t.commission, true);
            try svc.emit(.{ .map = try a.dupe(Entry, &.{
                .{ .key = "event", .value = .{ .text = try commissionName(a, tx.txid, @intCast(v)) } },
                .{ .key = "type", .value = .{ .text = "commission" } },
                .{ .key = "swap", .value = .{ .text = try a.dupe(u8, &w.header.toHex(tx.txid)) } },
                .{ .key = "poolId", .value = row.get("poolId").? },
                .{ .key = "asset", .value = .{ .text = if (t.bsv_in) "bsv" else "token" } },
                .{ .key = "amount", .value = .{ .uint = t.commission } },
                .{ .key = "at", .value = .{ .uint = now_ms } },
            }) });
        }
    }
}

// ---------------------------------------------------------------- hooks

/// The Swap `tx` (its pool input `input`, its continuation `cont`, the listed pool it spent `was`):
/// the trade, or null when its fee outputs are not the contract's (its row then has no trade part).
fn tradeOf(a: Allocator, tx: lookup.Tx, input: usize, cont: PoolFields, was: Value) !?Trade {
    const f = (try pool.feesOf(a, tx.tx, input)) orelse return null;
    const call = (try pool.parseSwapCall(a, tx.tx.inputs[input].unlocking_script.bytes)) orelse return null;
    const cp = pool.poolOfScript(tx.tx.outputs[0].locking_script.bytes) orelse return null;
    const l = pool.feeLayout(call, cp) catch return null;
    const before = if (f.bsv_in) was.getUint("tokenReserve") orelse return error.BadEvent else was.getUint("bsvReserve") orelse return error.BadEvent;
    const after = if (f.bsv_in) cont.token_reserve else cont.bsv_reserve;
    if (after > before) return null;
    return .{
        .bsv_in = f.bsv_in,
        .amount_in = call.amount_in,
        .amount_out = before - after,
        .lp = f.lp,
        .validator = f.validator,
        .commission = f.commission,
        .commission_pkh = f.commission_pkh,
        .payout_pkh = call.user_pkh,
        .lp_at = l.lp_at,
        .validator_at = l.validator_at,
        .commission_at = l.commission_at,
    };
}

/// A topic admitted `tx`: when its pools pass the pool checks (`judge`), each admitted output that
/// is a listed pool (an affirmed deploy, a Swap's continuation of a listed pool) or a validator's
/// Rescind of one is emitted as its pool's hop. A Swap's trade is counted and its fees that are
/// ours recognized. An output already recognized (its `<app>/output/…` standing) is passed over:
/// a re-admission is counted once.
pub fn admitted(a: Allocator, svc: *Service, topic: []const u8, tx: lookup.Tx, outputs_to_admit: []const u32, _: []const u32) anyerror!void {
    const id = tokenIdOf(topic) orelse return; // not a token topic: nothing of ours
    if (outputs_to_admit.len == 0) return;
    if (try judge(a, id, tx, outputs_to_admit) != null) return;
    const tp = try topicPrefix(a, topic);
    const token = try tokenTextOf(a, topic);
    for (outputs_to_admit) |vout| {
        if (vout >= tx.tx.outputs.len) continue;
        const o = tx.tx.outputs[vout];
        var pf = poolAt(tx.txid, vout, o.locking_script.bytes, @intCast(o.satoshis)) orelse continue;
        if ((try outputRow(a, tx.txid, vout)) != null) continue; // recognized already
        const here: ParsedOutpoint = .{ .txid = tx.txid, .vout = vout };
        // A Swap's continuation, or a Rescind, at output 0: of a listed pool this tx spends.
        if (vout == 0) {
            const took = for (tx.tx.inputs) |in| {
                const op = in.previous_outpoint;
                const was = (try outputRow(a, op.txid.bytes, op.index)) orelse continue;
                if (try continuationOf(a, tx, was, op.txid.bytes, op.index)) |how| break .{ how, was, ParsedOutpoint{ .txid = op.txid.bytes, .vout = op.index } };
            } else null;
            if (took) |s| {
                const how, const was, const op = s;
                const wf = try fieldsOfRow(was);
                pf.pool_id = wf.pool_id;
                pf.swaps = wf.swaps + 1;
                switch (how) {
                    .rescind => try emitHop(a, svc, try hopRow(a, "rescind", tx.txid, token, pf, true, null), here),
                    .swap => {
                        var buf: [64 + 1 + 10]u8 = undefined;
                        pf.key_id = try a.dupe(u8, pool.keyId(&buf, op.txid, op.vout));
                        const input = inputSpending(tx, op.txid, op.vout).?;
                        const trade = try tradeOf(a, tx, input, pf, was);
                        const row = try hopRow(a, "swap", tx.txid, token, pf, true, trade);
                        try emitHop(a, svc, row, here);
                        if (trade) |t| {
                            try countTrade(a, svc, tp, row, true);
                            try recognizeFees(a, svc, tp, tx, row, was, op, t);
                        }
                    },
                }
                continue;
            }
        }
        // A deploy that attests itself.
        if (!pool.affirmed(a, o.locking_script.bytes, @intCast(o.satoshis))) continue;
        const p = pool.poolOfScript(o.locking_script.bytes).?;
        var nb: [64]u8 = undefined;
        pf.key_id = try a.dupe(u8, pool.nonceKeyId(&nb, p.nonce));
        try emitHop(a, svc, try hopRow(a, "deploy", tx.txid, token, pf, true, null), here);
    }
}

/// A previous coin was consumed. A listed pool output (its `<app>/output/…` standing) whose spender
/// continues it with no listed continuation (the same test `admitted` lists by: `continuationOf`,
/// re-derived from the spending transaction, since this step's own emits are not readable) is
/// the pool's Close. A token fee output of ours leaves the tally.
pub fn spent(a: Allocator, svc: *Service, topic: []const u8, outpoint: lookup.Outpoint, spender: lookup.Tx) anyerror!void {
    _ = tokenIdOf(topic) orelse return;
    const tp = try topicPrefix(a, topic);
    // Our token fee output leaves the tally — once: a second spender of the coin (skein-overlay
    // 0.26.0: a proven winner B heard after the loser A) finds it spent already by the topic's
    // fact (`<app>/topic/output/<op>`, A's step's), and A's rejection does not give it back.
    if (try ourTokenFee(a, outpoint.txid, outpoint.vout)) |units| if (!try spentByOther(a, outpoint, spender.txid)) try adjustTally(a, svc, tp, units, false);
    const was = (try outputRow(a, outpoint.txid, outpoint.vout)) orelse return;
    if ((try continuationOf(a, spender, was, outpoint.txid, outpoint.vout)) != null) return;
    var pf = try fieldsOfRow(was);
    pf.key_id = null;
    try emitHop(a, svc, try hopRow(a, "close", spender.txid, try tokenTextOf(a, topic), pf, false, null), null);
}

/// Whether the topic's own fact says txid:vout is spent already by a transaction other than
/// `by` (skein-overlay `<app>/topic/output/<op>` `{state: spent, spentBy}`, an earlier step's).
fn spentByOther(a: Allocator, op: lookup.Outpoint, by: [32]u8) !bool {
    const name = try std.fmt.allocPrint(a, "{s}/topic/output/{s}.{d}", .{ app, &w.header.toHex(op.txid), op.vout });
    const f = (try latestOf(a, name)) orelse return false;
    if (!eql(u8, f.getText("state") orelse "", "spent")) return false;
    return !eql(u8, f.getText("spentBy") orelse "", &w.header.toHex(by));
}

/// Whether input `i` of a rejected transaction is given back (skein-overlay 0.26.0 `givenBack`:
/// its coin live in the topic again; a coin another admitted transaction consumes is not). An
/// engine before 0.26.0 names none: every input is.
fn givenBack(svc: *Service, i: usize) bool {
    const gb = svc.given_back orelse return true;
    return std.mem.indexOfScalar(u32, gb, @intCast(i)) != null;
}

/// A pool's state after an unwind of `txid`: its latest standing hop other than `txid`'s (a
/// deploy's, a Swap's or a rescind's: the pool's fields; a close's: none), or null when the pool
/// is gone.
fn afterUndo(a: Allocator, pid_text: []const u8, txid: [32]u8) !?Value {
    const hex = &w.header.toHex(txid);
    const hops = try standing(a, pid_text);
    var i: usize = hops.len;
    while (i > 0) {
        i -= 1;
        if (eql(u8, hops[i].getText("txid") orelse "", hex)) continue;
        return if (hops[i].get("outpoint") != null) hops[i] else null;
    }
    return null;
}

/// A judgement of `topic` was removed by a rejection: every row `tx` wrote is unwound — under the
/// same names: each listed output of its own `{reverted}` under `<app>/output/…`, one revert row
/// per pool it touched under `<app>/pool/<poolId>` and `<app>/hop` (the pool after the undo: the
/// listed pool it spent given back, or none), its commission flag `{reverted}`; a Swap's counts
/// unwound; the tally loses its fees of ours and gets back the fee outputs of ours it had spent.
pub fn rejected(a: Allocator, svc: *Service, topic: []const u8, tx: lookup.Tx) anyerror!void {
    _ = tokenIdOf(topic) orelse return;
    const tp = try topicPrefix(a, topic);
    const token = try tokenTextOf(a, topic);
    var touched: std.ArrayList(struct { pid: [36]u8, live: ?Value }) = .empty;
    for (0..tx.tx.outputs.len) |vi| {
        const vout: u32 = @intCast(vi);
        const row = (try outputRow(a, tx.txid, vout)) orelse continue;
        const pid_op = try parseOutpoint(row.getText("poolId") orelse return error.BadEvent);
        if (eql(u8, kindOf(row), "swap") and row.get("amountIn") != null) {
            try countTrade(a, svc, tp, row, false);
            var buf: [2]Ours = undefined;
            // Its fee outputs of ours leave the tally — those still unspent (a spent one left it at
            // its spend: the topic's fact `<app>/topic/output/<op>` says spent).
            for (try oursOfRow(a, row, tx.txid, &buf)) |o| if (o.token and !try spentByOther(a, .{ .txid = tx.txid, .vout = o.vout }, tx.txid)) try adjustTally(a, svc, tp, o.units, false);
            if (row.get("feeVouts")) |fv| if (fv.getUint("commission")) |cv| {
                const cname = try commissionName(a, tx.txid, @intCast(cv));
                if (try latestOf(a, cname)) |c| if (!isReverted(c)) try svc.emit(.{ .map = try a.dupe(Entry, &.{
                    .{ .key = "event", .value = .{ .text = cname } },
                    .{ .key = "type", .value = .{ .text = "commission" } },
                    .{ .key = "reverted", .value = .{ .boolean = true } },
                    .{ .key = "swap", .value = c.get("swap") orelse .null },
                    .{ .key = "poolId", .value = c.get("poolId") orelse .null },
                    .{ .key = "at", .value = .{ .uint = now_ms } },
                }) });
            };
        }
        try svc.emit(.{ .map = try a.dupe(Entry, &.{
            .{ .key = "event", .value = .{ .text = try outputName(a, tx.txid, vout) } },
            .{ .key = "type", .value = .{ .text = "revert" } },
            .{ .key = "reverted", .value = .{ .boolean = true } },
            .{ .key = "txid", .value = .{ .text = try a.dupe(u8, &w.header.toHex(tx.txid)) } },
            .{ .key = "poolId", .value = row.get("poolId").? },
            .{ .key = "at", .value = .{ .uint = now_ms } },
        }) });
        try touch(a, &touched, w.store.outpointKey(pid_op.txid, pid_op.vout), null);
    }
    for (tx.tx.inputs, 0..) |in, i| {
        const op = in.previous_outpoint;
        const back = givenBack(svc, i);
        if (back) if (try ourTokenFee(a, op.txid.bytes, op.index)) |units| try adjustTally(a, svc, tp, units, true);
        const was = (try outputRow(a, op.txid.bytes, op.index)) orelse continue;
        // A pool this tx continued or closed: the pool's latest standing hop is this tx's.
        const pid_text = was.getText("poolId") orelse return error.BadEvent;
        const hops = try standing(a, pid_text);
        if (hops.len == 0) continue;
        if (!eql(u8, hops[hops.len - 1].getText("txid") orelse "", &w.header.toHex(tx.txid))) continue;
        const pid_op = try parseOutpoint(pid_text);
        try touch(a, &touched, w.store.outpointKey(pid_op.txid, pid_op.vout), null);
    }
    // Each pool's revert row: the pool as it stands without this tx's hops (the hop it spent given
    // back, or — a proven winner's — the hop that took its coin), or none.
    for (touched.items) |t| {
        const row = try revertRow(a, tx.txid, token, t.pid, try afterUndo(a, try poolIdText(a, &t.pid), tx.txid));
        try svc.emit(try named(a, try poolName(a, try poolIdText(a, &t.pid)), row));
        try svc.emit(try named(a, try hopName(a), row));
    }
}

fn touch(a: Allocator, list: anytype, pid: [36]u8, live: ?Value) !void {
    for (list.items) |*t| if (eql(u8, &t.pid, &pid)) {
        if (live != null) t.live = live;
        return;
    };
    try list.append(a, .{ .pid = pid, .live = live });
}

// ---------------------------------------------------------------- the answers

/// A trade's price: `sats` per `tokens` token units (base units both), exact.
pub const Price = struct {
    sats: u64,
    tokens: u64,

    pub fn lt(x: Price, y: Price) bool {
        return @as(u128, x.sats) * y.tokens < @as(u128, y.sats) * x.tokens;
    }
};

/// A Swap row's price, from its amounts by side; null for a row with no tokens.
pub fn priceOf(rec: Value) ?Price {
    const in = rec.getUint("amountIn") orelse return null;
    const out = rec.getUint("amountOut") orelse return null;
    const buy = eql(u8, rec.getText("side") orelse "", "buy");
    const p: Price = if (buy) .{ .sats = in, .tokens = out } else .{ .sats = out, .tokens = in };
    return if (p.tokens == 0) null else p;
}

fn volumeOf(rec: Value) struct { sats: u64, tokens: u64 } {
    const p = priceOf(rec) orelse return .{ .sats = 0, .tokens = 0 };
    return .{ .sats = p.sats, .tokens = p.tokens };
}

/// THE pool row the live list and the positions answer (0.48.0 shape): `{poolId, outpoint,
/// bsvReserve, tokenReserve, lpPubKey, liquidityFeeBps, validationFeeBps, commissionBps,
/// validatorIdentityKey, at, swaps, lifetime}` from a standing hop row.
fn poolStateValue(a: Allocator, svc: *Service, tp: []const u8, hop: Value) !Value {
    var es: std.ArrayList(Entry) = .empty;
    inline for (.{ "poolId", "outpoint", "bsvReserve", "tokenReserve", "lpPubKey", "liquidityFeeBps", "validationFeeBps", "commissionBps", "validatorIdentityKey", "at", "swaps" }) |k|
        try es.append(a, .{ .key = k, .value = hop.get(k) orelse return error.BadEvent });
    const op = try parseOutpoint(hop.getText("poolId").?);
    try es.append(a, .{ .key = "lifetime", .value = try lifetimeValue(a, svc, tp, &w.store.outpointKey(op.txid, op.vout)) });
    return .{ .map = es.items };
}

/// A pool's state from its standing hops: its current hop (a deploy's or a Swap's: live; a
/// rescind's: rescinded), or closed.
const State = struct { status: []const u8, current: ?Value, lp: ?Value };
fn stateOf(a: Allocator, pid_text: []const u8, token: []const u8) !?State {
    const hops = try standing(a, pid_text);
    if (hops.len == 0) return null;
    const top = hops[hops.len - 1];
    if (!eql(u8, top.getText("tokenId") orelse "", token)) return null;
    const k = kindOf(top);
    if (eql(u8, k, "deploy") or eql(u8, k, "swap")) return .{ .status = "live", .current = top, .lp = top.get("lpPubKey") };
    if (eql(u8, k, "rescind")) return .{ .status = "rescinded", .current = top, .lp = top.get("lpPubKey") };
    return .{ .status = "closed", .current = null, .lp = top.get("lpPubKey") };
}

/// `{tokenId}`: every live pool of the token (a rescinded one is not listed), in pool id order:
/// the latest row of every `<app>/pool/…`; a pool whose latest is a revert is read whole.
fn answerAll(a: Allocator, svc: *Service, tp: []const u8, token: []const u8) !lookup.Answer {
    var out: std.ArrayList(Value) = .empty;
    for (try latestUnder(a, try std.fmt.allocPrint(a, "{s}/pool/", .{app}))) |r| {
        if (!eql(u8, r.getText("tokenId") orelse "", token)) continue;
        const k = kindOf(r);
        var hop = r;
        if (eql(u8, k, "revert")) {
            const st = (try stateOf(a, r.getText("poolId") orelse continue, token)) orelse continue;
            if (!eql(u8, st.status, "live")) continue;
            hop = st.current.?;
        } else if (!eql(u8, k, "deploy") and !eql(u8, k, "swap")) continue;
        try out.append(a, try poolStateValue(a, svc, tp, hop));
    }
    return .{ .freeform = .{ .array = out.items } };
}

/// `{poolIds}` (0.40.0): per pool id `{poolId, lpPubKey, status, outpoint?, current?, sats, tokens,
/// swaps}` — the LP fees of its standing Swaps (sats from buys, tokens from sells), their count, and
/// its state (`stateOf`); an unknown id `closed` with zeros and `lpPubKey` null.
fn answerPools(a: Allocator, svc: *Service, tp: []const u8, token: []const u8, v: Value) !lookup.Answer {
    if (v != .array) return error.BadQuery;
    const out = try a.alloc(Value, v.array.len);
    for (v.array, out) |x, *o| {
        if (x != .text) return error.BadQuery;
        const op = try parseOutpoint(x.text);
        const pid_text = try outpointStr(a, op.txid, op.vout);
        var sats: u64 = 0;
        var tokens: u64 = 0;
        var swaps: u64 = 0;
        const st = try stateOf(a, pid_text, token);
        if (st != null) for (try standing(a, pid_text)) |h| {
            if (!eql(u8, kindOf(h), "swap") or h.get("amountIn") == null) continue;
            const fee = h.getUint("feeLp") orelse 0;
            if (eql(u8, h.getText("side") orelse "", "buy")) sats += fee else tokens += fee;
            swaps += 1;
        };
        var es: std.ArrayList(Entry) = .empty;
        try es.appendSlice(a, &.{
            .{ .key = "poolId", .value = .{ .text = pid_text } },
            .{ .key = "lpPubKey", .value = if (st) |s| s.lp orelse .null else .null },
        });
        if (st) |s| if (s.current) |c| try es.appendSlice(a, &.{
            .{ .key = "outpoint", .value = c.get("outpoint") orelse .null },
            .{ .key = "current", .value = try poolStateValue(a, svc, tp, c) },
        });
        try es.appendSlice(a, &.{
            .{ .key = "status", .value = .{ .text = if (st) |s| s.status else "closed" } },
            .{ .key = "sats", .value = .{ .uint = sats } },
            .{ .key = "tokens", .value = .{ .uint = tokens } },
            .{ .key = "swaps", .value = .{ .uint = swaps } },
        });
        o.* = .{ .map = es.items };
    }
    return .{ .freeform = .{ .array = out } };
}

/// `/lookup` (BRC-24, service `ls_amm_<assetId>`), every query naming its token (`tokenId`):
/// `{tokenId}` every live pool of the token; `{tokenId, poolIds}` per pool its state and LP fees.
/// 0.49.0: `{outpoint}`, `{validatorIdentityKey}`, `{validatorIdentityKeys}` and `{commissionPkhs}`
/// are gone (no reader).
pub fn answer(a: Allocator, svc: *Service, _: *Chain, query: Value) anyerror!lookup.Answer {
    if (query != .map) return error.BadQuery;
    const token_text = query.getText("tokenId") orelse return error.BadQuery;
    const topic = try topicOf(a, token_text);
    const tp = try topicPrefix(a, topic);
    const token = try tokenTextOf(a, topic);
    if (query.get("poolIds")) |v| return answerPools(a, svc, tp, token, v);
    if (query.get("outpoint") != null or query.get("validatorIdentityKey") != null or query.get("validatorIdentityKeys") != null or query.get("commissionPkhs") != null) return error.BadQuery;
    return answerAll(a, svc, tp, token);
}

/// The service: its summaries, the queries, the hooks.
pub const spec: lookup.Spec = .{
    .maps = &maps,
    .index = service_name,
    .answer = answer,
    .admitted = admitted,
    .spent = spent,
    .rejected = rejected,
};
