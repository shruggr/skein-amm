/**
 * Deploy a Mandala token: a wallet action and nothing else (docs/notes.md
 * "Token deploy and the LP page"). One `createAction`:
 *
 * - output 0: `Mandala.deployValue(amount, …)` or `Mandala.deployAuthority(…)`,
 *   P2PKH to a wallet-derived key, payload {sym, dec, icon}; basket `bsv21`,
 *   tags and customInstructions as 1sat-sdk's `deployBsv21Mint` /
 *   `deployBsv21Auth` file them (`bsv21:deploy` [+ `bsv21:auth`], no
 *   `bsv21:<id>` and no `id` in customInstructions: the id is this outpoint);
 * - output 1, when the icon is uploaded: the image as a 1Sat ordinal
 *   inscription (basket `1sat`, tags/customInstructions as 1sat-sdk's
 *   `inscribe`) or as a 0-sat B protocol file (no basket: unspendable). The
 *   payload's icon is then the 4-byte vout 1.
 *
 * Keys: BRC-42 `getPublicKey` under 1sat-sdk's protocol (`P1SAT_PROTOCOL`),
 * counterparty self, keyID `<prefix>-<random hex>` as 1sat-sdk's
 * `resolveDestination` mints them; keyID recorded in customInstructions.
 *
 * Broadcast: 1sat-sdk's `runCreateActionPipeline` (what its actions run:
 * managed `id:` tags, createAction, signAction), the wallet broadcasts.
 */
import Mandala, { type MandalaMetadata } from "@1sat/templates/mandala";
import { buildInscriptionScript } from "@1sat/templates";
import {
  BSV21_AUTH_TAG,
  BSV21_BASKET,
  BSV21_DEPLOY_TAG,
  MAX_INSCRIPTION_BYTES,
  ORDINALS_BASKET,
  P1SAT_PROTOCOL,
  buildBsv21CustomInstructions,
  buildDataScript,
  buildOrdinalCustomInstructions,
  runCreateActionPipeline,
} from "@1sat/actions";
import {
  Hash,
  P2PKH,
  PublicKey,
  Utils,
  type CreateActionArgs,
  type CreateActionOutput,
  type WalletInterface,
  type WalletProtocol,
} from "@bsv/sdk";

export type SupplyModel = { kind: "fixed"; amount: bigint } | { kind: "authority" };

export type IconChoice =
  | { kind: "none" }
  /** One of the user's ordinals: the outpoint holding the bytes, `txid_vout`. */
  | { kind: "ordinal"; outpoint: string }
  /** A new image in the deploy transaction, output 1. */
  | { kind: "upload"; as: "ordinal" | "b"; content: Uint8Array; contentType: string };

export interface DeployRequest {
  symbol: string;
  decimals: number;
  supply: SupplyModel;
  icon: IconChoice;
}

/** A wallet-derived key: what to lock to and what to record. */
export interface DerivedKey {
  protocolID: WalletProtocol;
  keyID: string;
  /** Compressed public key, hex. */
  publicKey: string;
}

/** The deploy output's index (BRC-162: deploys are output 0). */
export const DEPLOY_VOUT = 0;
/** Where an uploaded icon goes. */
export const ICON_VOUT = 1;

/** BRC-100 descriptions are 5-50 characters. */
function describe(text: string): string {
  return text.length <= 50 ? text : `${text.slice(0, 49)}…`;
}

export function validateRequest(req: DeployRequest): void {
  if (!req.symbol.trim()) throw new Error("symbol is required");
  if (!Number.isInteger(req.decimals) || req.decimals < 0 || req.decimals > 18) {
    throw new Error("decimals must be an integer 0-18");
  }
  if (req.supply.kind === "fixed" && req.supply.amount <= 0n) throw new Error("supply must be positive");
  if (req.icon.kind === "upload") {
    if (!req.icon.contentType.startsWith("image/")) throw new Error("the icon must be an image");
    if (req.icon.content.length === 0) throw new Error("the icon file is empty");
    if (req.icon.content.length > MAX_INSCRIPTION_BYTES) {
      throw new Error(`the icon is ${req.icon.content.length} bytes; at most ${MAX_INSCRIPTION_BYTES}`);
    }
  }
}

/** The keyID prefixes 1sat-sdk uses for these outputs. */
export function keyIdPrefixes(req: DeployRequest): { token: string; icon?: string } {
  const sym = req.symbol.trim();
  return {
    token: req.supply.kind === "fixed" ? `bsv21-deploy-${sym}` : `bsv21-auth-${sym}`,
    ...(req.icon.kind === "upload" && req.icon.as === "ordinal" && { icon: "inscribe" }),
  };
}

