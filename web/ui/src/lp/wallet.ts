/**
 * The wallet reads behind the LP page (BRC-100 `listOutputs`, the connected
 * wallet from @1sat/react's `useWallet()`). Nothing here talks to an overlay.
 */
import { BSV21_BASKET, BSV21_DEPLOY_TAG, ORDINALS_BASKET } from "@1sat/types";
import { Beef, type Transaction, type WalletInterface, type WalletOutput } from "@bsv/sdk";

/** BRC-100's largest `limit`. */
const LIMIT = 10000;

export interface WalletAssets {
  /** `bsv21` basket, with locking scripts, tags and customInstructions. */
  tokenRows: WalletOutput[];
  /** Ordinals basket (`1sat`), with locking scripts and tags. */
  ordinalRows: WalletOutput[];
  /** Deploy transactions (and their BEEF ancestors) by txid, for icons by vout. */
  txs: Map<string, Transaction>;
  /** Reads that failed without failing the page (e.g. no BEEF from the wallet). */
  warnings: string[];
}

export async function loadWalletAssets(wallet: WalletInterface): Promise<WalletAssets> {
  const warnings: string[] = [];
  const [tokens, ordinals] = await Promise.all([
    wallet.listOutputs({
      basket: BSV21_BASKET,
      include: "locking scripts",
      includeTags: true,
      includeCustomInstructions: true,
      limit: LIMIT,
    }),
    wallet.listOutputs({
      basket: ORDINALS_BASKET,
      include: "locking scripts",
      includeTags: true,
      includeCustomInstructions: true,
      limit: LIMIT,
    }),
  ]);

  // Icons stored as a vout point into the deploy transaction; the deploy
  // outputs are tagged `bsv21:deploy` (1sat-sdk's deploy filing), so ask for
  // just those with their transactions.
  const txs = new Map<string, Transaction>();
  try {
    const deploys = await wallet.listOutputs({
      basket: BSV21_BASKET,
      tags: [BSV21_DEPLOY_TAG],
      include: "entire transactions",
      limit: LIMIT,
    });
    if (deploys.outputs.length > 0 && deploys.BEEF) {
      const beef = Beef.fromBinary(Array.from(deploys.BEEF));
      for (const btx of beef.txs) if (btx.tx) txs.set(btx.txid, btx.tx);
    }
  } catch (err) {
    warnings.push(`deploy transactions: ${err instanceof Error ? err.message : String(err)}`);
  }

  return { tokenRows: tokens.outputs, ordinalRows: ordinals.outputs, txs, warnings };
}
