//! amm-config: the Open Exchange's config instance (0.17.0, shruggr/skein-amm#4), as one skein
//! program (Zig, wasm32-wasi). The decisions are tokens.zig's and requests.zig's; this file wires
//! them to the VM.
//!
//! **Stepped** (mailbox routes):
//!
//!   box `amm/admin` (kernel.beef; the role root gates the three fns):
//!     {fn: "enable", args: {tokenId, beef}}   the BEEF (the door checked it and handed the pointer
//!                       record; the wire read back, chain.record.wireOf) must be the token id's
//!                       deploy (tokens.zig `deployOf`); refused for a token `enabled`, `creating`
//!                       or `starting`. A `stopped` one: `start {handle}` to the instance manager.
//!                       Else the row `creating` and `create {handle: <tokenId>, image, config,
//!                       beef}` (tokens.zig `createBody`; image `config["amm-config"].tokenImage`,
//!                       default amm-token). The thread rests on the manager's answer, then the row
//!                       `enabled` (url, delivered) or `failed` (reason) and root is answered.
//!     {fn: "disable", args: {tokenId}}        `stop {handle}`; on the answer the row `stopped`.
//!     {fn: "defaults", args: {terms?, market?}}   the head `amm/defaults` (tokens.zig `withDefaults`).
//!     Each answered to the sender in that box: `{fn, request, replyTo, result | error}`. A body
//!     that is itself an answer (`request` / `replyTo` in it: this program's answer looped back
//!     when root's key is the instance's own, a one-key host) is not a request: nothing runs.
//!   box `amm/requests` (anyone): {fn: "request", args: {tokenId}}, a holder's listing request
//!                       (requests.zig), recorded under the head `amm/requests`.
//!
//! **Called** (read routes; their filters: anyone, nothing logged):
//!
//!   tokens    `/tokens`: the enable list, newest first (tokens.zig `tokensJson`)
//!   defaults  `/defaults`: the defaults, `{terms?, market?}` (tokens.zig `defaultsJson`)
//!   requests  `/requests`: the holders' requests, less the tokens on the list
//!   serve     `/` (prefix): the Swap pages, www/ of the app's own tree (skein-sdk `files.serve`)
const std = @import("std");
const w = @import("chain");
const vm = @import("overlay_sk");
const tokens = @import("tokens.zig");
const requests = @import("requests.zig");
const sk = @import("sk");
const app = @import("app");
const files = @import("files");
const scbor = @import("sdk_cbor");

const cbor = w.cbor;
const Value = cbor.Value;
const Allocator = std.mem.Allocator;
const eql = std.mem.eql;

pub const app_name = "amm";
const app_head = "amm/app";

pub fn main() u8 {
    return vm.main("amm-config", run);
}

fn run(a: Allocator) anyerror!void {
    const in = try vm.input(a);
    if (eql(u8, in.getText("kind") orelse "", "call")) return call(a, in);
    return step(a, in);
}

// ---------------------------------------------------------------- reads

fn isFilter(in: Value) bool {
    const f = in.get("filter") orelse return false;
    return f == .boolean and f.boolean;
}

fn asFilterAnswer(a: Allocator, http: Value) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{.{ .key = "answer", .value = http }}) };
}

fn httpJson(a: Allocator, status: u16, body: []const u8) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "status", .value = .{ .uint = status } },
        .{ .key = "type", .value = .{ .text = "application/json" } },
        .{ .key = "body", .value = .{ .bytes = body } },
    }) };
}

fn headRecord(a: Allocator, name: []const u8) !?Value {
    const c = (try vm.head(a, name)) orelse return null;
    return try vm.store().getValue(a, c);
}

fn call(a: Allocator, in: Value) !void {
    const func = in.getText("fn") orelse return error.BadInput;
    const arg = try vm.callArg(a, in);
    const ans: Value = if (eql(u8, func, "tokens"))
        try httpJson(a, 200, try tokens.tokensJson(a, tokens.rowsOf(try headRecord(a, tokens.tokens_head))))
    else if (eql(u8, func, "defaults"))
        try httpJson(a, 200, try tokens.defaultsJson(a, try headRecord(a, tokens.defaults_head)))
    else if (eql(u8, func, "requests"))
        try requestsRead(a)
    else if (eql(u8, func, "serve"))
        try servePages(a, arg)
    else
        return error.UnknownFunction;
    return vm.answer(a, if (isFilter(in)) try asFilterAnswer(a, ans) else ans);
}

