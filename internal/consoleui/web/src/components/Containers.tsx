import { useEffect, useState } from "react";
import { liveContainers, type Container, type PortInfo } from "../api.ts";

// Containers shows a live-updating table of the project's containers
// (state + CPU/mem), fed by the /ws push stream. The first frame carries
// the container list only (fast); CPU/mem arrive on the next frame, so
// those cells show a loading shimmer until `statsReady` rather than a
// bare dash. Until the very first frame lands the card shows a loading
// state, not "no containers" — so an empty table never flashes.
export function Containers({ name, ports }: { name: string; ports: PortInfo[] }) {
  const [containers, setContainers] = useState<Container[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [statsReady, setStatsReady] = useState(false);
  const [streamErr, setStreamErr] = useState<string | null>(null);

  useEffect(() => {
    setContainers([]);
    setLoaded(false);
    setStatsReady(false);
    setStreamErr(null);
    const close = liveContainers(name, (f) => {
      setLoaded(true);
      setStatsReady(f.statsReady);
      setStreamErr(f.error ?? null);
      setContainers(f.containers ?? []);
    });
    return close;
  }, [name]);

  const portsForService = (svc: string) => ports.filter((p) => p.service === svc);

  return (
    <section className="card">
      <h2>
        Containers
        {loaded && !statsReady && containers.length > 0 && <span className="loading-tag">loading stats…</span>}
      </h2>
      {streamErr && <div className="banner warn">{streamErr}</div>}
      {!loaded && !streamErr && (
        <div className="loading-row">
          <span className="spinner" /> Reading containers…
        </div>
      )}
      {loaded && containers.length === 0 && !streamErr && <div className="muted">No containers running.</div>}
      {containers.length > 0 && (
        <table className="grid">
          <thead>
            <tr>
              <th>Service</th>
              <th>State</th>
              <th>CPU</th>
              <th>Mem</th>
              <th>Ports</th>
            </tr>
          </thead>
          <tbody>
            {containers.map((c) => (
              <tr key={c.id || c.name}>
                <td>
                  <span className="svc">{c.service || c.name}</span>
                  <span className="muted small">{c.status}</span>
                </td>
                <td>
                  <span className={`pill ${c.state === "running" ? "up" : "down"}`}>{c.state}</span>
                </td>
                <td className="num">
                  <Metric value={c.cpuPerc} pending={!statsReady && c.state === "running"} />
                </td>
                <td className="num">
                  <Metric value={c.memUsage} pending={!statsReady && c.state === "running"} />
                </td>
                <td>
                  {portsForService(c.service).map((p) => (
                    <a
                      key={p.envVar}
                      className="port-link"
                      href={`http://localhost:${p.hostPort}`}
                      target="_blank"
                      rel="noreferrer"
                      title={p.envVar}
                    >
                      {p.hostPort}
                      {p.container ? `→${p.container}` : ""}
                    </a>
                  ))}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

// Metric shows a live value, a shimmer placeholder while stats are still
// loading for a running container, or a dash when there is genuinely no
// value (a stopped container).
function Metric({ value, pending }: { value?: string; pending: boolean }) {
  if (value) return <>{value}</>;
  if (pending) return <span className="shimmer" />;
  return <>—</>;
}
