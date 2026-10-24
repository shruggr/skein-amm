/**
 * Settings, root only: register a token picked from discovery (topic and
 * lookup names derived, never typed), the registered list with Deregister,
 * and holders' requests to list; and (0.15.0) the validator's terms: shown,
 * set or cleared by root's message to `<app>/validator`; and (0.16.0) the
 * market's terms, its minimum commission: shown, set or cleared by root's
 * message to `<app>/amm-p2p`. Discovery is always on: no toggles.
 */
import { useState } from "react";
import { Id } from "../components/Id";
import { shortTokenId } from "../lib/tokenId";
import {
  NO_TERMS_TEXT,
  clearMarketMinimum,
  clearValidatorTerms,
  deregisterToken,
  hasTerms,
  setMarketMinimum,
  setValidatorTerms,
  useMarketMinimum,
  useValidatorTerms,
  lookupOf,
  registerToken,
  topicOf,
  useDiscoveryTokens,
  useListingRequests,
  useRegisteredTokens,
  type RegisteredToken,
  type Session,
} from "../data/exchange";
import { ago } from "./format";
import { NeedWallet, Notice, TokenName } from "./bits";

function useAct(): [string | null, boolean, (f: () => Promise<void>) => void] {
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const run = (f: () => Promise<void>) => {
    setBusy(true);
    setError(null);
    f()
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setBusy(false));
  };
  return [error, busy, run];
}

function Register() {
  const disc = useDiscoveryTokens(true);
  const [pick, setPick] = useState("");
  const [error, busy, run] = useAct();
  const [done, setDone] = useState<string | null>(null);
  const list = disc.data ?? [];
  const t = list.find((x) => x.tokenId === pick) ?? list[0];

  return (
    <form
      className="panel stack-sm"
      onSubmit={(e) => {
        e.preventDefault();
        if (!t) return;
        const sym = t.sym;
        setDone(null);
        run(async () => {
          await registerToken(t.tokenId);
          setDone(`${sym} is registered.`);
        });
      }}
    >
      <h2 className="h2">Register a token</h2>
      {disc.data === null ? (
        <p className="muted">Reading discovery…</p>
      ) : list.length === 0 ? (
        <p className="muted">Every token in discovery is registered here.</p>
      ) : (
        <>
          <div className="row row-wrap">
            <label htmlFor="reg-pick" className="sr-only">
              Token from discovery
            </label>
            <select id="reg-pick" className="mono grow" value={t?.tokenId ?? ""} onChange={(e) => setPick(e.target.value)}>
              {list.map((d) => (
                <option key={d.tokenId} value={d.tokenId}>
                  {d.sym} · {shortTokenId(d.tokenId)}
                  {d.seenAt ? ` · in discovery ${ago(d.seenAt)}` : ""}
                </option>
              ))}
            </select>
            <button type="submit" className="btn btn-primary" disabled={busy || !t}>
              {busy ? "Registering…" : "Register"}
            </button>
          </div>
          {t && (
            <dl className="kv kv-left small">
              <dt>Token id</dt>
              <dd>
                <Id value={t.tokenId} kind="token" label={`${t.sym} token id`} />
              </dd>
              <dt>Topic</dt>
              <dd>
                <Id value={topicOf(t.tokenId)} kind="topic" label="topic" />
              </dd>
              <dt>Lookup</dt>
              <dd>
                <Id value={lookupOf(t.tokenId)} kind="topic" label="lookup service" />
              </dd>
            </dl>
          )}
        </>
      )}
      <span className="help">Tokens in discovery that this exchange doesn&apos;t serve yet. Topic and lookup names are derived from the token id.</span>
      {error && <Notice kind="bad">Could not register: {error}</Notice>}
      {done && <Notice kind="ok">{done}</Notice>}
    </form>
  );
}

/** A fee field's text as basis points (an integer 0 to 10 000), or null. */
function bps(text: string): number | null {
  if (!/^\d+$/.test(text.trim())) return null;
  const n = Number(text.trim());
  return n <= 10_000 ? n : null;
}