/// `/requests`: the holders' requests, newest first, less the tokens on the enable list (enabled,
/// creating or starting: an enable settles a request).
fn requestsRead(a: Allocator) !Value {
    var settled: std.ArrayList([]const u8) = .empty;
    for (tokens.rowsOf(try headRecord(a, tokens.tokens_head))) |r| {
        const st = tokens.statusOf(r) orelse continue;
        if (st == .enabled or st == .creating or st == .starting) try settled.append(a, r.getText("topic") orelse continue);
    }
    const rs = try requests.list(a, vm.store(), try headRecord(a, requests.requests_head), settled.items);
    return httpJson(a, 200, try requests.requestsJson(a, rs));
}

fn toSdk(a: Allocator, v: Value) !scbor.Value {
    return scbor.decode(a, try cbor.encode(a, v));
}

fn fromSdk(a: Allocator, v: scbor.Value) !Value {
    return cbor.decode(a, try scbor.encode(a, v));
}

/// The read `/`: the pages from the app's own tree (the installed app record's `tree`).
fn servePages(a: Allocator, arg: Value) !Value {
    const req = try toSdk(a, arg);
    const m = try app.manifestOf(a, app_name);
    return fromSdk(a, try files.serve(a, req, scbor.Value.cidOf(m.get("tree")), files.rowOptions(req)));
}

// ---------------------------------------------------------------- steps

fn resultRecord(a: Allocator, op: []const u8, fields: []const cbor.Entry) !Value {
    var es: std.ArrayList(cbor.Entry) = .empty;
    try es.appendSlice(a, &.{
        .{ .key = "kind", .value = .{ .text = "amm-config-result" } },
        .{ .key = "op", .value = .{ .text = op } },
    });
    try es.appendSlice(a, fields);
    return .{ .map = es.items };
}

fn step(a: Allocator, in: Value) !void {
    const s = vm.store();
    const args = in.get("args") orelse return error.BadInput;
    const box = args.getText("box") orelse "";
    const body = try s.getValue(a, args.getCid("body") orelse return error.BadInput);
    if (eql(u8, box, requests.requests_box)) return listingRequest(a, in, args, body);
    if (!eql(u8, box, tokens.admin_box)) return error.BadInput;
    // The instance manager's answer this thread awaited.
    if (in.get("reply")) |r| if (r == .map) return managerAnswered(a, args, body, try s.getValue(a, r.getCid("body") orelse return error.BadInput));
    if (tokens.isAnswer(body)) {
        _ = try vm.finish(a, s, try resultRecord(a, "answer", &.{}));
        return;
    }
    const func = body.getText("fn") orelse "";
    if (eql(u8, func, "enable")) return enable(a, in, args, body);
    if (eql(u8, func, "disable")) return disable(a, args, body);
    if (eql(u8, func, "defaults")) return defaults(a, args, body);
    return answerRoot(a, args, func, try tokens.refusal(a, func, args.getCid("message"), "unknown-fn", "amm/admin takes enable, disable and defaults"));
}

/// Answer root (the message's sender) in the box it wrote to; the thread ends.
fn answerRoot(a: Allocator, args: Value, op: []const u8, answer: Value) !void {
    if (answer.get("error")) |e| std.log.err("{s}: refused: {s}", .{ op, e.getText("message") orelse "" });
    if (args.getBytes("sender")) |sender| {
        _ = vm.send(a, sender, tokens.admin_box, answer) catch |e| std.log.err("{s}: the answer is not sent: {s}", .{ op, @errorName(e) });
    }
    _ = try vm.finish(a, vm.store(), try resultRecord(a, op, &.{.{ .key = "answer", .value = answer }}));
}

