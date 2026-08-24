export type SystemName = "genset" | "propulsion";
export type ServiceName = SystemName | "switchboard";

export interface SystemStatus {
  target_load_ratio: number;
  current_load_ratio: number;
  allocated_power_kw?: number;
  [key: string]: unknown;
}

export interface SwitchboardStatus {
  switchboard_id: string;
  available_supply_kw: number;
  total_demand_kw: number;
  gensets: Record<string, { power_kw: number; stale: boolean }>;
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

export function fetchHealth(system: ServiceName): Promise<{ status: string }> {
  return apiFetch(`/api/${system}/health`);
}

export function fetchStatus(system: SystemName): Promise<SystemStatus> {
  return apiFetch(`/api/${system}/status`);
}

export function fetchSwitchboardStatus(): Promise<SwitchboardStatus> {
  return apiFetch(`/api/switchboard/status`);
}

export function setLoad(
  system: SystemName,
  loadRatio: number
): Promise<{ target_load_ratio: number }> {
  return apiFetch(`/api/${system}/load`, {
    method: "POST",
    body: JSON.stringify({ load_ratio: loadRatio }),
  });
}
