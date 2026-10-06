/**
 * The Validator page: the owner's view of their own skein instance as an AMM
 * validator — who it is (handle, identity, peer ID), whether a peer hears its
 * heartbeat, its policy when the owner can read it, the heartbeat start/stop
 * messages, the topics it validates (validate / stop validating, per topic) (as the owner, through the wallet-backed AuthFetch), the pools it
 * serves and the validators it sees.
 */
import { useCallback, useEffect, useState, type ReactNode } from "react";
import type { PoolState } from "@amm-poc/matching-engine";
import { useWallet } from "../wallet/AppWalletProvider";
import { useAuthFetch } from "../wallet/authFetch";
import { AMM_OVERLAY, AMM_OWNER_IDENTITY, AMM_PEER_OVERLAY, appName } from "../lib/config";
import { fetchLiveByToken, listTokenTopics, mergeLive, queryPools, type LiveAnswer } from "../lib/overlay";
import { ago } from "../lp/validators";
import { marginalPrice, shortKey, shortOutpoint } from "../market/view";
import { livenessLine, loadThisInstance, poolsServedBy, type ServedPool, type ThisInstance } from "../validator/instance";
import {
  byHandCommand,
  readAppPolicy,
  readGenesis,
  readValidated,
  sendHeartbeatControl,
  sendValidation,
  type GenesisRead,
  type HeartbeatAction,
  type HeartbeatResult,
  type ValidationAction,
} from "../validator/control";
import { LiveTable } from "./LiveTable";

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e));

function NotBuilt({ children }: { children: ReactNode }) {
  return (
    <p className="not-built">
      <strong>Not built.</strong> {children}
    </p>
  );
}

function Row({ k, children }: { k: string; children: ReactNode }) {
  return (
    <tr>
      <th>{k}</th>
      <td>{children}</td>
    </tr>
  );
}

// ---------------------------------------------------------------------------

function ThisInstanceSection({ t, error }: { t: ThisInstance | null; error: string | null }) {
  return (
    <section>
      <h2>This instance</h2>
      {error && <p className="bad" role="alert">{error}</p>}
      {!t ? (
        <p><small>Reading…</small></p>
      ) : (
        <table className="tokens kv">
          <tbody>
            <Row k="Origin"><code>{t.address.origin}</code></Row>
            <Row k="AMM routes"><code>{AMM_OVERLAY}</code></Row>
            <Row k="Handle">{t.handle ? <code>{t.handle}</code> : <span className="warn">unknown</span>}</Row>
            <Row k="Identity key">
              {t.identityKey ? (
                <>
                  <code>{t.identityKey}</code>
                  <br />
                  <small>BRC-169: {t.resolved?.resolveUrl}</small>
                </>
              ) : (
                <span className="bad">{t.resolveError ?? "unknown"}</span>
              )}
            </Row>
            <Row k="Peer ID">
              {t.peerId ? (
                <>
                  <code>{t.peerId}</code>
                  <br />
                  <small>as {t.peer?.label} reports it</small>
                </>
              ) : (
                <span className="warn">unknown until a peer sees us</span>
              )}
            </Row>
            <Row k="Liveness">
              <span className={`dot ${t.peer?.entry?.live ? "dot-live" : "dot-off"}`} /> {livenessLine(t, ago)}
              <br />
              <small>
                Read from the peer&apos;s liveness read of this instance&apos;s token topics,{" "}
                <code>{AMM_PEER_OVERLAY ? `${AMM_PEER_OVERLAY}/.live/tm_<txid>-live` : "(no peer)"}</code> (kept where the peer is a market for the token).
              </small>
            </Row>
          </tbody>
        </table>
      )}
    </section>
  );
}

// ---------------------------------------------------------------------------