/// The instance manager's key: the address book's (`local`, `manager`): the host names this
/// instance in SKEIN_MANAGER_FOR.
fn managerKey(a: Allocator) ![]const u8 {
    const k = (try sk.peerAt(a, "local", "manager")) orelse return error.NoInstanceManager;
    return if (k.len == 33) k else error.NoInstanceManager;
}

/// The image a token instance is created from: `config["amm-config"].tokenImage` of the installed
/// app record, else `amm-token`.
fn tokenImage(a: Allocator) ![]const u8 {
    const rec = (try headRecord(a, app_head)) orelse return tokens.default_token_image;
    const cf = rec.get("config") orelse return tokens.default_token_image;
    const sec = cf.get("amm-config") orelse return tokens.default_token_image;
    return sec.getText("tokenImage") orelse tokens.default_token_image;
}

fn saveRow(a: Allocator, row: Value) !void {
    const s = vm.store();
    const rec = try tokens.withRow(a, tokens.rowsOf(try headRecord(a, tokens.tokens_head)), row);
    try vm.advance(tokens.tokens_head, try s.putValue(a, rec));
}

fn tokenIdArg(a: Allocator, body: Value) !?[]const u8 {
    const fargs = body.get("args") orelse return null;
    return tokens.canonicalId(a, fargs.getText("tokenId") orelse return null);
}

/// `enable {tokenId, beef}`.
fn enable(a: Allocator, in: Value, args: Value, body: Value) !void {
    const s = vm.store();
    const message = args.getCid("message");
    const id = (try tokenIdArg(a, body)) orelse return answerRoot(a, args, "enable", try tokens.refusal(a, "enable", message, "bad-args", "want args {tokenId: <txid>_<vout>, beef}"));
    const prev = tokens.find(tokens.rowsOf(try headRecord(a, tokens.tokens_head)), id);
    if (prev) |p| switch (tokens.statusOf(p) orelse .failed) {
        .enabled, .creating, .starting => return answerRoot(a, args, "enable", try tokens.refusal(a, "enable", message, "bad-args", try std.fmt.allocPrint(a, "{s} is {s} already", .{ id, p.getText("status") orelse "" }))),
        .stopped => {
            // Started again: the instance stands.
            try saveRow(a, try tokens.updated(a, p, .starting, &.{}, &.{"reason"}));
            const m = try vm.send(a, try managerKey(a), "start", try tokens.handleBody(a, id));
            try vm.awaitRecord(m);
            _ = try vm.finish(a, s, try resultRecord(a, "enable", &.{ .{ .key = "tokenId", .value = .{ .text = id } }, .{ .key = "asked", .value = .{ .text = "start" } } }));
            return;
        },
        .failed => {},
    };
    const fargs = body.get("args").?;
    const env = fargs.get("beef") orelse return answerRoot(a, args, "enable", try tokens.refusal(a, "enable", message, "bad-args", "want args {tokenId, beef}: the deploy's Atomic BEEF"));
    // The door's envelope (shruggr/skein#146: its exact wire bytes back through `wireOf`), or the bytes.
    const wire: []const u8 = switch (env) {
        .bytes => |b| b,
        .map => w.record.wireOf(a, s, env) catch |e| return answerRoot(a, args, "enable", try tokens.refusal(a, "enable", message, "bad-args", try std.fmt.allocPrint(a, "beef: not a BEEF the door read ({s})", .{@errorName(e)}))),
        else => return answerRoot(a, args, "enable", try tokens.refusal(a, "enable", message, "bad-args", "beef: want the deploy's Atomic BEEF (bytes)")),
    };
    const d = switch (try tokens.deployOf(a, id, wire)) {
        .refused => |why| return answerRoot(a, args, "enable", try tokens.refusal(a, "enable", message, "bad-args", why)),
        .ok => |x| x,
    };
    const at: u64 = in.getUint("at") orelse in.getUint("now") orelse 0;
    try saveRow(a, try tokens.creatingRow(a, d, at));
    const m = try vm.send(a, try managerKey(a), "create", try tokens.createBody(a, d, try tokenImage(a), try headRecord(a, tokens.defaults_head), wire));
    try vm.awaitRecord(m);
    _ = try vm.finish(a, s, try resultRecord(a, "enable", &.{ .{ .key = "tokenId", .value = .{ .text = id } }, .{ .key = "asked", .value = .{ .text = "create" } } }));
}

