/**
 * The runtime's liveness read as the host answers it (skein docs/MESSAGES.md
 * "Liveness (#138)"): `GET <base>/.live/<topic>` → `[{sender, at, body, from}]`
 * newest first, `body` the beacon's dag-cbor `{identityKey, peerId}` in base64
 * (programs/amm-p2p liveness.zig `Body`).
 */
import { Utils } from "@bsv/sdk";
import { encode } from "cbor2";
import { parseLiveBeats, type LiveAnswer } from "../src/lib/overlay";

/** A validator's peer ID (recorded from the v2 instance). */
export const PEER = "16Uiu2HAm6mP74uTowae2xMtAKJpqw1Dt2NhcgSdXgyDoyfmkwGqq";

export interface BeatEntry {
  sender: string;
  at: number;
  body: string;
  from: string;
}

/** One entry of the read: `identityKey` beat at `at`, its body naming `peerId`. */
export function beatEntry(identityKey: string, peerId: string, at: number, sender: string = identityKey): BeatEntry {
  const body = encode({ identityKey: Uint8Array.from(Utils.toArray(identityKey, "hex")), peerId: Uint8Array.from(Utils.fromBase58(peerId)) });
  return { sender, at, body: Utils.toBase64(Array.from(body)), from: peerId };
}

/** The read's answer for `keys`, each beating 5 s before `now`, as the page parses it. */
export function liveFor(keys: string[], now = 1_000_000, peerId: string = PEER): LiveAnswer {
  return parseLiveBeats(
    keys.map((k) => beatEntry(k, peerId, now - 5_000)),
    now,
  );
}
