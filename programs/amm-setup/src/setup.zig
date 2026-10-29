//! amm-setup's decisions, pure (0.22.0, shruggr/skein-amm#5): main.zig wires them to the VM.
//!
//! - **The token record**, the root of the head `amm/token` (immutable: written once, by the first
//!   deploy event this instance takes):
//!
//!     {tokenId, topic, lookup, ammLookup, sym, dec, icon?, txid, vout}
//!
//!   `tokenId` `<txid>_<vout>`; `topic` `tm_color_<tokenId>`; `lookup` `ls_color_<tokenId>`;
//!   `ammLookup` `ls_amm_<tokenId>`; `sym`, `dec`, `icon` the deploy's (1Color's token list's
//!   fields; `icon` a data URL, absent when the deploy carries none); `txid` the deploy transaction
//!   (display hex), `vout` its deploy output.
//! - **The deploy event** `{kind: "deploy", beef: <the door's envelope>}`: refused (`Check.refused`)
//!   when the instance has its token already, the record is not a deploy event, it carries no BEEF
//!   the door read, or the BEEF's subject deploys no 1Color token (src/deploy.zig `deployIn`).
//! - **The registrations**, the engine's by call (skein-overlay 0.18.0): `register {topic, program:
//!   "color-topic", seed: [<txid>]}`, `registerLookup {service: ls_color_<id>, program:
//!   "color-lookup", topics: [<topic>]}`, `registerLookup {service: ls_amm_<id>, program:
//!   "amm-lookup", topics: [<topic>]}` — the programs the app's own roles.
const std = @import("std");
const w = @import("chain");
const deploy = @import("deploy");

const cbor = w.cbor;
const Value = cbor.Value;
const Allocator = std.mem.Allocator;
const eql = std.mem.eql;

pub const token_head = "amm/token";
pub const event_kind = "deploy";
/// The app's roles the registrations name.
pub const topic_program = "color-topic";
pub const color_lookup_program = "color-lookup";
pub const amm_lookup_program = "amm-lookup";
pub const result_kind = "amm-setup-result";

pub const Deploy = deploy.Deploy;

/// What the step does with an event: refuse it (why), or take the deploy's envelope (its wire
/// bytes still to be read back from the store: main.zig, `chain.record.wireOf`).
pub const Event = union(enum) { refused: []const u8, envelope: Value };

/// The event record as the door handed it back, against whether the head `amm/token` exists.
pub fn eventOf(rec: Value, has_token: bool) Event {
    if (has_token) return .{ .refused = "this instance has its token: amm/token is written once" };
    if (rec != .map or !eql(u8, rec.getText("kind") orelse "", event_kind)) return .{ .refused = "not a deploy event: want {kind: \"deploy\", beef}" };
    const env = rec.get("beef") orelse return .{ .refused = "the deploy event carries no BEEF" };
    // The route's kernel.beef rewrote the bytes into the door's envelope; anything else did not
    // come through the door.
    if (env != .map or env.getCid("beef") == null) return .{ .refused = "the deploy event's beef is not the door's envelope (the route's kernel.beef)" };
    return .{ .envelope = env };
}

fn map(a: Allocator, es: []const cbor.Entry) !Value {
    return .{ .map = try a.dupe(cbor.Entry, es) };
}

/// The record under `amm/token`.
pub fn tokenRecord(a: Allocator, d: Deploy) !Value {
    var es: std.ArrayList(cbor.Entry) = .empty;
    try es.appendSlice(a, &.{
        .{ .key = "tokenId", .value = .{ .text = d.token_id } },
        .{ .key = "topic", .value = .{ .text = d.topic } },
        .{ .key = "lookup", .value = .{ .text = d.lookup } },
        .{ .key = "ammLookup", .value = .{ .text = try deploy.ammLookupName(a, d.token_id) } },
        .{ .key = "sym", .value = .{ .text = d.sym } },
        .{ .key = "dec", .value = .{ .uint = d.dec } },
    });
    if (d.icon) |ic| try es.append(a, .{ .key = "icon", .value = .{ .text = ic } });
    try es.appendSlice(a, &.{
        .{ .key = "txid", .value = .{ .text = d.txid } },
        .{ .key = "vout", .value = .{ .uint = d.vout } },
    });
    return .{ .map = es.items };
}

