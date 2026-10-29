//! amm-config's decisions, natively (0.17.0, shruggr/skein-amm#4): the deploy check, the create's
//! body and config, the enable list's rows and its read, the defaults, and the config
//! app's manifest.
const std = @import("std");
const w = @import("chain");
const tokens = @import("src/tokens.zig");
const discovery = @import("src/discovery.zig");
const vec = @import("vectors");
const dagjson = @import("dagjson");
const scbor = @import("sdk_cbor");
const config_manifest = @import("config_manifest").json;

const testing = std.testing;
const cbor = w.cbor;
const Value = cbor.Value;
const Allocator = std.mem.Allocator;

fn unhex(a: Allocator, h: []const u8) []u8 {
    const out = a.alloc(u8, h.len / 2) catch @panic("OOM");
    _ = std.fmt.hexToBytes(out, h) catch @panic("bad hex");
    return out;
}

/// An Atomic BEEF of one raw transaction (no proof: the door's business, not this check's).
fn atomic(a: Allocator, raw: []const u8) ![]u8 {
    const txid = w.beef.txidOf(raw);
    var entries = [_]w.beef.Entry{.{ .txid = txid, .format = .raw, .raw = raw }};
    return w.beef.serialize(a, .{ .version = w.beef.V2, .atomic = txid, .bumps = &.{}, .entries = &entries });
}

fn displayHex(a: Allocator, txid: [32]u8) ![]const u8 {
    return a.dupe(u8, &w.header.toHex(txid));
}

test "the deploy check: the BEEF's subject is the token id's transaction and its output a deploy; else refused" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const raw = unhex(a, vec.token_deploy);
    const txid = try displayHex(a, w.beef.txidOf(raw));
    const beef = try atomic(a, raw);
    const id = try std.fmt.allocPrint(a, "{s}_0", .{txid});
    const ok = try tokens.deployOf(a, id, beef);
    try testing.expect(ok == .ok);
    try testing.expectEqualStrings(id, ok.ok.token_id);
    try testing.expectEqualStrings(try std.fmt.allocPrint(a, "tm_color_{s}_0", .{txid}), ok.ok.topic);
    try testing.expectEqualStrings(try std.fmt.allocPrint(a, "ls_color_{s}_0", .{txid}), ok.ok.lookup);
    // Any id form: the canonical text.
    try testing.expectEqualStrings(id, (try tokens.canonicalId(a, txid)).?);
    try testing.expectEqualStrings(id, (try tokens.canonicalId(a, try std.fmt.allocPrint(a, "{s}.0", .{txid}))).?);
    try testing.expect((try tokens.canonicalId(a, "abc")) == null);
    // Another transaction's id: refused.
    const other = try std.fmt.allocPrint(a, "{s}_0", .{"ab" ** 32});
    try testing.expect((try tokens.deployOf(a, other, beef)) == .refused);
    // No such output.
    try testing.expect((try tokens.deployOf(a, try std.fmt.allocPrint(a, "{s}_7", .{txid}), beef)) == .refused);
    // Not a deploy: the funding transaction's output.
    const fund = unhex(a, vec.fund);
    const fbeef = try atomic(a, fund);
    const fid = try std.fmt.allocPrint(a, "{s}_0", .{try displayHex(a, w.beef.txidOf(fund))});
    const nd = try tokens.deployOf(a, fid, fbeef);
    try testing.expect(nd == .refused);
    // Not a BEEF.
    try testing.expect((try tokens.deployOf(a, id, "junk")) == .refused);
    // A BRC-161 deploy at another output: legacy_deploy1's output 1? (its deploy is where the vectors put it)
    const l1 = unhex(a, vec.legacy_deploy0);
    const l1id = try std.fmt.allocPrint(a, "{s}_0", .{try displayHex(a, w.beef.txidOf(l1))});
    try testing.expect((try tokens.deployOf(a, l1id, try atomic(a, l1))) == .ok);
}