function OwnerLine({ walletKey, genesis }: { walletKey: string | null; genesis: GenesisRead | null }) {
  const owner = genesis?.owner ?? (AMM_OWNER_IDENTITY || undefined);
  const source = genesis?.owner ? "read from the genesis through the explorer" : AMM_OWNER_IDENTITY ? "from VITE_AMM_OWNER_IDENTITY (the deploy's owner.identity), not from the instance" : "";
  if (!walletKey) return <p>Connect the instance owner's wallet to send these messages.</p>;
  if (!owner) {
    return (
      <p className="warn">
        The owner's key cannot be learned from any open route (the manifest and resolve give the instance's identity, not its
        owner; the explorer answers the owner only). Try it: the messagebox admits anyone's message into box amm-p2p, and
        amm-p2p ignores (errors on) one that is not from the owner.
      </p>
    );
  }
  const match = owner === walletKey.toLowerCase();
  return (
    <p className={match ? "ok" : "warn"}>
      Owner <code title={owner}>{shortKey(owner)}</code> ({source}).{" "}
      {match
        ? "The connected wallet is the owner."
        : `The connected wallet (${shortKey(walletKey)}) is not the owner: the messagebox will admit the message and amm-p2p will refuse it on its thread (you can still try).`}
    </p>
  );
}

function HeartbeatSection(props: { t: ThisInstance | null; genesis: GenesisRead | null; onSent: () => void }) {
  const { t, genesis, onSent } = props;
  const { identityKey: walletKey } = useWallet();
  const authFetch = useAuthFetch();
  const [busy, setBusy] = useState<HeartbeatAction | null>(null);
  const [result, setResult] = useState<HeartbeatResult | null>(null);
  const [error, setError] = useState<string | null>(null);

  async function send(action: HeartbeatAction) {
    if (!authFetch || !t?.identityKey) return;
    setBusy(action);
    setError(null);
    setResult(null);
    try {
      setResult(await sendHeartbeatControl(authFetch, t.address.origin, t.identityKey, action));
      onSent();
    } catch (e) {
      setError(errText(e));
    } finally {
      setBusy(null);
    }
  }

  const ready = !!authFetch && !!t?.identityKey;
  return (
    <section>
      <h2>Register / heartbeat</h2>
      <p>
        <small>
          A validator heartbeats on <code>tm_&lt;txid&gt;-live</code> for each topic it validates (below); markets keep it
          in their liveness read, <code>/amm/.live/tm_&lt;txid&gt;-live</code>. The heartbeat starts when amm-p2p receives{" "}
          <code>{"{kind: \"amm-p2p-start\"}"}</code> in box <code>amm/amm-p2p</code> from the owner (or the cron provider),
          after every boot; stop ends it and keeps the validated topics. These buttons send that
          message as the connected wallet: <code>POST {t?.address.origin ?? "<instance>"}/sendMessage</code> (BRC-33),
          BRC-104-signed by the wallet.
        </small>
      </p>
      <OwnerLine walletKey={walletKey} genesis={genesis} />
      <div>
        <button type="button" disabled={!ready || busy !== null} onClick={() => void send("start")}>
          {busy === "start" ? "Sending…" : "Start heartbeat"}
        </button>
        <button type="button" disabled={!ready || busy !== null} onClick={() => void send("stop")}>
          {busy === "stop" ? "Sending…" : "Stop heartbeat"}
        </button>
        {!t?.identityKey && <small className="bad"> the instance's identity key is unknown (the message's recipient)</small>}
      </div>
      {error && <p className="bad" role="alert">{error}</p>}
      {result && (
        <div className="leg">
          <p>
            <strong>{result.action === "start" ? "Start" : "Stop"}:</strong> HTTP {result.status}{" "}
            {result.admitted ? (
              <span className="ok">admitted{result.id && <> as <code>{shortKey(result.id)}</code></>}</span>
            ) : (
              <span className="bad">refused: {result.refusal}</span>
            )}
          </p>
          {result.admitted && (
            <p>
              <small>
                Admitted means the messagebox took the message (box <code>amm/amm-p2p</code> takes any sender).
                amm-p2p then acts only if the sender is the owner; a refusal there is on the instance's thread and not in
                this answer. Whether the heartbeat runs shows in the peer's <code>/amm/.live/tm_&lt;txid&gt;-live</code> within one interval
                (Refresh).
              </small>
            </p>
          )}
          <details>
            <summary>Request and answer</summary>
            <pre>{`POST ${result.url}\n${JSON.stringify(result.request, null, 2)}\n\n${typeof result.answer === "string" ? result.answer : JSON.stringify(result.answer, null, 2)}`}</pre>
          </details>
        </div>
      )}
      <p>
        <small>By hand on the host (sent as the cron provider, which amm-p2p also accepts):</small>
      </p>
      <pre>{byHandCommand(t?.address.name)}</pre>
    </section>
  );
}

// ---------------------------------------------------------------------------

