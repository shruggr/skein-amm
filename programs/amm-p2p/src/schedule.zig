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
//! What makes the program emit the requests is a message to box `amm-p2p`,
//! `{kind: "amm-p2p-start", jobs?: ["heartbeat" | "catchup"]}` (or
//! `amm-p2p-stop`), from the owner or from the cron provider (`skein-host
//! event <agent> amm-p2p '{"kind":"amm-p2p-start"}'`): nothing in skein
//! steps a program at install or start (README.md, "Scheduling").
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

/// The jobs a start/stop message names: its `jobs`, else every job a node
/// runs (the heartbeat only when it is a validator: `validator`).
pub fn jobsOf(a: Allocator, body: Value, validator: bool) ![]const Job {
    if (body.getArray("jobs")) |js| {
        const out = try a.alloc(Job, js.len);
        for (js, out) |j, *o| o.* = std.meta.stringToEnum(Job, if (j == .text) j.text else return error.BadJobs) orelse return error.BadJobs;
        return out;
    }
    return if (validator) &.{ .heartbeat, .catchup } else &.{.catchup};
}
