//! The periodic jobs under skein#69 (docs/MESSAGES.md "Scheduling: the
//! waker and the cron provider"): a schedule is a message to the cron
//! provider (the address book's role `cron`, box `cron`), emitted from a
//! step, never host configuration:
//!
//!   {fn: "tick", every: <ms>, box: "amm-p2p", body: {kind: "amm-p2p-tick", job}, name: "amm-p2p-<job>"}
//!   {fn: "stop", name: "amm-p2p-<job>"}
//!
//! Each tick comes back as a message from the provider's key into box
//! `amm-p2p`, its body `{kind: "amm-p2p-tick", job, name, due}`, and
//! launches a thread of this program. A tick request replaces the schedule
//! of the same name, so asking again is harmless.
//!
//! The fallback since 0.2.0: the heartbeat is a `beacon` the host publishes
//! (main.zig `beacons`), and the catch-up pass is a utility no start
//! schedules. A start that names its jobs asks the cron provider (the
//! address book's entry at `local` `cron`) as before: `{kind:
//! "amm-p2p-start", jobs: ["heartbeat" | "catchup"]}` (or `amm-p2p-stop`),
//! from the owner or from the cron provider.
//! No VM imports.
const std = @import("std");
const w = @import("chain");

const cbor = w.cbor;
const Value = cbor.Value;
const Allocator = std.mem.Allocator;

pub const Job = enum { heartbeat, catchup };

pub const tick_kind = "amm-p2p-tick";
pub const start_kind = "amm-p2p-start";
pub const stop_kind = "amm-p2p-stop";

/// The schedule's name at the cron provider.
pub fn name(job: Job) []const u8 {
    return switch (job) {
        .heartbeat => "amm-p2p-heartbeat",
        .catchup => "amm-p2p-catchup",
    };
}

/// The tick request for `job`, every `every_ms`, into `box`.
pub fn tick(a: Allocator, job: Job, every_ms: u64, box: []const u8) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "fn", .value = .{ .text = "tick" } },
        .{ .key = "every", .value = .{ .uint = every_ms } },
        .{ .key = "box", .value = .{ .text = box } },
        .{ .key = "body", .value = .{ .map = try a.dupe(cbor.Entry, &.{
            .{ .key = "kind", .value = .{ .text = tick_kind } },
            .{ .key = "job", .value = .{ .text = @tagName(job) } },
        }) } },
        .{ .key = "name", .value = .{ .text = name(job) } },
    }) };
}

pub fn stop(a: Allocator, job: Job) !Value {
    return .{ .map = try a.dupe(cbor.Entry, &.{
        .{ .key = "fn", .value = .{ .text = "stop" } },
        .{ .key = "name", .value = .{ .text = name(job) } },
    }) };
}

/// The job a tick's body names.
pub fn jobOf(body: Value) !Job {
    if (!std.mem.eql(u8, body.getText("kind") orelse "", tick_kind)) return error.NotATick;
    return std.meta.stringToEnum(Job, body.getText("job") orelse return error.BadTick) orelse error.BadTick;
}

/// The jobs a start/stop message names (its `jobs`): the cron fallback. A start without `jobs`
/// is the beacon's (main.zig), never a schedule.
pub fn jobsOf(a: Allocator, body: Value) ![]const Job {
    const js = body.getArray("jobs") orelse return error.NoJobs;
    const out = try a.alloc(Job, js.len);
    for (js, out) |j, *o| o.* = std.meta.stringToEnum(Job, if (j == .text) j.text else return error.BadJobs) orelse return error.BadJobs;
    return out;
}