test "the create: {handle: <tokenId>, image, config: {amm: {overlay: {topics, lookups, terms?}, amm-p2p: {market?}}}, beef}" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const d: tokens.Deploy = .{ .token_id = "aa_0", .topic = "tm_color_aa_0", .lookup = "ls_color_aa_0", .sym = "AMM", .dec = 5 };
    const plain = try tokens.createBody(a, d, "amm-token", null, "BEEF");
    try testing.expectEqualStrings("aa_0", plain.getText("handle").?);
    try testing.expectEqualStrings("amm-token", plain.getText("image").?);
    try testing.expectEqualStrings("BEEF", plain.getBytes("beef").?);
    const ov = plain.get("config").?.get("amm").?.get("overlay").?;
    try testing.expectEqualStrings("color-topic", ov.get("topics").?.getText("tm_color_aa_0").?);
    const ls = ov.get("lookups").?;
    // 0.21.0: the token's own ls_amm_<id> and ls_color_<id> only (no ls_amm, no ls_color, no ls_color_deploys).
    try testing.expectEqual(@as(usize, 2), ls.map.len);
    for ([_][]const u8{ "ls_amm_aa_0", "ls_color_aa_0" }) |k| try testing.expect(ls.get(k) != null);
    for ([_][]const u8{ "ls_amm", "ls_color", "ls_color_deploys" }) |k| try testing.expect(ls.get(k) == null);
    try testing.expectEqualStrings("color-lookup", ls.get("ls_color_aa_0").?.getText("program").?);
    try testing.expectEqualStrings("amm-lookup", ls.get("ls_amm_aa_0").?.getText("program").?);
    try testing.expect(ls.get("ls_amm_aa_0").?.get("topics") == null);
    try testing.expectEqualStrings("tm_color_aa_0", ls.get("ls_color_aa_0").?.getArray("topics").?[0].text);
    try testing.expect(ov.get("terms") == null);
    try testing.expect(plain.get("config").?.get("amm").?.get("amm-p2p") == null);
    // With defaults: terms and market as the initial values.
    const df = switch (try tokens.withDefaults(a, null, try dagjsonValue(a, "{\"terms\":{\"lpFeeBps\":30,\"validatorFeeBps\":5,\"commissionBps\":25},\"market\":{\"minCommissionBps\":1}}"))) {
        .ok => |x| x,
        .bad => |why| {
            std.debug.print("{s}\n", .{why});
            return error.Bad;
        },
    };
    const with = try tokens.createBody(a, d, "amm-token", df, "BEEF");
    const amm = with.get("config").?.get("amm").?;
    try testing.expectEqual(@as(u64, 25), amm.get("overlay").?.get("terms").?.getUint("commissionBps").?);
    try testing.expectEqual(@as(u64, 1), amm.get("amm-p2p").?.get("market").?.getUint("minCommissionBps").?);
    try testing.expectEqualStrings("aa_0", (try tokens.handleBody(a, "aa_0")).getText("handle").?);
    // shruggr/skein#137: the create names the topics the host routes to it; so does a start.
    const ts = plain.getArray("topics").?;
    try testing.expectEqual(@as(usize, 3), ts.len);
    try testing.expectEqualStrings("tm_color_aa_0", ts[0].text);
    try testing.expectEqualStrings("ls_color_aa_0", ts[1].text);
    try testing.expectEqualStrings("ls_amm_aa_0", ts[2].text);
    try testing.expect((try tokens.handleBody(a, "aa_0")).get("topics") == null);
    const id = "959bebef" ++ "00" ** 28 ++ "_0";
    const st = (try tokens.startBody(a, id)).?;
    try testing.expectEqualStrings(id, st.getText("handle").?);
    try testing.expectEqualStrings("tm_color_" ++ id, st.getArray("topics").?[0].text);
    try testing.expectEqualStrings("ls_color_" ++ id, st.getArray("topics").?[1].text);
    try testing.expectEqualStrings("ls_amm_" ++ id, st.getArray("topics").?[2].text);
    // A start names the canonical id whatever form it was given (`<txid>` alone → `_0`).
    const bare = (try tokens.startBody(a, "959bebef" ++ "00" ** 28)).?;
    try testing.expectEqualStrings("ls_amm_" ++ id, bare.getArray("topics").?[2].text);
    try testing.expect((try tokens.startBody(a, "not-a-token")) == null);
}

fn dagjsonValue(a: Allocator, text: []const u8) !Value {
    return cbor.decode(a, try scbor.encode(a, try dagjson.decode(a, text)));
}

