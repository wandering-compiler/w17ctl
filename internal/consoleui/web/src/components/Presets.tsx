import { useState } from "react";
import { api, type ProjectDetail } from "../api.ts";

// Presets is the click-to-run profile panel: activate a preset (which
// `up` then applies), edit which services it starts + its extra env, and
// create / delete presets. This is the "run just the admin on my weak
// laptop, with the heavy env switched off" surface.
export function Presets({ detail, reload }: { detail: ProjectDetail; reload: () => Promise<void> }) {
  const [newName, setNewName] = useState("");
  const [busy, setBusy] = useState(false);
  const services = detail.services ?? [];

  const guard = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    try {
      await fn();
      await reload();
    } catch (e) {
      alert(String(e instanceof Error ? e.message : e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="card">
      <h2>Presets</h2>
      <p className="muted small">
        A preset selects which services <code>up</code> starts and layers extra env on top of the managed ports.
      </p>

      <div className="preset-row">
        <button
          className={`pill-btn ${detail.activePreset === "" ? "active" : ""}`}
          disabled={busy}
          onClick={() =>
            void guard(() =>
              detail.presets.find((p) => p.active)
                ? api.activatePreset(detail.name, detail.presets.find((p) => p.active)!.name, true)
                : Promise.resolve(),
            )
          }
        >
          full stack
        </button>
        {detail.presets.map((p) => (
          <button
            key={p.name}
            className={`pill-btn ${p.active ? "active" : ""}`}
            disabled={busy}
            onClick={() => void guard(() => api.activatePreset(detail.name, p.name, false))}
          >
            {p.active ? "▶ " : ""}
            {p.name}
          </button>
        ))}
      </div>

      {detail.presets.map((p) => (
        <PresetEditor
          key={p.name}
          presetName={p.name}
          services={services}
          selected={p.services ?? []}
          env={p.env ?? {}}
          busy={busy}
          onSave={(svc, env) => void guard(() => api.savePreset(detail.name, { name: p.name, services: svc, env }))}
          onDelete={() => void guard(() => api.deletePreset(detail.name, p.name))}
        />
      ))}

      <div className="new-preset">
        <input
          placeholder="new preset name"
          value={newName}
          onChange={(e) => setNewName(e.target.value)}
        />
        <button
          className="btn"
          disabled={busy || newName.trim() === ""}
          onClick={() =>
            void guard(async () => {
              await api.savePreset(detail.name, { name: newName.trim(), services: [], env: {} });
              setNewName("");
            })
          }
        >
          + add
        </button>
      </div>
    </section>
  );
}

function PresetEditor({
  presetName,
  services,
  selected,
  env,
  busy,
  onSave,
  onDelete,
}: {
  presetName: string;
  services: string[];
  selected: string[];
  env: Record<string, string>;
  busy: boolean;
  onSave: (services: string[], env: Record<string, string>) => void;
  onDelete: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [sel, setSel] = useState<string[]>(selected);
  const [envText, setEnvText] = useState(
    Object.entries(env)
      .map(([k, v]) => `${k}=${v}`)
      .join("\n"),
  );

  const toggle = (svc: string) =>
    setSel((cur) => (cur.includes(svc) ? cur.filter((s) => s !== svc) : [...cur, svc]));

  const parseEnv = (): Record<string, string> => {
    const out: Record<string, string> = {};
    for (const line of envText.split("\n")) {
      const t = line.trim();
      if (!t) continue;
      const i = t.indexOf("=");
      if (i > 0) out[t.slice(0, i)] = t.slice(i + 1);
    }
    return out;
  };

  return (
    <div className="preset-editor">
      <button className="preset-editor-head" onClick={() => setOpen((o) => !o)}>
        {open ? "▾" : "▸"} {presetName}
        <span className="muted small">
          {sel.length === 0 ? "all services" : `${sel.length} service(s)`} · {Object.keys(env).length} env
        </span>
      </button>
      {open && (
        <div className="preset-editor-body">
          <div className="svc-grid">
            {services.length === 0 && <span className="muted small">No services discovered (run codegen + up first).</span>}
            {services.map((svc) => (
              <label key={svc} className="svc-check">
                <input type="checkbox" checked={sel.includes(svc)} onChange={() => toggle(svc)} />
                {svc}
              </label>
            ))}
          </div>
          <label className="env-label">
            extra env (KEY=VALUE per line)
            <textarea
              value={envText}
              onChange={(e) => setEnvText(e.target.value)}
              rows={4}
              spellCheck={false}
              placeholder="W17_OTEL_ENDPOINT="
            />
          </label>
          <div className="preset-editor-actions">
            <button className="btn primary" disabled={busy} onClick={() => onSave(sel, parseEnv())}>
              save
            </button>
            <button className="btn danger" disabled={busy} onClick={onDelete}>
              delete
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
