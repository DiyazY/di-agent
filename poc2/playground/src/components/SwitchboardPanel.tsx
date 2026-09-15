import { useCallback, useEffect, useState } from "react";
import { fetchSwitchboardHealth, fetchSwitchboardStatus, SwitchboardStatus } from "../api";

const POLL_INTERVAL_MS = 2000;

export default function SwitchboardPanel() {
  const [status, setStatus] = useState<SwitchboardStatus | null>(null);
  const [healthy, setHealthy] = useState<boolean | null>(null);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      const [health, statusData] = await Promise.all([
        fetchSwitchboardHealth(),
        fetchSwitchboardStatus(),
      ]);
      setHealthy(health.status === "ok");
      setStatus(statusData);
      setError(null);
    } catch (err) {
      setHealthy(false);
      setError(err instanceof Error ? err.message : String(err));
    }
  }, []);

  useEffect(() => {
    refresh();
    const id = setInterval(refresh, POLL_INTERVAL_MS);
    return () => clearInterval(id);
  }, [refresh]);

  return (
    <div className="panel">
      <h2>
        <span
          className={`status-dot ${
            healthy === null ? "" : healthy ? "ok" : "error"
          }`}
        />
        switchboard
      </h2>

      <div className="metric-row">
        <span>Available supply</span>
        <strong>
          {status ? `${status.available_supply_kw.toFixed(1)} kW` : "--"}
        </strong>
      </div>
      <div className="metric-row">
        <span>Total demand</span>
        <strong>
          {status ? `${status.total_demand_kw.toFixed(1)} kW` : "--"}
        </strong>
      </div>
      <div className="metric-row">
        <span>System CO2 emissions</span>
        <strong>
          {status ? `${(status.total_co2_kg_per_s * 3600).toFixed(2)} kg/h` : "--"}
        </strong>
      </div>
      <div className="metric-row">
        <span>System NOx emissions</span>
        <strong>
          {status ? `${(status.total_nox_kg_per_s * 3600).toFixed(3)} kg/h` : "--"}
        </strong>
      </div>

      {status && Object.keys(status.gensets ?? {}).length > 0 && (
        <ul className="entry-list">
          {Object.entries(status.gensets).map(([id, genset]) => (
            <li key={id}>
              {id}: {genset.power_kw.toFixed(1)} kW, CO2{" "}
              {(genset.co2_kg_per_s * 3600).toFixed(2)} kg/h, NOx{" "}
              {(genset.nox_kg_per_s * 3600).toFixed(3)} kg/h
              {genset.stale ? " (stale)" : ""}
            </li>
          ))}
        </ul>
      )}

      {status && Object.keys(status.batteries ?? {}).length > 0 && (
        <ul className="entry-list">
          {Object.entries(status.batteries).map(([id, battery]) => (
            <li key={id}>
              {id}: {battery.power_kw.toFixed(1)} kW{battery.stale ? " (stale)" : ""}
            </li>
          ))}
        </ul>
      )}

      {status && Object.keys(status.consumers ?? {}).length > 0 && (
        <ul className="entry-list">
          {Object.entries(status.consumers).map(([id, consumer]) => (
            <li key={id}>
              {id}: {consumer.allocated_power_kw.toFixed(1)} /{" "}
              {consumer.requested_power_kw.toFixed(1)} kW
              {consumer.stale ? " (stale)" : ""}
            </li>
          ))}
        </ul>
      )}

      {error && <p className="error-text">{error}</p>}
    </div>
  );
}