test "the defaults: partial, null clears one, a bad shape refused" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const one = (try tokens.withDefaults(a, null, try dagjsonValue(a, "{\"market\":{\"minCommissionBps\":3}}"))).ok;
    try testing.expect(one.get("terms") == null);
    const two = (try tokens.withDefaults(a, one, try dagjsonValue(a, "{\"terms\":{\"lpFeeBps\":30,\"validatorFeeBps\":5,\"commissionBps\":0}}"))).ok;
    try testing.expectEqual(@as(u64, 3), two.get("market").?.getUint("minCommissionBps").?);
    try testing.expectEqual(@as(u64, 30), two.get("terms").?.getUint("lpFeeBps").?);
    const dj = try tokens.defaultsJson(a, two);
    try testing.expect(std.mem.indexOf(u8, dj, "\"minCommissionBps\":3") != null and std.mem.indexOf(u8, dj, "\"lpFeeBps\":30") != null);
    try testing.expectEqualStrings("{}", try tokens.defaultsJson(a, null));
    const three = (try tokens.withDefaults(a, two, try dagjsonValue(a, "{\"market\":null}"))).ok;
    try testing.expect(three.get("market") == null and three.get("terms") != null);
    try testing.expect((try tokens.withDefaults(a, null, try dagjsonValue(a, "{\"terms\":{\"lpFeeBps\":30}}"))) == .bad);
    try testing.expect((try tokens.withDefaults(a, null, try dagjsonValue(a, "{\"terms\":{\"lpFeeBps\":30,\"validatorFeeBps\":5,\"commissionBps\":20000}}"))) == .bad);
    try testing.expect((try tokens.withDefaults(a, null, try dagjsonValue(a, "{\"other\":1}"))) == .bad);
    try testing.expect((try tokens.withDefaults(a, null, null)) == .bad);
}

test "the enable list: creating → enabled (url, delivered) or failed; stopped; newest first" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const d1: tokens.Deploy = .{ .token_id = "aa_0", .topic = "tm_color_aa_0", .lookup = "ls_color_aa_0", .sym = "A", .dec = 2 };
    const d2: tokens.Deploy = .{ .token_id = "bb_0", .topic = "tm_color_bb_0", .lookup = "ls_color_bb_0", .icon = "data:image/png;base64,AA==" };
    var rec = try tokens.withRow(a, &.{}, try tokens.creatingRow(a, d1, 100));
    rec = try tokens.withRow(a, tokens.rowsOf(rec), try tokens.creatingRow(a, d2, 200));
    try testing.expectEqual(@as(usize, 2), tokens.rowsOf(rec).len);
    const r1 = tokens.find(tokens.rowsOf(rec), "aa_0").?;
    try testing.expectEqual(tokens.Status.creating, tokens.statusOf(r1).?);
    const ok = try dagjsonValue(a, "{\"handle\":\"aa_0\",\"url\":\"http://h/@aa_0\",\"delivered\":{\"verdict\":\"accept\"}}");
    const e1 = try tokens.answered(a, r1, ok, false);
    try testing.expectEqual(tokens.Status.enabled, tokens.statusOf(e1).?);
    try testing.expectEqualStrings("http://h/@aa_0", e1.getText("url").?);
    try testing.expectEqualStrings("accept", e1.get("delivered").?.getText("verdict").?);
    rec = try tokens.withRow(a, tokens.rowsOf(rec), e1);
    try testing.expectEqual(@as(usize, 2), tokens.rowsOf(rec).len);
    const failed = try tokens.answered(a, tokens.find(tokens.rowsOf(rec), "bb_0").?, try dagjsonValue(a, "{\"error\":\"handle taken\"}"), false);
    try testing.expectEqual(tokens.Status.failed, tokens.statusOf(failed).?);
    try testing.expectEqualStrings("handle taken", failed.getText("reason").?);
    // A start's failure leaves it stopped.
    try testing.expectEqual(tokens.Status.stopped, tokens.statusOf(try tokens.answered(a, e1, try dagjsonValue(a, "{\"error\":\"x\"}"), true)).?);
    const stopped = try tokens.updated(a, e1, .stopped, &.{}, &.{"reason"});
    try testing.expectEqualStrings("http://h/@aa_0", stopped.getText("url").?);
    rec = try tokens.withRow(a, tokens.rowsOf(rec), failed);
    const json = try tokens.tokensJson(a, tokens.rowsOf(rec));
    // Newest first: bb_0 (200) before aa_0 (100).
    try testing.expect(std.mem.indexOf(u8, json, "\"bb_0\"").? < std.mem.indexOf(u8, json, "\"aa_0\"").?);
    try testing.expect(std.mem.indexOf(u8, json, "\"status\":\"enabled\"") != null);
    try testing.expect(std.mem.indexOf(u8, json, "\"url\":\"http://h/@aa_0\"") != null);
    try testing.expect(std.mem.indexOf(u8, json, "\"since\":100") != null);
}

test "answers: a body naming its request is an answer, not a request" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const msg = &w.store.hashCid(.tx, .{1} ** 32);
    const ans = try tokens.answer(a, "enable", msg, .null);
    try testing.expect(tokens.isAnswer(ans));
    try testing.expect(tokens.isAnswer(try tokens.refusal(a, "enable", msg, "bad-args", "no")));
    try testing.expect(!tokens.isAnswer(try dagjsonValue(a, "{\"fn\":\"enable\",\"args\":{\"tokenId\":\"x\"}}")));
}

