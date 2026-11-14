import { FormEvent, useCallback, useEffect, useState } from "react";
import {
  DatasetCollectorStatus,
  DatasetConfig,
  DatasetStatus,
  fetchDatasetCollectorStatus,
  fetchDatasetStatus,
  PlaygroundConfig,
  updateDatasetConfig,
} from "../api";

type Settings = Omit<DatasetConfig, "run_id">;

function settings(config: DatasetConfig): Settings {
  const { mode, interval_ms, points_per_interval, loop, random_seed } = config;
  return { mode, interval_ms, points_per_interval, loop, random_seed };
}

export default function DatasetPanel({ config }: { config: NonNullable<PlaygroundConfig["dataset"]> }) {
  const [status, setStatus] = useState<DatasetStatus | null>(null);
  const [draft, setDraft] = useState<Settings | null>(null);
  const [collector, setCollector] = useState<DatasetCollectorStatus | null>(null);
  const [streamError, setStreamError] = useState<string | null>(null);
  const [collectorError, setCollectorError] = useState<string | null>(null);
  const [applyError, setApplyError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const refresh = useCallback(async () => {
    try {
      const next = await fetchDatasetStatus();
      setStatus(next);
      setDraft((current) => current ?? settings(next.config));
      setStreamError(null);
    } catch (error) {
      setStreamError(error instanceof Error ? error.message : String(error));
    }
    if (config.collectorEnabled) {
      try {
        setCollector(await fetchDatasetCollectorStatus());
        setCollectorError(null);
      } catch (error) {
        setCollectorError(error instanceof Error ? error.message : String(error));
      }
    }
  }, [config.collectorEnabled]);

  useEffect(() => {
    void refresh();
    const timer = setInterval(() => void refresh(), 2000);
    return () => clearInterval(timer);
  }, [refresh]);

  const apply = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!draft || !status) return;
    if (!window.confirm(
      "This changes the shared stream and restarts it from the beginning. Mode changes may temporarily mix origins in an agent collection window. Continue?"
    )) return;
    setSubmitting(true);
    setApplyError(null);
    try {
      const updated = await updateDatasetConfig(draft);
      setDraft(settings(updated));
      await refresh();
    } catch (error) {
      setApplyError(error instanceof Error ? error.message : String(error));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <section className="panel dataset-panel">
      <h2>Engine dataset</h2>
      <p>Recorded replay uses stored measurements, not a live engine. Synthetic values are generated from the selected run&apos;s profiles.</p>
      <div className="metric-row"><span>Run</span><strong>{status?.config.run_id ?? "--"}</strong></div>
      <div className="metric-row"><span>Active mode</span><strong>{status?.config.mode === "real" ? "Recorded replay" : status?.config.mode ?? "--"}</strong></div>
      {status && <div className="metric-row"><span>Active pacing</span><strong>{status.config.points_per_interval} step(s) / {status.config.interval_ms} ms</strong></div>}
      {status && <div className="metric-row"><span>Active loop / seed</span><strong>{status.config.loop ? "On" : "Off"} / {status.config.mode === "synthetic" ? status.config.random_seed : "Not used"}</strong></div>}
      <div className="metric-row"><span>Stream state</span><strong>{streamError ? "Unavailable" : status?.state ?? "Loading"}</strong></div>
      <div className="metric-row"><span>Published samples</span><strong>{status?.published ?? "--"}</strong></div>
      <div className="metric-row"><span>Collector</span><strong>{!config.collectorEnabled ? "Disabled" : collectorError ? "Unavailable" : collector ? (collector.connected ? "Connected" : "Not ready") : "Loading"}</strong></div>
      <div className="metric-row"><span>Configured dataset agent node</span><strong>{config.agentNodeName || "Not configured"}</strong></div>
      {collector && !collectorError && <div className="metric-row"><span>Collector received / dropped</span><strong>{collector.received} / {collector.dropped}</strong></div>}
      {streamError && <p className="error-text" role="alert">Stream status: {streamError}</p>}
      {status?.last_error && <p className="error-text" role="alert">Stream error: {status.last_error}</p>}
      {collectorError && <p className="error-text" role="alert">Collector status: {collectorError}</p>}
      {collector?.last_error && !collectorError && <p className="error-text" role="alert">Collector error: {collector.last_error}</p>}
      <p className="hint-text">Changes restart the shared stream. Settings are temporary and revert after a pod restart. There is no pause or seek.</p>
      {draft && (
        <form className="dataset-controls" onSubmit={(event) => void apply(event)}>
          <label htmlFor="dataset-mode">Mode</label>
          <select id="dataset-mode" value={draft.mode} onChange={(e) => setDraft({ ...draft, mode: e.target.value as DatasetConfig["mode"] })}>
            <option value="synthetic">Synthetic</option>
            <option value="real">Recorded replay</option>
          </select>
          <label htmlFor="dataset-interval">Interval (ms)</label>
          <input id="dataset-interval" type="number" min="10" max="3600000" step="1" required value={draft.interval_ms} onChange={(e) => setDraft({ ...draft, interval_ms: e.target.valueAsNumber })} />
          <label htmlFor="dataset-points">Time steps per interval</label>
          <input id="dataset-points" type="number" min="1" max="10000" step="1" required value={draft.points_per_interval} onChange={(e) => setDraft({ ...draft, points_per_interval: e.target.valueAsNumber })} />
          <label htmlFor="dataset-seed">Synthetic seed</label>
          <input id="dataset-seed" type="number" step="1" required disabled={draft.mode !== "synthetic"} value={draft.random_seed} onChange={(e) => setDraft({ ...draft, random_seed: e.target.valueAsNumber })} />
          <label className="dataset-checkbox"><input type="checkbox" checked={draft.loop} onChange={(e) => setDraft({ ...draft, loop: e.target.checked })} /> Loop</label>
          <button type="submit" disabled={submitting || !!streamError}>{submitting ? "Applying..." : "Apply settings"}</button>
        </form>
      )}
      {applyError && <p className="error-text" role="alert">Could not apply settings: {applyError}</p>}
      <p>Each time step emits one sample per active signal. The agent normally averages samples over a 5-second collection window; its engine properties are not engine-health constructs. Values may use dataset-native units and observed bounds, not physical limits.</p>
      {config.grafanaDashboardUrl ? (
        <a href={config.grafanaDashboardUrl} target="_blank" rel="noreferrer">Open dataset telemetry dashboard</a>
      ) : (
        <p>Set the playground Grafana dashboard URL to link to raw telemetry charts.</p>
      )}
    </section>
  );
}
