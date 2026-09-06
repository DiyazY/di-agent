export type SystemName = "genset" | "propulsion" | "battery" | "auxload";

export interface PlaygroundConfig {
  gensetIds: string[];
  propulsionIds: string[];
  batteryIds: string[];
  auxloadIds: string[];
  shorePower: boolean;
}

export interface SystemStatus {
  target_load_ratio: number;
  current_load_ratio: number;
  allocated_power_kw?: number;
  speed_rpm?: number;
  soc?: number;
  soc_rate_per_hour?: number;
  time_to_empty_hr?: number;
  time_to_full_hr?: number;
  [key: string]: unknown;
}

export interface SwitchboardStatus {
  switchboard_id: string;
  available_supply_kw: number;
  total_demand_kw: number;
  total_co2_kg_per_s: number;
  total_nox_kg_per_s: number;
  gensets: Record<
    string,
    { power_kw: number; co2_kg_per_s: number; nox_kg_per_s: number; stale: boolean }
  >;
  batteries: Record<string, { power_kw: number; stale: boolean }>;
  consumers: Record<
    string,
    { requested_power_kw: number; priority: number; allocated_power_kw: number; stale: boolean }
  >;
}

export interface ShorePowerStatus {
  shore_power_id: string;
  connected: boolean;
  target_power_ratio: number;
  current_power_ratio: number;
  last_message?: {
    power_kw: number;
    losses_kw: number;
    [key: string]: unknown;
  } | null;
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

export function fetchShorePowerHealth(): Promise<{ status: string }> {
  return apiFetch(`/api/shore-power/health`);
}

export function fetchShorePowerStatus(): Promise<ShorePowerStatus> {
  return apiFetch(`/api/shore-power/status`);
}

export function setShorePowerConnected(connected: boolean): Promise<{ connected: boolean }> {
  return apiFetch(`/api/shore-power/connect`, {
    method: "POST",
    body: JSON.stringify({ connected }),
  });
}

export function setShorePowerRatio(
  powerRatio: number
): Promise<{ target_power_ratio: number }> {
  return apiFetch(`/api/shore-power/power`, {
    method: "POST",
    body: JSON.stringify({ power_ratio: powerRatio }),
  });
}

