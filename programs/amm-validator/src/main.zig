//! amm-validator: the AMM validator as a skein program (wasm32-wasi). The
//! thin VM adapter around messages.zig and validator.zig; the decisions are
//! there.
//!
//! **A direct call** (what libp2p calls a stream, skein#51). The app asks
//! for one route (docs/APPS.md §2: libp2p protocols are routes
//! `libp2p:<protocol>`, not namespaced): `libp2p:/amm-validator/1/swap` →
//! fn `swap` (0.11.0: `.../deploy` is gone; fn `affirm` is called in-VM by amm-p2p's
//! `amm.pool.affirm`, below, and answered at once). Each frame on one of them is a `p2p-frame` request the front
//! door steps on, and calls the handler with in its step (skein docs/MESSAGES.md,
//! "libp2p (#51)", "Streams"; "Route handlers: the program-facing contract
//! (#68, #66)"): `arg` is `{transport: "libp2p", protocol, from, key, body,
//! request, resolved?, …}`.
//!
//! The frame's body is a signed-message package `{message, body}` (MESSAGES
//! "Signed messages (#70)"): the taker (or LP) authenticates by its wallet
//! identity, the package checked with the SDK's verifier (messages.zig
//! `open`). Then, in this ONE step (0.51.0, David 2026-11-11): the checks and
//! the oracle's signature (validator.zig; every input unspent in our own view
//! before signing, the pool input's script run on the finished transaction),
//! and the signed BEEF handed to this instance's own overlay engine by its
//! in-VM `intake` (skein-overlay 0.28.0: the chain app's ingest and broadcast,
//! the admission, in this step). The handler answers `{verdict: "accept",
//! body: <reply>}` at once — a refusal, an affirmation, or the signed
//! transaction with the overlay's verdict (messages.zig `answerFromSubmit`).
//! Requests for one pool are taken one step at a time in arrival order: the
//! first is signed and admitted; each later one finds the pool spent
//! (`pool_spent`, the newest state and its BEEF). Nothing waits: no lock, no
//! `{wait: true}`, no `pending`.
//!
//! The validator role (0.6.0, shruggr/skein#120, David 2026-10-06 evening; 0.8.1, skein-overlay
//! 0.12.0, David 2026-10-09: "every skein is marketplace AND validator from install, always" — the
//! owner's switch of 0.6.2 is gone): a swap is signed, or a pool affirmed, only when the token's
//! topic `tm_color_<txid>_0` is in the engine's registered set (the head `<app>/topics`, written by the
//! owner's `register` / `deregister`); else refused `not_validating`. "The validator program signs
//! for any registered token." (0.3.2–0.5.0: amm-p2p's validated set,
//! `validate` / `unvalidate` in box `amm/validate`; gone.)
//!
//! The terms (0.15.0, David Case 2026-10-23: "The configs should not be published in the image.
//! This will be a dynamic, competitive marketplace."): root's message `{fn: "terms", args:
//! {lpFeeBps, validatorFeeBps, commissionBps}}` in the box `amm/validator` sets them, `{fn: "terms",
//! args: null}` clears them (the role `root` gates `terms`); the record is the head `<app>/terms`
//! (src/terms.zig), and the step sends the instance itself `{fn: "rebeat"}` in box `<app>` so the
//! engine re-declares every topic's beacon with them (skein-overlay 0.16.0). Set: a validator;
//! unset: a market only — an affirm and a swap are refused `not_validating`, "no terms: this skein
//! validates nothing" (`config.amm.ammValidator`, 0.9.0 to 0.14.2, is gone; no fallback).
//!
//! Config: the overlay's own configuration as the engine reads it
//! from the app record (skein-overlay `engine_vm.configured`: the served
//! topics, the ones registered with the engine (`<app>/topics`) among them, the app's roles,
//! `app`; the overlay engine is the app's role `overlay`, called in-VM).
const std = @import("std");
const w = @import("chain");
const ov = @import("skein_overlay");
const vm = @import("overlay_sk");
const wire = @import("wallet").wire;
const validator = @import("validator.zig");
const messages = @import("messages.zig");
const view_mod = @import("view.zig");
const Oracle = @import("oracle.zig").Oracle;
const terms = @import("terms");