test "the config app's manifest: amm; root's enable, disable, defaults, discovery; admin behind kernel.beef; the reads; the pages" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const m = try dagjson.decode(a, config_manifest);
    try testing.expectEqualStrings("amm", scbor.Value.str(m.get("name")).?);
    const root = m.get("roles").?.get("root").?.array;
    try testing.expectEqual(@as(usize, 4), root.len);
    for ([_][]const u8{ "enable", "disable", "defaults", "discovery" }, root) |x, g| try testing.expectEqualStrings(x, scbor.Value.str(g).?);
    try testing.expectEqualStrings("amm-token", scbor.Value.str(m.get("config").?.get("amm-config").?.get("tokenImage")).?);
    try testing.expectEqualStrings("chain/1", scbor.Value.str(m.get("requires").?.array[0]).?);
    // Only its own program: no token programs in this tree.
    try testing.expectEqual(@as(usize, 1), m.get("programs").?.map.len);
    var seen: u8 = 0;
    for (m.get("routes").?.array) |r| {
        const t = scbor.Value.str(r.get("transport")) orelse "mailbox";
        const addr = scbor.Value.str(r.get("address")).?;
        if (std.mem.eql(u8, t, "mailbox") and std.mem.eql(u8, addr, "admin")) {
            try testing.expectEqualStrings("kernel.beef", scbor.Value.str(r.get("filters").?.array[0]).?);
            seen |= 1;
        }
        // 0.19.0: no listing requests (the box amm/requests and /requests are gone).
        try testing.expect(!std.mem.eql(u8, addr, "requests") and !std.mem.eql(u8, addr, "/requests"));
        if (std.mem.eql(u8, t, "http") and std.mem.eql(u8, addr, "/tokens")) seen |= 4;
        if (std.mem.eql(u8, t, "http") and std.mem.eql(u8, addr, "/")) seen |= 16;
        if (std.mem.eql(u8, t, "http") and std.mem.eql(u8, addr, "/defaults")) seen |= 32;
        if (std.mem.eql(u8, t, "http") and std.mem.eql(u8, addr, "/discovery")) {
            try testing.expectEqualStrings("discovery", scbor.Value.str(r.get("filters").?.array[0]).?);
            seen |= 64;
        }
    }
    try testing.expectEqual(@as(u8, 1 | 4 | 16 | 32 | 64), seen);
    try testing.expect(m.get("filters").?.get("requests") == null);
    try testing.expectEqualStrings("amm-config.discovery", scbor.Value.str(m.get("filters").?.get("discovery")).?);
    try testing.expectEqualStrings(tokens.admin_box, "amm/admin");
}

test "discovery (0.20.0): the create names tm_color and ls_color over it, nothing else; no beef" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const c = try discovery.createBody(a, "amm-token");
    try testing.expectEqualStrings("discovery", c.getText("handle").?);
    try testing.expectEqualStrings("amm-token", c.getText("image").?);
    try testing.expect(c.get("beef") == null);
    const amm = c.get("config").?.get("amm").?;
    try testing.expectEqual(@as(usize, 1), amm.map.len); // no amm-p2p: no market
    const ov = amm.get("overlay").?;
    try testing.expectEqual(@as(usize, 2), ov.map.len); // topics, lookups: no terms
    try testing.expectEqual(@as(usize, 1), ov.get("topics").?.map.len);
    try testing.expectEqualStrings("color-topic", ov.get("topics").?.getText("tm_color").?);
    const ls = ov.get("lookups").?;
    try testing.expectEqual(@as(usize, 1), ls.map.len);
    try testing.expectEqualStrings("color-lookup", ls.get("ls_color").?.getText("program").?);
    try testing.expectEqual(@as(usize, 1), ls.get("ls_color").?.getArray("topics").?.len);
    try testing.expectEqualStrings("tm_color", ls.get("ls_color").?.getArray("topics").?[0].text);
    try testing.expect(ls.get("ls_color_deploys") == null);
    try testing.expectEqualStrings("discovery", (try discovery.handleBody(a)).getText("handle").?);
    // shruggr/skein#137: the create and a start name the topics the host routes to it; a stop does not.
    for ([_]Value{ c, try discovery.startBody(a) }) |b| {
        const ts = b.getArray("topics").?;
        try testing.expectEqual(@as(usize, 2), ts.len);
        try testing.expectEqualStrings("tm_color", ts[0].text);
        try testing.expectEqualStrings("ls_color", ts[1].text);
    }
    try testing.expectEqualStrings("discovery", (try discovery.startBody(a)).getText("handle").?);
    try testing.expect((try discovery.handleBody(a)).get("topics") == null);
}