/** Validator terms (0.15.0, root only): the record amm.pool.terms answers; set or clear it. */
function ValidatorTermsCard() {
  const terms = useValidatorTerms();
  const [lp, setLp] = useState("");
  const [val, setVal] = useState("");
  const [com, setCom] = useState("");
  const [error, busy, run] = useAct();
  const [done, setDone] = useState<string | null>(null);
  const t = terms.data;
  const lpN = bps(lp);
  const valN = bps(val);
  const comN = bps(com);
  const ready = lpN !== null && valN !== null && comN !== null;

  return (
    <form
      className="panel stack-sm"
      aria-labelledby="terms-h"
      onSubmit={(e) => {
        e.preventDefault();
        if (!ready) return;
        setDone(null);
        run(async () => {
          await setValidatorTerms({ lpBps: lpN, validatorBps: valN, commissionBps: comN });
          setDone("The terms are sent. They apply once this skein has taken the message.");
          terms.reload();
        });
      }}
    >
      <h2 id="terms-h" className="h2">
        Validator terms
      </h2>
      {t === null ? (
        <p className="muted">{terms.error ? `Could not read the terms: ${terms.error}` : "Reading the terms…"}</p>
      ) : hasTerms(t) ? (
        <p className="fg2">
          LP {t.fees.lpBps} · validator {t.fees.validatorBps} · commission {t.fees.commissionBps} bps. This exchange affirms pools and signs swaps at these terms.
        </p>
      ) : (
        <p className="fg2">{NO_TERMS_TEXT}</p>
      )}
      <div className="row row-wrap">
        <div className="field">
          <label htmlFor="terms-lp">LP fee (bps)</label>
          <input id="terms-lp" inputMode="numeric" value={lp} onChange={(e) => setLp(e.target.value)} aria-invalid={(lp !== "" && lpN === null) || undefined} />
        </div>
        <div className="field">
          <label htmlFor="terms-val">Validator fee (bps)</label>
          <input id="terms-val" inputMode="numeric" value={val} onChange={(e) => setVal(e.target.value)} aria-invalid={(val !== "" && valN === null) || undefined} />
        </div>
        <div className="field">
          <label htmlFor="terms-com">Commission (bps)</label>
          <input id="terms-com" inputMode="numeric" value={com} onChange={(e) => setCom(e.target.value)} aria-invalid={(com !== "" && comN === null) || undefined} />
        </div>
      </div>
      <div className="row row-wrap">
        <button type="submit" className="btn btn-primary" disabled={busy || !ready}>
          {busy ? "Sending…" : "Set terms"}
        </button>
        {hasTerms(t) && (
          <button
            type="button"
            className="btn btn-outline"
            disabled={busy}
            onClick={() => {
              setDone(null);
              run(async () => {
                await clearValidatorTerms();
                setDone("Clearing is sent. Once taken, this exchange validates nothing.");
                terms.reload();
              });
            }}
          >
            Clear terms
          </button>
        )}
      </div>
      <span className="help">Each fee is an integer from 0 to 10000 basis points. A new pool here must carry these fees exactly.</span>
      {error && <Notice kind="bad">Could not send: {error}</Notice>}
      {done && <Notice kind="ok">{done}</Notice>}
    </form>
  );
}

/** Market terms (0.16.0, root only): this market's minimum commission, as amm.swap.terms answers it; set or clear it. */
function MarketTermsCard() {
  const market = useMarketMinimum(true);
  const [min, setMin] = useState("");
  const [error, busy, run] = useAct();
  const [done, setDone] = useState<string | null>(null);
  const m = market.data;
  const minN = bps(min);

  return (
    <form
      className="panel stack-sm"
      aria-labelledby="market-h"
      onSubmit={(e) => {
        e.preventDefault();
        if (minN === null) return;
        setDone(null);
        run(async () => {
          await setMarketMinimum(minN);
          setDone("The minimum is sent. It applies once this skein has taken the message.");
          market.reload();
        });
      }}
    >
      <h2 id="market-h" className="h2">
        Market terms
      </h2>
      {m === null ? (
        <p className="muted">{market.error ? `Could not read the market terms: ${market.error}` : "Reading the market terms…"}</p>
      ) : m > 0 ? (
        <p className="fg2">Minimum commission {m} bps. The Swap page lists only pools at or above it.</p>
      ) : (
        <p className="fg2">No minimum commission: the Swap page lists every pool.</p>
      )}
      <div className="row row-wrap">
        <div className="field">
          <label htmlFor="market-min">Minimum commission (bps)</label>
          <input id="market-min" inputMode="numeric" value={min} onChange={(e) => setMin(e.target.value)} aria-invalid={(min !== "" && minN === null) || undefined} />
        </div>
      </div>
      <div className="row row-wrap">
        <button type="submit" className="btn btn-primary" disabled={busy || minN === null}>
          {busy ? "Sending…" : "Set minimum"}
        </button>
        {m !== null && m > 0 && (
          <button
            type="button"
            className="btn btn-outline"
            disabled={busy}
            onClick={() => {
              setDone(null);
              run(async () => {
                await clearMarketMinimum();
                setDone("Clearing is sent. Once taken, the Swap page lists every pool.");
                market.reload();
              });
            }}
          >
            Clear minimum
          </button>
        )}
      </div>
      <span className="help">An integer from 0 to 10000 basis points. A listing preference of this market: anyone can still swap against any pool elsewhere.</span>
      {error && <Notice kind="bad">Could not send: {error}</Notice>}
      {done && <Notice kind="ok">{done}</Notice>}
    </form>
  );
}

