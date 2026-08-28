import { useEffect, useState } from "react";
import { api, type FixtureFile, type FixtureRow } from "../api.ts";

// Fixtures renders a project's seed data in a human-readable form: one
// table per model, grouped by connection. The on-disk shape is Django-
// style dumpdata (model / pk / fields) — here it becomes a flat grid a
// human can actually read.
export function Fixtures({ name }: { name: string }) {
  const [files, setFiles] = useState<FixtureFile[] | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    setFiles(null);
    setErr(null);
    api
      .fixtures(name)
      .then(setFiles)
      .catch((e) => setErr(String(e instanceof Error ? e.message : e)));
  }, [name]);

  if (err) return null; // fixtures are optional — stay quiet on error
  if (!files || files.length === 0) return null;

  return (
    <section className="card">
      <h2>Fixtures</h2>
      <p className="muted small">Seed data that loads into the dev database — the default values shipped with the project.</p>
      {files.map((f) => (
        <div key={f.file} className="fixture-file">
          <div className="muted small">
            {f.connection} · <code>{f.file}</code>
          </div>
          {f.models.map((m) => (
            <FixtureTable key={m.model} model={m.model} rows={m.rows} />
          ))}
        </div>
      ))}
    </section>
  );
}

function FixtureTable({ model, rows }: { model: string; rows: FixtureRow[] }) {
  // Column set = union of all field keys across the rows, file order.
  const cols: string[] = [];
  for (const r of rows) {
    for (const k of Object.keys(r.fields)) {
      if (!cols.includes(k)) cols.push(k);
    }
  }
  return (
    <div className="fixture-model">
      <h3>
        {model} <span className="muted small">{rows.length} row(s)</span>
      </h3>
      <table className="grid">
        <thead>
          <tr>
            <th>pk</th>
            {cols.map((c) => (
              <th key={c}>{c}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr key={i}>
              <td className="num muted">{fmt(r.pk)}</td>
              {cols.map((c) => (
                <td key={c}>{fmt(r.fields[c])}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// fmt renders a fixture value compactly: scalars as-is, arrays/objects as
// short JSON.
function fmt(v: unknown): string {
  if (v === null || v === undefined) return "—";
  if (Array.isArray(v)) return v.length === 1 ? String(v[0]) : JSON.stringify(v);
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}