const cbor = w.cbor;
const Value = cbor.Value;

pub const program_name = "amm-validator";

const signer = @import("signer");

var oracle_dummy: u8 = 0;
/// Every key and signature through the signer app (skein-signer; skein-sdk `signer.requestAs`), under
/// the name the request needs (`signer.nameFor`: `1/amm pool` for the validator's keys; none for the
/// identity key) — the kernel checks the name against this app's approved `calls` (etc/app.json).
/// The raw `wallet` import is not used.
fn oracleCall(_: *anyopaque, a: std.mem.Allocator, frame: []const u8) anyerror![]const u8 {
    const q = signer.decode(frame) catch return error.BadFrame;
    const res = try signer.requestAs(a, try signer.nameFor(a, q, null), frame);
    if (wire.errorMessage(res)) |m| std.log.err("signer: {s}", .{m});
    return res;
}

/// The validator's terms now (0.15.0, David Case 2026-10-23: a runtime record root sets; no fee
/// numbers in the image): the record of the head `<app>/terms` (src/terms.zig), or null — no head,
/// the record `null` (cleared): a market only, nothing affirmed or signed. No fallback.
///
/// (0.17.0, shruggr/skein-amm#4) No head yet: the app record's `config.overlay.terms`, the initial
/// value a token instance's create gives (src/terms.zig `current`; the engine's beat reads the same).
fn termsNow(a: std.mem.Allocator, in: Value) !?validator.Terms {
    const app = ov.calls.appOf(in);
    const s = vm.store();
    const head: ?Value = if (try vm.head(a, try terms.headName(a, app))) |rc| try s.getValue(a, rc) else null;
    const rec: ?Value = if (try vm.head(a, try std.fmt.allocPrint(a, "{s}/app", .{app}))) |rc| try s.getValue(a, rc) else null;
    return terms.current(head, rec);
}

/// The topics this instance signs for (0.6.0, shruggr/skein#120; 0.8.1, David 2026-10-09: every
/// skein is a validator, always): every topic the engine serves — the ones registered with it (the
/// head `<app>/topics`) and (0.17.0, shruggr/skein-amm#4: a token instance's topic comes from its
/// config at birth) the ones `config.overlay.topics` names; any other refused `not_validating`.
fn validatedSet(a: std.mem.Allocator, in: Value) ![]const []const u8 {
    return ov.calls.servedTopics(a, in);
}

/// The app's overlay state (`<app>/state`) over the chain app's (`chain/state`), as the step sees them.
fn overlayState(a: std.mem.Allocator, in: Value) !*ov.state.State {
    const st = try a.create(ov.state.State);
    st.* = (try ov.engine_vm.load(a, in)).st;
    st.now = @intCast(in.getUint("now") orelse 0);
    return st;
}

fn identityOf(a: std.mem.Allocator, in: Value, oracle: Oracle) ![33]u8 {
    const k = (if (in.get("self")) |me| me.getBytes("identity") else null) orelse return oracle.identity(a);
    return if (k.len == 33) k[0..33].* else error.BadInput;
}

fn run(a: std.mem.Allocator) anyerror!void {
    const raw_in = try vm.input(a);
    // A step: root's message in the box `<app>/validator` — the terms (0.15.0), answered at once.
    // 0.49.0: the wallet-funded Rescind is gone (it needs an app-initiated wallet spend, not yet
    // designed; David 2026-11-09); any other message there is refused.
    if (!std.mem.eql(u8, raw_in.getText("kind") orelse "", "call")) {
        // 0.17.0: this program's own answer to root, looped back when root is the instance's own key
        // (a one-key host, shruggr/skein#160): an answer (it names its `request`), not a request.
        if (try isOwnAnswer(a, raw_in)) return finishAnswer(a);
        if (try isTermsMessage(a, raw_in)) return termsStep(a, raw_in);
        return refuseStep(a, raw_in);
    }
    const op = validator.Op.parse(raw_in.getText("fn") orelse "") orelse return error.UnknownFunction;
    const arg = try vm.callArg(a, raw_in);
    // The configuration as the engine reads it: the app this program was installed in (the
    // matched row's program record names it), its `config.overlay`, its roles.
    const in = try ov.engine_vm.configured(a, raw_in, arg);
    // 0.51.0: the request's one step (its writes are the overlay's and the chain app's, by call).
    if (raw_in.get("step") == null) return error.BadInput;
    return directCall(a, in, arg, op);
}