function RegisteredRow({ t }: { t: RegisteredToken }) {
  const [confirm, setConfirm] = useState(false);
  const [error, busy, run] = useAct();
  return (
    <li className="reg-row">
      <TokenName tokenId={t.tokenId} sym={t.sym} {...(t.icon ? { icon: t.icon } : {})} size="sm" />
      {confirm ? (
        <span className="row row-wrap end">
          <span className="small fg2">Stop serving {t.sym} here?</span>
          <button type="button" className="btn btn-primary" disabled={busy} onClick={() => run(() => deregisterToken(t.tokenId))}>
            {busy ? "Deregistering…" : "Deregister"}
          </button>
          <button type="button" className="btn btn-outline" disabled={busy} onClick={() => setConfirm(false)}>
            Cancel
          </button>
        </span>
      ) : (
        <button type="button" className="btn btn-outline" onClick={() => setConfirm(true)} aria-label={`Deregister ${t.sym}`}>
          Deregister
        </button>
      )}
      {error && <Notice kind="bad">{error}</Notice>}
    </li>
  );
}

function Requests() {
  const reqs = useListingRequests(true);
  const [error, busy, run] = useAct();
  if (reqs.data === null) return null; // loading, or the skein offers no such read
  return (
    <section className="panel stack-sm" aria-labelledby="req-h">
      <h2 id="req-h" className="h2 h2-sm">
        Requests to list
      </h2>
      {reqs.data.length === 0 ? (
        <p className="muted small">No holder has asked to list a token.</p>
      ) : (
        <>
          <p className="fg2 small">Holders who pressed “Add to this exchange”. Registering a token lists it.</p>
          <ul className="reqs">
            {reqs.data.map((r) => (
              <li key={`${r.tokenId}-${r.from}`} className="reg-row">
                <span className="req-text">
                  <TokenName tokenId={r.tokenId} sym={r.sym} size="sm" />
                  <span className="small muted">
                    from <Id value={r.from} kind="key" label="requester key" /> · {ago(r.at)} ago
                  </span>
                </span>
                <button type="button" className="btn btn-outline" disabled={busy} onClick={() => run(() => registerToken(r.tokenId))} aria-label={`Register ${r.sym}`}>
                  Register
                </button>
              </li>
            ))}
          </ul>
        </>
      )}
      {error && <Notice kind="bad">{error}</Notice>}
    </section>
  );
}

export function Settings({ session, root }: { session: Session; root: boolean }) {
  const connected = session.status === "connected";
  const reg = useRegisteredTokens(connected && root);
  return (
    <>
      <div className="stack-xs">
        <h1 className="page-title">Settings</h1>
        <p className="lead lead-tight">Root only. This exchange is a market and a validator for every token registered here.</p>
      </div>
      {!connected ? (
        <NeedWallet session={session} what="this exchange's settings" />
      ) : !root ? (
        <section className="panel">
          <p className="fg2">The connected wallet is not root on this skein. Settings are root&apos;s.</p>
        </section>
      ) : (
        <>
          <ValidatorTermsCard />
          <MarketTermsCard />
          <Register />
          <section className="panel stack-sm" aria-labelledby="regd-h">
            <h2 id="regd-h" className="h2">
              Registered
            </h2>
            {reg.data === null ? (
              <p className="muted">Loading…</p>
            ) : reg.data.length === 0 ? (
              <p className="muted">No token is registered.</p>
            ) : (
              <ul className="regs">
                {reg.data.map((t) => (
                  <RegisteredRow key={t.tokenId} t={t} />
                ))}
              </ul>
            )}
          </section>
          <Requests />
        </>
      )}
    </>
  );
}
