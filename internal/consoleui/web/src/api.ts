// Typed client for the w17ctl local-UI JSON API. Mirrors the Go wire
// structs in domains/console/ui.

export interface ProjectSummary {
  name: string;
  path: string;
  activePreset: string;
  portCount: number;
  presetCount: number;
}

export interface PortInfo {
  envVar: string;
  hostPort: number;
  container?: number;
  service?: string;
}

export interface PresetInfo {
  name: string;
  active: boolean;
  services: string[] | null;
  env: Record<string, string> | null;
}

export interface LockSummary {
  project: string;
  connections: string[];
}

export interface ServiceLink {
  label: string;
  url: string;
  kind: string;
  service: string;
  web: boolean;
}

export interface ProjectDetail {
  name: string;
  path: string;
  activePreset: string;
  ports: PortInfo[];
  presets: PresetInfo[];
  services: string[] | null;
  lock?: LockSummary;
  links: ServiceLink[];
}

export interface ConfigPreview {
  lockYaml: string;
  mapIndex: string;
  agents: string;
}

export interface FixtureRow {
  pk: unknown;
  fields: Record<string, unknown>;
}

export interface FixtureModel {
  model: string;
  rows: FixtureRow[];
}

export interface FixtureFile {
  connection: string;
  file: string;
  models: FixtureModel[];
}

export interface Container {
  id: string;
  name: string;
  service: string;
  image: string;
  state: string;
  status: string;
  ports: string;
  cpuPerc?: string;
  memUsage?: string;
  memPerc?: string;
}

export interface ActionResult {
  ok: boolean;
  output: string;
  preset?: string;
}

export interface LiveFrame {
  project: string;
  containers: Container[] | null;
  statsReady: boolean;
  error?: string;
}

async function req<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  const data = text ? JSON.parse(text) : undefined;
  if (!res.ok) {
    const msg = data && typeof data === "object" && "error" in data ? (data as { error: string }).error : res.statusText;
    throw new Error(msg);
  }
  return data as T;
}

export const api = {
  projects: () => req<ProjectSummary[]>("GET", "/api/projects"),
  detail: (name: string) => req<ProjectDetail>("GET", `/api/projects/${encodeURIComponent(name)}`),
  containers: (name: string) => req<Container[]>("GET", `/api/projects/${encodeURIComponent(name)}/containers`),
  config: (name: string) => req<ConfigPreview>("GET", `/api/projects/${encodeURIComponent(name)}/config`),
  fixtures: (name: string) => req<FixtureFile[]>("GET", `/api/projects/${encodeURIComponent(name)}/fixtures`),

  up: (name: string, opts: { preset?: string; services?: string[] }) =>
    req<ActionResult>("POST", `/api/projects/${encodeURIComponent(name)}/up`, opts),
  down: (name: string, keepVolumes: boolean) =>
    req<ActionResult>("POST", `/api/projects/${encodeURIComponent(name)}/down`, { keepVolumes }),

  savePreset: (name: string, preset: { name: string; services: string[]; env: Record<string, string> }) =>
    req<PresetInfo[]>("POST", `/api/projects/${encodeURIComponent(name)}/presets`, preset),
  deletePreset: (name: string, preset: string) =>
    req<PresetInfo[]>("DELETE", `/api/projects/${encodeURIComponent(name)}/presets/${encodeURIComponent(preset)}`),
  activatePreset: (name: string, preset: string, clear: boolean) =>
    req<PresetInfo[]>("POST", `/api/projects/${encodeURIComponent(name)}/presets/${encodeURIComponent(preset)}/activate`, { clear }),
  setPresetEnv: (name: string, preset: string, env: Record<string, string>) =>
    req<PresetInfo[]>("PUT", `/api/projects/${encodeURIComponent(name)}/presets/${encodeURIComponent(preset)}/env`, { env }),
};

// liveContainers opens the /ws stream for a project, invoking onFrame on
// each push. Returns a close function.
export function liveContainers(project: string, onFrame: (f: LiveFrame) => void): () => void {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const ws = new WebSocket(`${proto}//${location.host}/ws?project=${encodeURIComponent(project)}`);
  ws.onmessage = (ev) => {
    try {
      onFrame(JSON.parse(ev.data) as LiveFrame);
    } catch {
      /* ignore malformed frame */
    }
  };
  return () => ws.close();
}
