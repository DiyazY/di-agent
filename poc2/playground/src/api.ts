export type SystemName = "genset" | "propulsion" | "battery" | "auxload";

export interface PlaygroundConfig {
  gensetIds: string[];
  propulsionIds: string[];
  batteryIds: string[];
  auxloadIds: string[];
}

export interface SystemStatus {
  target_load_ratio: number;
  current_load_ratio: number;
  allocated_power_kw?: number;
  soc?: number;
  [key: string]: unknown;
}

export interface SwitchboardStatus {
  switchboard_id: string;
  available_supply_kw: number;
  total_demand_kw: number;
  gensets: Record<string, { power_kw: number; stale: boolean }>;
  batteries: Record<string, { power_kw: number; stale: boolean }>;
  consumers: Record<
    string,
    { requested_power_kw: number; priority: number; allocated_power_kw: number; stale: boolean }
  >;
}

async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { "Content-Type": "application/json", ...init?.headers },
  });
  if (!res.ok) {
    const detail = await res.text().catch(() => "");
    throw new Error(`${path} -> ${res.status} ${detail}`);
  }
  return res.json() as Promise<T>;
}

// Discovers which genset/propulsion/battery/auxload instances are deployed,
// generated at deploy time from GENSET_COUNT/PROPULSION_COUNT/BATTERY_COUNT/
// AUXLOAD_COUNT (see nginx.conf.template and scripts/09-playground.sh).
export function fetchConfig(): Promise<PlaygroundConfig> {
  return apiFetch(`/config.json`);
}

export function fetchHealth(system: SystemName, id: string): Promise<{ status: string }> {
  return apiFetch(`/api/${system}/${id}/health`);
}

export function fetchStatus(system: SystemName, id: string): Promise<SystemStatus> {
  return apiFetch(`/api/${system}/${id}/status`);
}

export function fetchSwitchboardHealth(): Promise<{ status: string }> {
  return apiFetch(`/api/switchboard/health`);
}

export function fetchSwitchboardStatus(): Promise<SwitchboardStatus> {
  return apiFetch(`/api/switchboard/status`);
}

export function setLoad(
  system: SystemName,
  id: string,
  loadRatio: number
): Promise<{ target_load_ratio: number }> {
  return apiFetch(`/api/${system}/${id}/load`, {
    method: "POST",
    body: JSON.stringify({ load_ratio: loadRatio }),
  });
}

