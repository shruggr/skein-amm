//! Getting a signed transaction into this instance's own overlay: the
//! validator only adds its signature; the submission itself is the overlay
//! engine's own (skein-overlay 0.6.0 src/submit.zig), the same `submit.route`
//! that `POST /submit` runs (src/routes.zig `submitRouted`). `Route.route`
//! hands it the signed BEEF — the request's BEEF (the taker's ancestry and
//! BUMPs) with its subject replaced by the signed transaction
//! (`submissionBeef`), as bytes — for the token's topic `tm_<txid>`, in the
//! front door's step on the direct call: decoded once into records in the
//! step's write cache, checked against the chain app's headers and SPV
//! (state.zig `verifyDecoded`, the chain state read only) and judged by the
//! topic's own program (fn `identify`). What it makes is `submit.Routed`: the
//! submit event, or nothing new (`pending`, `unchanged`: a resubmission), or
//! a refusal.
//!
//! As `POST /submit` does with the event, main.zig launches the engine's
//! submission thread on it (args `{event, box: "submit"}`) and answers
//! `{wait: true}`: the thread hands the BEEF to the chain app (`ingest`,
//! which registers and broadcasts every unproven transaction it carries) and
//! admits on the chain app's first `accepted` or `proven`; the direct call's
//! answer is read from the state when that thread comes to rest.
const std = @import("std");
const w = @import("chain");
const ov = @import("skein_overlay");
const sksubmit = ov.submit;

const cbor = w.cbor;
const Value = cbor.Value;
const beef = w.beef;

pub const Routed = sksubmit.Routed;

/// The engine's `submit.route`, over this step's overlay state: `caller`
/// reaches the topic's program (fn `identify`; skein-overlay's
/// `engine_vm.caller()` in a program), `st` is the app's overlay state over
/// the chain state, loaded for the handler (the route decodes the BEEF into
/// the step's write cache), `in` the handler's input as the engine reads its
/// configuration (`engine_vm.configured`: `defaults.overlayTopics` with the
/// topics registered with the engine in, `programs` the app's roles).
pub const Route = struct {
    caller: ov.calls.Caller,
    st: *ov.state.State,
    in: Value,

    pub fn route(self: Route, a: std.mem.Allocator, signed_beef: []const u8, topic: []const u8) !Routed {
        return sksubmit.route(a, self.caller, self.st, self.in, .{ .bytes = signed_beef }, try a.dupe([]const u8, &.{topic}), null, null);
    }
};

/// The signed BEEF, what a submission carries: the request's with its
/// subject replaced by the signed transaction (same place, parents first,
/// its BUMPs kept), or a V1 BEEF of the signed transaction alone (a raw
/// request), which an overlay accepts only if it holds every parent
/// (docs/OVERLAY.md: "every other transaction's inputs come from a
/// transaction decoded before it or held").
pub fn submissionBeef(a: std.mem.Allocator, signed: []const u8, ancestry: ?beef.Beef) ![]u8 {
    const txid = beef.txidOf(signed);
    const parsed = try w.bsvz.transaction.Transaction.parse(a, signed);
    const b = ancestry orelse return beef.serialize(a, .{
        .version = beef.V1,
        .bumps = &.{},
        .entries = try a.dupe(beef.Entry, &.{.{ .txid = txid, .format = .raw, .raw = signed, .tx = parsed }}),
    });
    const subject = b.subject() orelse return error.InvalidBeef;
    const entries = try a.dupe(beef.Entry, b.entries);
    const i = b.indexOf(subject) orelse return error.InvalidBeef;
    entries[i] = .{ .txid = txid, .format = .raw, .raw = signed, .tx = parsed };
    var out = b;
    out.entries = entries;
    if (out.atomic != null) out.atomic = txid;
    return beef.serialize(a, out);
}