/** The createAction arguments for a deploy, given the derived keys. Pure. */
export function buildDeployArgs(req: DeployRequest, tokenKey: DerivedKey, iconKey?: DerivedKey): CreateActionArgs {
  validateRequest(req);
  const sym = req.symbol.trim();
  const fixed = req.supply.kind === "fixed";
  const amount = req.supply.kind === "fixed" ? req.supply.amount : 0n;

  const payload: MandalaMetadata = { sym, dec: req.decimals };
  if (req.icon.kind === "ordinal") payload.icon = req.icon.outpoint;
  if (req.icon.kind === "upload") payload.icon = ICON_VOUT;

  const tokenAddress = PublicKey.fromString(tokenKey.publicKey).toAddress();
  const deploy = fixed
    ? Mandala.deployValue(amount, { lock: tokenAddress, payload })
    : Mandala.deployAuthority({ lock: tokenAddress, payload });

  const outputs: CreateActionOutput[] = [
    {
      lockingScript: deploy.lock().toHex(),
      satoshis: 1,
      outputDescription: describe(fixed ? `Deploy ${sym}` : `Deploy ${sym} auth`),
      basket: BSV21_BASKET,
      tags: fixed ? [BSV21_DEPLOY_TAG] : [BSV21_DEPLOY_TAG, BSV21_AUTH_TAG],
      customInstructions: buildBsv21CustomInstructions({
        // No `id`: the token id is this output's outpoint.
        token: {
          amt: String(amount),
          op: fixed ? "deploy+mint" : "deploy+auth",
          sym,
          dec: req.decimals,
          // The JSON field is a string; a vout pointer has no outpoint until
          // the txid exists, so only an absolute outpoint is recorded here.
          ...(req.icon.kind === "ordinal" && { icon: req.icon.outpoint }),
        },
        protocolID: tokenKey.protocolID,
        keyID: tokenKey.keyID,
      }),
    },
  ];

  if (req.icon.kind === "upload") {
    const { content, contentType } = req.icon;
    if (req.icon.as === "ordinal") {
      if (!iconKey) throw new Error("an inscribed icon needs a derived key");
      const typeBase = contentType.split(";")[0]?.trim() || contentType;
      const tags = [`type:${typeBase}`, "origin", `sha256:${Utils.toHex(Hash.sha256(Array.from(content)))}`];
      const script = buildInscriptionScript(
        new P2PKH().lock(PublicKey.fromString(iconKey.publicKey).toAddress()),
        content,
        contentType,
      );
      outputs.push({
        lockingScript: script.toHex(),
        satoshis: 1,
        outputDescription: describe(`${sym} icon inscription`),
        basket: ORDINALS_BASKET,
        tags,
        customInstructions: buildOrdinalCustomInstructions({
          protocolID: iconKey.protocolID,
          keyID: iconKey.keyID,
          tags,
        }),
      });
    } else {
      outputs.push({
        lockingScript: buildDataScript(content, contentType).toHex(),
        satoshis: 0,
        outputDescription: describe(`${sym} icon (B file)`),
      });
    }
  }

  return {
    description: describe(fixed ? `Deploy ${sym} (${amount} fixed supply)` : `Deploy ${sym} (mintable)`),
    outputs,
    // The deploy must stay output 0 and the icon output 1.
    options: { randomizeOutputs: false, acceptDelayedBroadcast: false },
  };
}

function randomHex(bytes = 8): string {
  return Utils.toHex(Array.from(crypto.getRandomValues(new Uint8Array(bytes))));
}

/** BRC-42 self key under 1sat-sdk's protocol, as `resolveDestination` derives it. */
export async function deriveSelfKey(wallet: WalletInterface, keyIDPrefix: string): Promise<DerivedKey> {
  const protocolID = P1SAT_PROTOCOL as WalletProtocol;
  const keyID = `${keyIDPrefix}-${randomHex()}`;
  const { publicKey } = await wallet.getPublicKey({ protocolID, keyID, counterparty: "self", forSelf: true });
  return { protocolID, keyID, publicKey };
}

export interface DeployResult {
  txid: string;
  /** `txid_0` */
  tokenId: string;
  args: CreateActionArgs;
}

export async function deployToken(wallet: WalletInterface, req: DeployRequest): Promise<DeployResult> {
  validateRequest(req);
  const prefixes = keyIdPrefixes(req);
  const tokenKey = await deriveSelfKey(wallet, prefixes.token);
  const iconKey = prefixes.icon ? await deriveSelfKey(wallet, prefixes.icon) : undefined;
  const args = buildDeployArgs(req, tokenKey, iconKey);
  const result = await runCreateActionPipeline(wallet, args, []);
  if (result.error) throw new Error(result.error);
  if (!result.txid) throw new Error("the wallet returned no txid");
  return { txid: result.txid, tokenId: `${result.txid}_${DEPLOY_VOUT}`, args };
}
