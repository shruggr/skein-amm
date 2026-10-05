/**
 * Pending payouts (shell, every page): the BRC-29 payouts the page built and
 * the wallet has not internalized yet (src/wallet/pendingPayouts.ts), each
 * with "Internalize now": the final transaction's BEEF from the instance's
 * lookup (`{outpoint, beef: true}`; for a swap, found by the payout's script
 * in the live pools' BEEF), then `internalizeAction` with the stored
 * remittance. The record is cleared only once the wallet accepted it.
 */
import { useEffect, useState } from "react";
import { useWallet } from "../wallet/AppWalletProvider";
import { AMM_OVERLAY } from "../lib/config";
import { internalizeNow, onPendingPayoutsChange, PendingPayoutStore, type PendingPayout } from "../wallet/pendingPayouts";

const store = new PendingPayoutStore();
const errText = (e: unknown) => (e instanceof Error ? e.message : String(e));

export function PendingPayouts() {
  const { wallet, status } = useWallet();
  const [items, setItems] = useState<PendingPayout[]>(() => store.list());
  const [busy, setBusy] = useState<string | null>(null);
  const [notes, setNotes] = useState<Record<string, { ok: boolean; text: string }>>({});

  useEffect(() => onPendingPayoutsChange(() => setItems(store.list())), []);

  if (items.length === 0) return null;
  const connected = !!wallet && status === "connected";

  async function run(p: PendingPayout) {
    if (!wallet) return;
    setBusy(p.id);
    try {
      const r = await internalizeNow(wallet, store, AMM_OVERLAY, p);
      setNotes((n) => ({ ...n, [p.id]: { ok: r.accepted, text: r.accepted ? `internalized (txid ${r.txid.slice(0, 16)}…)` : "the wallet did not accept it" } }));
    } catch (e) {
      setNotes((n) => ({ ...n, [p.id]: { ok: false, text: errText(e) } }));
    } finally {
      setBusy(null);
      setItems(store.list());
    }
  }

  return (
    <section>
      <h2>Pending payouts</h2>
      <p>
        <small>
          Sats paid to your wallet as BRC-29 payments that the wallet has not taken in yet. Each is kept in this browser
          {store.durable ? "" : " (storage is blocked here: these will not survive a reload)"} until internalizeAction succeeds.
        </small>
      </p>
      <table>
        <thead>
          <tr>
            <th>Kind</th>
            <th>Output</th>
            <th>Sats</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {items.map((p) => (
            <tr key={p.id}>
              <td>{p.kind === "swap" ? "Swap payout" : "Liquidity withdrawal"}</td>
              <td>
                <code title={p.id}>
                  {p.txid.slice(0, 12)}…:{p.vout}
                </code>
                {!p.final && <small> (provisional txid: the swap is not signed yet)</small>}
                {notes[p.id] && (
                  <>
                    <br />
                    <small className={notes[p.id]!.ok ? "ok" : "bad"}>{notes[p.id]!.text}</small>
                  </>
                )}
              </td>
              <td>{p.satoshis}</td>
              <td>
                <button type="button" onClick={() => void run(p)} disabled={!connected || busy !== null}>
                  {busy === p.id ? "Internalizing…" : "Internalize now"}
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