/// `disable {tokenId}`.
fn disable(a: Allocator, args: Value, body: Value) !void {
    const s = vm.store();
    const message = args.getCid("message");
    const id = (try tokenIdArg(a, body)) orelse return answerRoot(a, args, "disable", try tokens.refusal(a, "disable", message, "bad-args", "want args {tokenId: <txid>_<vout>}"));
    const prev = tokens.find(tokens.rowsOf(try headRecord(a, tokens.tokens_head)), id) orelse return answerRoot(a, args, "disable", try tokens.refusal(a, "disable", message, "bad-args", try std.fmt.allocPrint(a, "{s} is not on the list", .{id})));
    if (tokens.statusOf(prev) != .enabled) return answerRoot(a, args, "disable", try tokens.refusal(a, "disable", message, "bad-args", try std.fmt.allocPrint(a, "{s} is {s}, not enabled", .{ id, prev.getText("status") orelse "" })));
    const m = try vm.send(a, try managerKey(a), "stop", try tokens.handleBody(a, id));
    try vm.awaitRecord(m);
    _ = try vm.finish(a, s, try resultRecord(a, "disable", &.{ .{ .key = "tokenId", .value = .{ .text = id } }, .{ .key = "asked", .value = .{ .text = "stop" } } }));
}

/// The manager's answer to this thread's create, start or stop: the row moved, root answered.
fn managerAnswered(a: Allocator, args: Value, body: Value, ans: Value) !void {
    const func = body.getText("fn") orelse "";
    const message = args.getCid("message");
    const id = (try tokenIdArg(a, body)) orelse return error.BadInput;
    const prev = tokens.find(tokens.rowsOf(try headRecord(a, tokens.tokens_head)), id) orelse return error.BadState;
    const row: Value = if (eql(u8, func, "disable"))
        (if (ans.getText("error")) |e| try tokens.updated(a, prev, .enabled, &.{.{ .key = "reason", .value = .{ .text = e } }}, &.{}) else try tokens.updated(a, prev, .stopped, &.{}, &.{"reason"}))
    else
        try tokens.answered(a, prev, ans, tokens.statusOf(prev) == .starting);
    try saveRow(a, row);
    const answer = if (ans.getText("error")) |e| try tokens.refusal(a, func, message, "refused", e) else try tokens.answer(a, func, message, row);
    return answerRoot(a, args, func, answer);
}

/// `defaults {terms?, market?}`.
fn defaults(a: Allocator, args: Value, body: Value) !void {
    const s = vm.store();
    const message = args.getCid("message");
    switch (try tokens.withDefaults(a, try headRecord(a, tokens.defaults_head), body.get("args"))) {
        .bad => |why| return answerRoot(a, args, "defaults", try tokens.refusal(a, "defaults", message, "bad-args", why)),
        .ok => |rec| {
            try vm.advance(tokens.defaults_head, try s.putValue(a, rec));
            return answerRoot(a, args, "defaults", try tokens.answer(a, "defaults", message, rec));
        },
    }
}

/// A holder's listing request, `{fn: "request", args: {tokenId}}` in the box `amm/requests`.
fn listingRequest(a: Allocator, in: Value, args: Value, body: Value) !void {
    const s = vm.store();
    if (!eql(u8, body.getText("fn") orelse "", "request")) return error.BadMessage;
    const token = (body.get("args") orelse return error.BadMessage).getText("tokenId") orelse return error.BadMessage;
    const sender = args.getBytes("sender") orelse return error.BadInput;
    const at: u64 = in.getUint("at") orelse in.getUint("now") orelse 0;
    const rec = try requests.record(a, s, try headRecord(a, requests.requests_head), token, sender, at);
    try vm.advance(requests.requests_head, try s.putValue(a, rec));
    _ = try vm.finish(a, s, try resultRecord(a, "request", &.{.{ .key = "tokenId", .value = .{ .text = try requests.tokenIdText(a, token) } }}));
}