fn answerBody(a: std.mem.Allocator, r: validator.Reply) !void {
    try vm.answer(a, .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "verdict", .value = .{ .text = "accept" } },
        .{ .key = "body", .value = .{ .bytes = try cbor.encode(a, try messages.replyValue(a, r)) } },
    }) });
}

// ---------------------------------------------------------------- the overlay, in-VM (0.51.0)

var overlay_in: Value = .null;

/// The app's overlay engine (its role `overlay` in the app record's `programs`), its fn `intake`.
fn intakeCall(_: *anyopaque, a: std.mem.Allocator, args: Value) anyerror!Value {
    const programs = overlay_in.get("programs") orelse return error.NoOverlayEngine;
    const engine = programs.getCid(ov.config.engine_role) orelse return error.NoOverlayEngine;
    return vm.call(a, engine, "intake", args);
}

/// The app's overlay state again (the intake's head moves read back in this step).
fn stateAfter(_: *anyopaque, a: std.mem.Allocator) anyerror!*ov.state.State {
    return overlayState(a, overlay_in);
}

/// fn `swap`, `affirm`: a frame of a direct call (above), answered in this step.
fn directCall(a: std.mem.Allocator, in: Value, arg: Value, op: validator.Op) !void {
    const transport = arg.getText("transport") orelse "";
    if (!std.mem.eql(u8, transport, "libp2p") and !std.mem.eql(u8, transport, "local")) return error.NotADirectCall;
    const oracle: Oracle = .{ .ptr = &oracle_dummy, .callFn = oracleCall };
    const identity = try identityOf(a, in, oracle);
    const body = switch (try messages.open(a, arg.getBytes("body") orelse "", identity, op)) {
        .refused => |r| return answerBody(a, r),
        .ok => |o| o.body,
    };

    const st = try overlayState(a, in);
    const ovv = try a.create(view_mod.OverlayView);
    ovv.* = .{ .st = st };
    overlay_in = in;
    const now: i64 = if (in.getUint("now")) |n| @intCast(n) else 0;
    const served = try messages.respond(a, op, body, .{
        .config = .{ .identity = identity, .terms = try termsNow(a, in), .now_ms = now, .validated = try validatedSet(a, in) },
        .view = ovv.view(),
        .oracle = oracle,
        .st = st,
        .overlay = .{ .ptr = &oracle_dummy, .intakeFn = intakeCall, .stateFn = stateAfter },
    });
    return answerBody(a, served.reply);
}

// ---------------------------------------------------------------- the terms (0.15.0)

/// Whether the step is a message that answers a request (`{fn, request, result | error}`: this
/// program's answer to root, delivered back when root's key is the instance's own) — its first step.
fn isOwnAnswer(a: std.mem.Allocator, raw_in: Value) !bool {
    if (raw_in.get("reply") != null or raw_in.get("resolved") != null) return false;
    const args = raw_in.get("args") orelse return false;
    const bc = args.getCid("body") orelse return false;
    const body = try vm.store().getValue(a, bc);
    return body.get("request") != null or body.get("replyTo") != null;
}

/// An answer delivered back: nothing to do; the thread ends.
fn finishAnswer(a: std.mem.Allocator) !void {
    _ = try vm.finish(a, vm.store(), .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "kind", .value = .{ .text = "amm-answer" } },
        .{ .key = "stage", .value = .{ .text = "ignored" } },
    }) });
}

