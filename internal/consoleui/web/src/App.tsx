import { useCallback, useEffect, useState } from "react";
import { api, type ProjectSummary } from "./api.ts";
import { Detail } from "./components/Detail.tsx";

export function App() {
  const [projects, setProjects] = useState<ProjectSummary[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      const list = await api.projects();
      setProjects(list);
      setErr(null);
      setSelected((cur) => cur ?? (list.length > 0 ? list[0].name : null));
    } catch (e) {
      setErr(String(e instanceof Error ? e.message : e));
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="brand">
          w17ctl<span className="brand-sub">local</span>
        </div>
        {err && <div className="banner error">{err}</div>}
        {projects.length === 0 && !err && (
          <div className="muted pad">
            No registered projects. Run <code>w17ctl project import</code> in a project.
          </div>
        )}
        <nav className="project-list">
          {projects.map((p) => (
            <button
              key={p.name}
              className={`project-item ${p.name === selected ? "active" : ""}`}
              onClick={() => setSelected(p.name)}
            >
              <span className="project-name">{p.name}</span>
              <span className="project-meta">
                {p.activePreset ? `▶ ${p.activePreset}` : "full stack"} · {p.portCount} ports
              </span>
            </button>
          ))}
        </nav>
        <button className="refresh" onClick={() => void refresh()}>
          ↻ refresh
        </button>
      </aside>
      <main className="content">
        {selected ? (
          <Detail name={selected} onChanged={() => void refresh()} />
        ) : (
          <div className="muted pad">Select a project.</div>
        )}
      </main>
    </div>
  );
}
