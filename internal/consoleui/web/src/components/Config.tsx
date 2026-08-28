import { useEffect, useState } from "react";
import { api, type ConfigPreview, type ProjectDetail } from "../api.ts";

// Config is the read-side digest: assigned host ports (clickable), the
// lock summary, discovered services, plus collapsible raw previews of the
// signed lock, the AI project-map overview, and AGENTS.md.
export function Config({ name, detail }: { name: string; detail: ProjectDetail }) {
  const [preview, setPreview] = useState<ConfigPreview | null>(null);

  useEffect(() => {
    setPreview(null);
    api.config(name).then(setPreview).catch(() => setPreview(null));
  }, [name]);

  return (
    <section className="card">
      <h2>Ports &amp; config</h2>

      <h3>Assigned host ports</h3>
      <table className="grid">
        <thead>
          <tr>
            <th>Host</th>
            <th>Container</th>
            <th>Service</th>
            <th>Env var</th>
          </tr>
        </thead>
        <tbody>
          {detail.ports.map((p) => (
            <tr key={p.envVar}>
              <td>
                <a className="port-link" href={`http://localhost:${p.hostPort}`} target="_blank" rel="noreferrer">
                  {p.hostPort}
                </a>
              </td>
              <td className="num muted">{p.container || "—"}</td>
              <td>{p.service || "—"}</td>
              <td className="muted small">{p.envVar}</td>
            </tr>
          ))}
        </tbody>
      </table>

      {detail.lock && (
        <>
          <h3>Lock</h3>
          <div className="kv">
            <span className="muted">project</span>
            <span>{detail.lock.project}</span>
            <span className="muted">connections</span>
            <span>{detail.lock.connections.length > 0 ? detail.lock.connections.join(", ") : "—"}</span>
          </div>
        </>
      )}

      <h3>Services</h3>
      <div className="chips">
        {(detail.services ?? []).map((s) => (
          <span key={s} className="chip">
            {s}
          </span>
        ))}
      </div>

      {preview && preview.mapIndex && <Collapsible title="Project map (overview)" body={preview.mapIndex} />}
      {preview && preview.lockYaml && <Collapsible title="w17/lock.yaml" body={preview.lockYaml} />}
      {preview && preview.agents && <Collapsible title="AGENTS.md" body={preview.agents} />}
    </section>
  );
}

function Collapsible({ title, body }: { title: string; body: string }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="collapsible">
      <button className="collapsible-head" onClick={() => setOpen((o) => !o)}>
        {open ? "▾" : "▸"} {title}
      </button>
      {open && <pre className="preview">{body}</pre>}
    </div>
  );
}