/// Whether the step is root's message `{fn: "terms", …}` (its first step: no reply, nothing resolved).
fn isTermsMessage(a: std.mem.Allocator, raw_in: Value) !bool {
    if (raw_in.get("reply") != null or raw_in.get("resolved") != null) return false;
    const args = raw_in.get("args") orelse return false;
    const bc = args.getCid("body") orelse return false;
    return std.mem.eql(u8, (try vm.store().getValue(a, bc)).getText("fn") orelse "", terms.fn_terms);
}

/// Root's `{fn: "terms", args}` (the role `root` gates it; src/terms.zig): set (`args` the three
/// fees) or clear (`args: null`) the record under `<app>/terms`, tell the engine (`{fn: "rebeat"}`
/// to the instance itself in box `<app>`: it re-declares every topic's beacon with the terms, or
/// none), and answer `{fn: "terms", request, result: <the terms> | null}` — or `error: {code:
/// "bad-args", message}`, nothing written — to the sender in the box it wrote to.
fn termsStep(a: std.mem.Allocator, raw_in: Value) !void {
    const args = raw_in.get("args") orelse return error.BadInput;
    const in = try ov.engine_vm.configured(a, raw_in, null);
    const s = vm.store();
    const body = try s.getValue(a, args.getCid("body") orelse return error.BadInput);
    const rec: Value = switch (terms.parseArgs(body.get("args"))) {
        .bad => |why| return rootAnswer(a, args, terms.fn_terms, "error", .{ .map = try a.dupe(cbor.Entry, &.{
            .{ .key = "code", .value = .{ .text = "bad-args" } },
            .{ .key = "message", .value = .{ .text = why } },
        }) }, "amm-terms"),
        .set => |t| try terms.record(a, t),
        .clear => .null,
    };
    const app = ov.calls.appOf(in);
    const rc = try s.putValue(a, rec);
    try vm.advance(try terms.headName(a, app), rc);
    const oracle: Oracle = .{ .ptr = &oracle_dummy, .callFn = oracleCall };
    const identity = try identityOf(a, in, oracle);
    _ = try vm.send(a, &identity, app, .{ .map = try a.dupe(cbor.Entry, &.{.{ .key = "fn", .value = .{ .text = "rebeat" } }}) });
    return rootAnswer(a, args, terms.fn_terms, "result", rec, "amm-terms");
}

/// Answer root's message: `{fn, request: <the message>, <field>: v}` to its sender, in the box it
/// wrote to; then the thread ends, its result record `{kind, stage: "answered", answer}`.
fn rootAnswer(a: std.mem.Allocator, args: Value, func: []const u8, field: []const u8, v: Value, kind: []const u8) !void {
    const answer: Value = .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "fn", .value = .{ .text = func } },
        .{ .key = "request", .value = if (args.getCid("message")) |m| .{ .cid = m } else .null },
        .{ .key = field, .value = v },
    }) };
    if (args.getBytes("sender")) |sender| if (args.getText("box")) |box| {
        _ = vm.send(a, sender, box, answer) catch |e| std.log.err("{s}: the answer is not sent: {s}", .{ func, @errorName(e) });
    };
    _ = try vm.finish(a, vm.store(), .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "kind", .value = .{ .text = kind } },
        .{ .key = "stage", .value = .{ .text = "answered" } },
        .{ .key = "answer", .value = answer },
    }) });
}

// ---------------------------------------------------------------- any other message (0.49.0)

/// Root's message in box `<app>/validator` that is not `terms`: refused, `{fn, request, error:
/// {code: "unknown-fn", message}}` to its sender, nothing written. (0.13.0–0.48.x: `rescind`, the
/// validator's wallet-funded Rescind, removed in 0.49.0; the contract keeps Rescind on chain.)
fn refuseStep(a: std.mem.Allocator, raw_in: Value) !void {
    const args = raw_in.get("args") orelse return error.BadInput;
    const body = try vm.store().getValue(a, args.getCid("body") orelse return error.BadInput);
    const func = body.getText("fn") orelse "";
    return rootAnswer(a, args, func, "error", .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "code", .value = .{ .text = "unknown-fn" } },
        .{ .key = "message", .value = .{ .text = "this box takes {fn: \"terms\"} only" } },
    }) }, "amm-refused");
}

pub fn main() u8 {
    return vm.main(program_name, run);
}
