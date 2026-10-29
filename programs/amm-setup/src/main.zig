//! amm-setup: the token app's setup (0.22.0, shruggr/skein-amm#5), as one skein program (Zig,
//! wasm32-wasi). The decisions are setup.zig's (and src/deploy.zig's); this file wires them to the VM.
//!
//! **Stepped** (the event route `deploy`, box `amm/deploy`, filter `kernel.beef`, handler
//! `amm-setup.deploy`): the deploy event `{kind: "deploy", beef}`, its BEEF validated by the door
//! (its BUMPs against `chain/state`) and handed over as the envelope; the wire bytes read back
//! (`chain.record.wireOf`). In one step:
//!
//!   1. refused — a logged refusal (the result record's `refused`), nothing written — when the head
//!      `amm/token` exists ("this instance has its token"), the record is not a deploy event, or
//!      the BEEF's subject deploys no 1Color token;
//!   2. the head `amm/token` ← `{tokenId, topic, lookup, ammLookup, sym, dec, icon?, txid, vout}`;
//!   3. the engine called in-VM (skein-overlay 0.18.0, register by call): `register {topic:
//!      tm_color_<id>, program: "color-topic", seed: [<txid>]}`, `registerLookup {service:
//!      ls_color_<id>, program: "color-lookup", topics: [tm_color_<id>]}`, `registerLookup
//!      {service: ls_amm_<id>, program: "amm-lookup", topics: [tm_color_<id>]}`. A refused one
//!      fails the step: the head move and the calls are one step, so nothing is written;
//!   4. a deploy the seed did not take (the register's `missing`: the chain app does not hold it —
//!      the door stores the blocks, the chain app holds what is ingested) is submitted to the
//!      engine as the validator submits: `{fn: "submit", args: {beef, topics: [tm_color_<id>]}}` from
//!      this instance to itself in the box `amm/submit`.
//!
//! **Called** (read route; its filter: anyone, nothing logged):
//!
//!   token   `/token`: the head `amm/token` as JSON; 404 before the deploy event
const std = @import("std");
const w = @import("chain");
const vm = @import("overlay_sk");
const deploy = @import("deploy");
const setup = @import("setup.zig");

const cbor = w.cbor;
const Value = cbor.Value;
const Allocator = std.mem.Allocator;
const eql = std.mem.eql;

pub const app_name = "amm";
const app_head = "amm/app";
const submit_box = "amm/submit";

pub fn main() u8 {
    return vm.main("amm-setup", run);
}

fn run(a: Allocator) anyerror!void {
    const in = try vm.input(a);
    if (eql(u8, in.getText("kind") orelse "", "call")) return call(a, in);
    return step(a, in);
}

fn headRecord(a: Allocator, name: []const u8) !?Value {
    const c = (try vm.head(a, name)) orelse return null;
    return try vm.store().getValue(a, c);
}

// ---------------------------------------------------------------- reads

fn isFilter(in: Value) bool {
    const f = in.get("filter") orelse return false;
    return f == .boolean and f.boolean;
}

fn httpJson(a: Allocator, status: u16, body: []const u8) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "status", .value = .{ .uint = status } },
        .{ .key = "type", .value = .{ .text = "application/json" } },
        .{ .key = "body", .value = .{ .bytes = body } },
    }) };
}

fn call(a: Allocator, in: Value) !void {
    const func = in.getText("fn") orelse return error.BadInput;
    if (!eql(u8, func, "token")) return error.UnknownFunction;
    const ans = if (try setup.tokenJson(a, try headRecord(a, setup.token_head))) |j|
        try httpJson(a, 200, j)
    else
        try httpJson(a, 404, "{\"status\":\"error\",\"message\":\"no token: this instance has taken no deploy event\"}");
    return vm.answer(a, if (isFilter(in)) .{ .map = try a.dupe(cbor.Entry, &.{.{ .key = "answer", .value = ans }}) } else ans);
}

// ---------------------------------------------------------------- the deploy event

fn result(a: Allocator, fields: []const cbor.Entry) !Value {
    var es: std.ArrayList(cbor.Entry) = .empty;
    try es.appendSlice(a, &.{
        .{ .key = "kind", .value = .{ .text = setup.result_kind } },
        .{ .key = "op", .value = .{ .text = setup.event_kind } },
    });
    try es.appendSlice(a, fields);
    return .{ .map = es.items };
}

/// A logged refusal: the result record names why; no head moves, nothing called.
fn refuse(a: Allocator, why: []const u8) !void {
    std.log.err("deploy: refused: {s}", .{why});
    _ = try vm.finish(a, vm.store(), try result(a, &.{.{ .key = "refused", .value = .{ .text = why } }}));
}

/// The overlay engine's program record: the installed app record's role `overlay`.
fn engineProgram(a: Allocator, in: Value) ![]const u8 {
    if (try headRecord(a, app_head)) |rec| if (rec.get("programs")) |ps| if (ps.getCid("overlay")) |p| return p;
    if (in.get("programs")) |ps| if (ps.getCid("overlay")) |p| return p;
    return error.NoOverlayProgram;
}

fn step(a: Allocator, in: Value) !void {
    const s = vm.store();
    const args = in.get("args") orelse return error.BadInput;
    const ev = args.getCid("event") orelse return error.BadInput;
    const rec = try s.getValue(a, ev);
    const env = switch (setup.eventOf(rec, (try vm.head(a, setup.token_head)) != null)) {
        .refused => |why| return refuse(a, why),
        .envelope => |e| e,
    };
    const wire = w.record.wireOf(a, s, env) catch |e| return refuse(a, try std.fmt.allocPrint(a, "the deploy event's BEEF is not one the door read ({s})", .{@errorName(e)}));
    const d = switch (try deploy.deployIn(a, wire)) {
        .refused => |why| return refuse(a, why),
        .ok => |x| x,
    };

    const token = try s.putValue(a, try setup.tokenRecord(a, d));
    try vm.advance(setup.token_head, token);

    const engine = try engineProgram(a, in);
    var answers: std.ArrayList(Value) = .empty;
    var took = false;
    for (try setup.registrations(a, d), 0..) |r, i| {
        const ans = try vm.call(a, engine, r.func, r.arg);
        if (try setup.refusalOf(a, r.func, ans)) |why| {
            // The step fails: the head move above and any registration before are not written.
            std.log.err("deploy: {s}", .{why});
            return error.RegistrationRefused;
        }
        if (i == 0) took = setup.seeded(ans, d.txid);
        try answers.append(a, ans);
    }

    var fields: std.ArrayList(cbor.Entry) = .empty;
    try fields.appendSlice(a, &.{
        .{ .key = "tokenId", .value = .{ .text = d.token_id } },
        .{ .key = "token", .value = .{ .cid = token } },
        .{ .key = "registered", .value = .{ .array = answers.items } },
    });
    if (!took) {
        const me = vm.selfKey(in) orelse return error.NoSelfKey;
        const m = try vm.send(a, me, submit_box, try setup.submitBody(a, wire, d.topic));
        try fields.append(a, .{ .key = "submitted", .value = .{ .cid = m } });
    }
    _ = try vm.finish(a, s, try result(a, fields.items));
}