test "discovery (0.18.0): on/off decided from the record; the manager's answers move it; the read" {
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try testing.expectEqualStrings("{\"on\":false}", try discovery.json(a, null));
    try testing.expect(discovery.onArg(try dagjsonValue(a, "{\"on\":true}")).?);
    try testing.expect(!discovery.onArg(try dagjsonValue(a, "{\"on\":false}")).?);
    try testing.expect(discovery.onArg(try dagjsonValue(a, "{\"on\":1}")) == null);
    try testing.expect(discovery.onArg(null) == null);
    // Off with nothing: refused. On with nothing: create.
    try testing.expect((try discovery.decide(a, null, false)) == .refused);
    try testing.expect((try discovery.decide(a, null, true)) == .create);
    const creating = try discovery.asked(a, null, .create, 100);
    try testing.expectEqual(discovery.Status.creating, discovery.statusOf(creating).?);
    try testing.expect(creating.get("on").?.boolean);
    try testing.expect((try discovery.decide(a, creating, true)) == .refused);
    try testing.expect((try discovery.decide(a, creating, false)) == .refused);
    // Created: enabled, its url.
    const enabled = try discovery.answered(a, creating, try dagjsonValue(a, "{\"handle\":\"discovery\",\"url\":\"https://swap.example/@discovery\"}"));
    try testing.expectEqual(discovery.Status.enabled, discovery.statusOf(enabled).?);
    try testing.expectEqualStrings("https://swap.example/@discovery", enabled.getText("url").?);
    try testing.expectEqual(@as(u64, 100), enabled.getUint("since").?);
    const j = try discovery.json(a, enabled);
    try testing.expect(std.mem.indexOf(u8, j, "\"on\":true") != null and std.mem.indexOf(u8, j, "\"status\":\"enabled\"") != null and std.mem.indexOf(u8, j, "\"url\":\"https://swap.example/@discovery\"") != null);
    try testing.expect(std.mem.indexOf(u8, j, "kind") == null);
    try testing.expect((try discovery.decide(a, enabled, true)) == .refused);
    // Off: stop → stopped (the url kept); on again: start → enabled.
    try testing.expect((try discovery.decide(a, enabled, false)) == .stop);
    const stopping = try discovery.asked(a, enabled, .stop, 200);
    try testing.expect(!stopping.get("on").?.boolean);
    const stopped = try discovery.answered(a, stopping, try dagjsonValue(a, "{\"handle\":\"discovery\",\"stopped\":true}"));
    try testing.expectEqual(discovery.Status.stopped, discovery.statusOf(stopped).?);
    try testing.expectEqualStrings("https://swap.example/@discovery", stopped.getText("url").?);
    try testing.expect((try discovery.decide(a, stopped, false)) == .refused);
    try testing.expect((try discovery.decide(a, stopped, true)) == .start);
    const starting = try discovery.asked(a, stopped, .start, 300);
    const again = try discovery.answered(a, starting, try dagjsonValue(a, "{\"handle\":\"discovery\",\"started\":true,\"url\":\"https://swap.example/@discovery\"}"));
    try testing.expectEqual(discovery.Status.enabled, discovery.statusOf(again).?);
    // Failures: a create → failed (on again creates); a start → stopped; a stop → enabled; each with its reason.
    const failed = try discovery.answered(a, creating, try dagjsonValue(a, "{\"error\":\"no image amm-token\"}"));
    try testing.expectEqual(discovery.Status.failed, discovery.statusOf(failed).?);
    try testing.expectEqualStrings("no image amm-token", failed.getText("reason").?);
    try testing.expect((try discovery.decide(a, failed, true)) == .create);
    try testing.expect((try discovery.decide(a, failed, false)) == .refused);
    try testing.expect(discovery.answered(a, failed, .null) == error.BadState);
    try testing.expectEqual(discovery.Status.stopped, discovery.statusOf(try discovery.answered(a, starting, try dagjsonValue(a, "{\"error\":\"x\"}"))).?);
    try testing.expectEqual(discovery.Status.enabled, discovery.statusOf(try discovery.answered(a, stopping, try dagjsonValue(a, "{\"error\":\"x\"}"))).?);
    // A new ask drops the earlier reason.
    try testing.expect((try discovery.asked(a, failed, .create, 400)).get("reason") == null);
}