/// One registration: the engine's fn and its argument.
pub const Registration = struct { func: []const u8, arg: Value };

/// The three registrations, in order: the topic (seeded with the deploy), its 1Color lookup, the
/// AMM lookup.
pub fn registrations(a: Allocator, d: Deploy) ![3]Registration {
    const topics: Value = .{ .array = try a.dupe(Value, &.{.{ .text = d.topic }}) };
    return .{
        .{ .func = "register", .arg = try map(a, &.{
            .{ .key = "topic", .value = .{ .text = d.topic } },
            .{ .key = "program", .value = .{ .text = topic_program } },
            .{ .key = "seed", .value = .{ .array = try a.dupe(Value, &.{.{ .text = d.txid }}) } },
        }) },
        .{ .func = "registerLookup", .arg = try map(a, &.{
            .{ .key = "service", .value = .{ .text = d.lookup } },
            .{ .key = "program", .value = .{ .text = color_lookup_program } },
            .{ .key = "topics", .value = topics },
        }) },
        .{ .func = "registerLookup", .arg = try map(a, &.{
            .{ .key = "service", .value = .{ .text = try deploy.ammLookupName(a, d.token_id) } },
            .{ .key = "program", .value = .{ .text = amm_lookup_program } },
            .{ .key = "topics", .value = topics },
        }) },
    };
}

/// The engine's answer to a registration: `{fn, result}` → null; `{fn, error: {code, message}}` (or
/// anything else) → why.
pub fn refusalOf(a: Allocator, func: []const u8, ans: Value) !?[]const u8 {
    if (ans.get("result")) |r| if (r == .map) return null;
    const msg = if (ans.get("error")) |e| e.getText("message") orelse "" else "";
    return try std.fmt.allocPrint(a, "{s} refused by the engine: {s}", .{ func, if (msg.len > 0) msg else "no result" });
}

/// Whether the topic's register answer took the deploy: its `seeded` names `txid`. Not seeded
/// (`missing`: the chain app does not hold it yet) → main.zig submits the deploy to the engine.
pub fn seeded(ans: Value, txid: []const u8) bool {
    const r = ans.get("result") orelse return false;
    for (r.getArray("seeded") orelse return false) |x| if (x == .text and eql(u8, x.text, txid)) return true;
    return false;
}

/// The submission of the deploy to this instance's own engine (the validator's form, amm-validator
/// submit.zig): `{fn: "submit", args: {beef: <bytes>, topics: [<topic>]}}`, in the box `<app>/submit`.
pub fn submitBody(a: Allocator, wire: []const u8, topic: []const u8) !Value {
    return map(a, &.{
        .{ .key = "fn", .value = .{ .text = "submit" } },
        .{ .key = "args", .value = try map(a, &.{
            .{ .key = "beef", .value = .{ .bytes = wire } },
            .{ .key = "topics", .value = .{ .array = try a.dupe(Value, &.{.{ .text = topic }}) } },
        }) },
    });
}

/// The `/token` answer's JSON: the record's fields (text and numbers), or null when there is none.
pub fn tokenJson(a: Allocator, rec: ?Value) !?[]const u8 {
    const r = rec orelse return null;
    if (r != .map) return null;
    var out: std.Io.Writer.Allocating = .init(a);
    var jw: std.json.Stringify = .{ .writer = &out.writer };
    try jw.beginObject();
    for (r.map) |e| switch (e.value) {
        .text => |t| {
            try jw.objectField(e.key);
            try jw.write(t);
        },
        .uint => |n| {
            try jw.objectField(e.key);
            try jw.write(n);
        },
        else => {},
    };
    try jw.endObject();
    return out.written();
}
