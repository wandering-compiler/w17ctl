import { useCallback, useEffect, useState } from "react";
import { api, type ActionResult, type ProjectDetail } from "../api.ts";
import { Containers } from "./Containers.tsx";
import { Presets } from "./Presets.tsx";
import { Config } from "./Config.tsx";
import { Fixtures } from "./Fixtures.tsx";

export function Detail({ name, onChanged }: { name: string; onChanged: () => void }) {
  const [detail, setDetail] = useState<ProjectDetail | null>(null);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<ActionResult | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      setDetail(await api.detail(name));
      setErr(null);
    } catch (e) {
      setErr(String(e instanceof Error ? e.message : e));
    }
  }, [name]);

  useEffect(() => {
    setResult(null);
    void load();
  }, [load]);

  const runAction = async (fn: () => Promise<ActionResult>) => {
    setBusy(true);
    setResult(null);
    try {
      setResult(await fn());
      await load();
      onChanged();
    } catch (e) {
      setResult({ ok: false, output: String(e instanceof Error ? e.message : e) });
    } finally {
      setBusy(false);
    }
  };

  if (err) return <div className="banner error pad">{err}</div>;
  if (!detail) return <div className="muted pad">Loading…</div>;

  return (
    <div className="detail">
      <header className="detail-head">
        <div>
          <h1>{detail.name}</h1>
          <div className="muted path">{detail.path}</div>
        </div>
        <div className="actions">
          <span className="active-badge">{detail.activePreset ? `▶ ${detail.activePreset}` : "full stack"}</span>
          <button
            className="btn primary"
            disabled={busy}
            title="docker compose up -d (via w17ctl stack up — applies the active preset + managed ports)"
            onClick={() => void runAction(() => api.up(name, {}))}
          >
            ▲ up
          </button>
          <button
            className="btn"
            disabled={busy}
            title="docker compose down (keeps volumes)"
            onClick={() => void runAction(() => api.down(name, true))}
          >
            ■ down
          </button>
          <button
            className="btn danger"
            disabled={busy}
            title="docker compose down -v (drops volumes — all local DB data)"
            onClick={() => {
              if (confirm(`Tear down ${name} AND drop volumes (all local DB data)?`)) {
                void runAction(() => api.down(name, false));
              }
            }}
          >
            ✕ down -v
          </button>
        </div>
      </header>

      {detail.links.length > 0 && (
        <div className="quick-links">
          {detail.links.map((l) =>
            l.web ? (
              <a key={l.url} className={`qlink ${l.kind}`} href={l.url} target="_blank" rel="noreferrer">
                {l.label} <span className="qlink-arrow">↗</span>
              </a>
            ) : (
              <span key={l.url} className="qlink infra" title="not a web surface">
                {l.label} <span className="muted small">{l.url.replace("http://localhost:", ":")}</span>
              </span>
            ),
          )}
        </div>
      )}

      {busy && <div className="banner">working… (docker compose can take a while on first pull)</div>}
      {result && (
        <div className={`banner ${result.ok ? "ok" : "error"}`}>
          {result.ok ? "✓ done" : "✗ failed"}
          {result.preset ? ` · preset ${result.preset}` : ""}
          {result.output && <pre className="output">{result.output}</pre>}
        </div>
      )}

      <Containers name={name} ports={detail.ports} />
      <Presets
        detail={detail}
        reload={async () => {
          await load();
          onChanged();
        }}
      />
      <Fixtures name={name} />
      <Config name={name} detail={detail} />
    </div>
  );
}