function ValidationSection(props: { t: ThisInstance | null; refreshKey: number }) {
  const { t, refreshKey } = props;
  const authFetch = useAuthFetch();
  const origin = t?.address.origin;
  const [topics, setTopics] = useState<string[] | null>(null);
  const [validated, setValidated] = useState<string[] | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [result, setResult] = useState<HeartbeatResult | null>(null);
  const [error, setError] = useState<string | null>(null);

  const read = useCallback(async () => {
    setError(null);
    try {
      setTopics((await listTokenTopics(AMM_OVERLAY)).filter((x) => x.kind === "native").map((x) => x.topic));
      const app = appName(AMM_OVERLAY);
      if (authFetch && origin && app) {
        const v = await readValidated(authFetch, origin, app);
        setValidated(v ?? null);
        if (!v) setError("the validated set is not readable: the explorer answers the instance's owner only");
      }
    } catch (e) {
      setError(errText(e));
    }
  }, [authFetch, origin]);

  useEffect(() => {
    void read();
  }, [read, refreshKey]);

  async function send(action: ValidationAction, topic: string) {
    if (!authFetch || !origin || !t?.identityKey) return;
    setBusy(topic);
    setError(null);
    setResult(null);
    try {
      setResult(await sendValidation(authFetch, origin, t.identityKey, action, topic));
      await read();
    } catch (e) {
      setError(errText(e));
    } finally {
      setBusy(null);
    }
  }

  const ready = !!authFetch && !!t?.identityKey;
  const all = [...new Set([...(topics ?? []), ...(validated ?? [])])];
  return (
    <section>
      <h2>Validation</h2>
      <p>
        <small>
          Per topic, the owner sets up validation: <code>{"{fn: \"validate\" | \"unvalidate\", args: {topic}}"}</code>{" "}
          in box <code>amm/validate</code> (the owner's row: nobody else's message is admitted there). One setting: a
          validated topic is beaconed on <code>tm_&lt;txid&gt;-live</code> while the heartbeat runs, and this instance
          signs its swaps and takes on its new liquidity (addLiquidity, pool deploys); a topic not validated is refused
          (<code>not_validating</code>). The set is read through the explorer (owner only).
        </small>
      </p>
      {topics === null ? (
        <p>Reading…</p>
      ) : all.length === 0 ? (
        <p><small>The instance serves no token topics.</small></p>
      ) : (
        <table className="tokens">
          <thead>
            <tr>
              <th>Topic</th>
              <th>Validating</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {all.map((topic) => {
              const on = validated?.includes(topic);
              return (
                <tr key={topic}>
                  <td><code title={topic}>{shortKey(topic)}</code></td>
                  <td>{validated === null ? "?" : on ? <span className="ok">yes</span> : "no"}</td>
                  <td>
                    <button type="button" disabled={!ready || busy !== null || on === true} onClick={() => void send("validate", topic)}>
                      {busy === topic && !on ? "Sending…" : "Validate"}
                    </button>
                    <button type="button" disabled={!ready || busy !== null || on === false} onClick={() => void send("unvalidate", topic)}>
                      {busy === topic && on ? "Sending…" : "Stop validating"}
                    </button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
      {!authFetch && <small>connect the owner's wallet to read and change the set</small>}
      {error && <p className="bad" role="alert">{error}</p>}
      {result && (
        <p>
          <strong>{result.action === "validate" ? "Validate" : "Stop validating"}:</strong> HTTP {result.status}{" "}
          {result.admitted ? <span className="ok">admitted</span> : <span className="bad">refused: {result.refusal}</span>}
        </p>
      )}
    </section>
  );
}

// ---------------------------------------------------------------------------

function PolicySection(props: { genesis: GenesisRead | null; onRead: (g: GenesisRead) => void; origin: string | undefined }) {
  const { genesis, onRead, origin } = props;
  const authFetch = useAuthFetch();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function read() {
    if (!authFetch || !origin) return;
    setBusy(true);
    setError(null);
    try {
      const g = await readGenesis(authFetch, origin);
      // The installed app's config.amm, which the programs read before the genesis defaults.
      const app = appName(AMM_OVERLAY);
      const fromApp = app ? await readAppPolicy(authFetch, origin, app).catch(() => undefined) : undefined;
      onRead(fromApp && g.ok ? { ...g, policy: { ...g.policy, ...fromApp } } : g);
    } catch (e) {
      setError(errText(e));
    } finally {
      setBusy(false);
    }
  }

  const p = genesis?.ok ? genesis.policy : undefined;
  const show = (v: number | undefined, unit: string) => (v === undefined ? <span className="warn">not configured</span> : `${v} ${unit}`);
  return (
    <section>
      <h2>Policy</h2>
      {p ? (
        <table className="tokens kv">
          <tbody>
            <Row k="Min validator fee">{show(p.minValidatorFeeBps, "bps")}</Row>
            <Row k="Max LP fee">{show(p.maxLpFeeBps, "bps")}</Row>
            <Row k="Heartbeat interval">{show(p.heartbeatSeconds, "s")}</Row>
            <Row k="Offline after">{show(p.offlineSeconds, "s")}</Row>
            {p.peerId && <Row k="Peer ID (config)"><code>{p.peerId}</code></Row>}
          </tbody>
        </table>
      ) : (
        <p className="warn">
          Not readable from the instance: min validator fee, max LP fee (the app record's <code>config.amm.ammValidator</code>) and
          the heartbeat interval (<code>config.amm.ammP2p.heartbeatSeconds</code>) are not exposed by any open route.
        </p>
      )}
      <p>
        <small>
          The owner can read them through the explorer (<code>/explore</code>, BRC-104, owner only), signed by the wallet.
        </small>
      </p>
      <button type="button" disabled={!authFetch || !origin || busy} onClick={() => void read()}>
        {busy ? "Reading…" : "Read through the explorer (owner only)"}
      </button>
      {!authFetch && <small> connect a wallet first</small>}
      {genesis && !genesis.ok && <p className="bad">{genesis.error}</p>}
      {error && <p className="bad" role="alert">{error}</p>}
      <NotBuilt>
        Editing the policy: it is the manifest's <code>config.amm</code>, changed by installing the app again with a new
        manifest; the app offers no <code>writes: true</code> function for it.
      </NotBuilt>
    </section>
  );
}

// ---------------------------------------------------------------------------

function PoolsServedSection(props: { identityKey: string | undefined; peerLive: boolean | undefined; refreshKey: number }) {
  const { identityKey, peerLive, refreshKey } = props;
  const authFetch = useAuthFetch();
  const [served, setServed] = useState<ServedPool[] | null>(null);
  const [errors, setErrors] = useState<string[]>([]);
  const [count, setCount] = useState(0);

  useEffect(() => {
    if (!identityKey) return;
    let cancelled = false;
    void (async () => {
      const errs: string[] = [];
      try {
        const topics = await listTokenTopics(AMM_OVERLAY);
        const pools = new Map<string, PoolState[] | Error>();
        await Promise.all(
          topics.map(async (t) => {
            try {
              pools.set(t.tokenId, await queryPools(authFetch, AMM_OVERLAY, t.tokenId));
            } catch (e) {
              pools.set(t.tokenId, e instanceof Error ? e : new Error(String(e)));
              errs.push(`${t.tokenId}: ${errText(e)}`);
            }
          }),
        );
        let total = 0;
        for (const a of pools.values()) if (Array.isArray(a)) total += a.length;
        if (!cancelled) {
          setServed(poolsServedBy(identityKey, topics, pools));
          setCount(total);
        }
      } catch (e) {
        errs.push(errText(e));
      }
      if (!cancelled) setErrors(errs);
    })();
    return () => {
      cancelled = true;
    };
  }, [identityKey, refreshKey, authFetch]);

  return (
    <section>
      <h2>Pools served</h2>
      <p><small>The pools in this instance's lookup whose validator identity is this instance's.</small></p>
      {!identityKey && <p className="warn">Unknown: this instance's identity key is unknown.</p>}
      {errors.map((e) => (
        <p key={e} className="bad">{e}</p>
      ))}
      {served && served.length === 0 && (
        <p>
          None. The lookup holds {count} pool{count === 1 ? "" : "s"}, none naming this instance as validator (on v2 the
          seeded pool names the fixture validator, not amm2).
        </p>
      )}
      {served && served.length > 0 && (
        <div className="scroll">
          <table className="tokens">
            <thead>
              <tr>
                <th>Pool</th>
                <th>Token</th>
                <th className="num">BSV reserve</th>
                <th className="num">Token reserve</th>
                <th className="num">Price</th>
                <th className="num">LP / val. / comm. (bps)</th>
                <th>Validator</th>
              </tr>
            </thead>
            <tbody>
              {served.map(({ topic, pool }) => (
                <tr key={pool.outpoint}>
                  <td><code title={pool.outpoint}>{shortOutpoint(pool.outpoint)}</code></td>
                  <td><code title={topic.tokenId}>{shortOutpoint(topic.tokenId)}</code></td>
                  <td className="num">{pool.bsvReserve.toString()}</td>
                  <td className="num">{pool.tokenReserve.toString()}</td>
                  <td className="num">{marginalPrice(pool)}</td>
                  <td className="num">{`${pool.liquidityFeeBps} / ${pool.validationFeeBps} / ${pool.commissionBps}`}</td>
                  <td>
                    <span className={`dot ${peerLive ? "dot-live" : "dot-off"}`} /> {peerLive ? "live" : "not seen live"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          <p><small>Price: sats per token base unit. Liveness: this instance's heartbeat as the peer sees it.</small></p>
        </div>
      )}
    </section>
  );
}

function PeersSection({ live, error }: { live: LiveAnswer | null; error: string | null }) {
  return (
    <section>
      <h2>Peers</h2>
      <p>
        <small>
          The validators live in this node&apos;s liveness read (<code>{AMM_OVERLAY}/.live/tm_&lt;txid&gt;-live</code>, each token it
          serves, merged; kept when this node is a market, <code>config.amm.ammP2p.market</code>).
        </small>
      </p>
      {error && <p className="bad">{error}</p>}
      {live && live.validators.length > 0 ? (
        <LiveTable live={live} />
      ) : (
        live && <p>None{live.kept ? ` within ${Math.round(live.windowMs / 1000)} s` : ": this node keeps no liveness for its tokens"}.</p>
      )}
    </section>
  );
}

// ---------------------------------------------------------------------------

export function ValidatorPage() {
  const [t, setT] = useState<ThisInstance | null>(null);
  const [tError, setTError] = useState<string | null>(null);
  const [live, setLive] = useState<LiveAnswer | null>(null);
  const [liveError, setLiveError] = useState<string | null>(null);
  const [genesis, setGenesis] = useState<GenesisRead | null>(null);
  const [refreshKey, setRefreshKey] = useState(0);

  const refresh = useCallback(async () => {
    setTError(null);
    setLiveError(null);
    let topics: Awaited<ReturnType<typeof listTokenTopics>> = [];
    try {
      topics = await listTokenTopics(AMM_OVERLAY);
    } catch (e) {
      setLiveError(`${AMM_OVERLAY}/listTopicManagers: ${errText(e)}`);
    }
    try {
      setT(await loadThisInstance(AMM_OVERLAY, AMM_PEER_OVERLAY, topics.map((x) => x.topic)));
    } catch (e) {
      setTError(errText(e));
    }
    try {
      const reads = await fetchLiveByToken(AMM_OVERLAY, topics);
      setLive(mergeLive([...reads.values()].filter((r): r is LiveAnswer => !(r instanceof Error))));
      const failed = [...reads.values()].filter((r): r is Error => r instanceof Error);
      if (failed.length) setLiveError(`${AMM_OVERLAY}/.live: ${failed.map((e) => e.message).join("; ")}`);
    } catch (e) {
      setLiveError(`${AMM_OVERLAY}/.live: ${errText(e)}`);
    }
    setRefreshKey((k) => k + 1);
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  return (
    <>
      <section>
        <div className="section-head">
          <h2>Validator</h2>
          <button type="button" onClick={() => void refresh()}>Refresh</button>
        </div>
        <p>
          <small>
            Your own skein instance as an AMM validator. Instance: <code>{AMM_OVERLAY}</code>; peer:{" "}
            <code>{AMM_PEER_OVERLAY || "none"}</code>.
          </small>
        </p>
      </section>
      <ThisInstanceSection t={t} error={tError} />
      <HeartbeatSection t={t} genesis={genesis} onSent={() => void refresh()} />
      <ValidationSection t={t} refreshKey={refreshKey} />
      <PolicySection genesis={genesis} onRead={setGenesis} origin={t?.address.origin} />
      <PoolsServedSection identityKey={t?.identityKey} peerLive={t?.peer?.entry?.live} refreshKey={refreshKey} />
      <PeersSection live={live} error={liveError} />
    </>
  );
}
