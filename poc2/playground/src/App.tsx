import { useEffect, useState } from "react";
import "./App.css";
import { fetchConfig, PlaygroundConfig } from "./api";
import SystemPanel from "./components/SystemPanel";
import SwitchboardPanel from "./components/SwitchboardPanel";
import ShorePowerPanel from "./components/ShorePowerPanel";
import ConditionsPanel from "./components/ConditionsPanel";
import VoyagePanel from "./components/VoyagePanel";

export default function App() {
  // Which genset/propulsion/battery/auxload instances are deployed,
  // discovered from config.json (see nginx.conf.template and
  // scripts/09-playground.sh).
  const [config, setConfig] = useState<PlaygroundConfig | null>(null);
  const [configError, setConfigError] = useState<string | null>(null);

  useEffect(() => {
    fetchConfig()
      .then(setConfig)
      .catch((error: unknown) => {
        setConfigError(error instanceof Error ? error.message : "Unable to load playground configuration.");
      });
  }, []);

  return (
    <div className="app">
      <h1>di-agent Playground</h1>
      <p className="subtitle">
        Live control panel for the genset, battery, propulsion and auxiliary-load simulators.
      </p>
      {configError && (
        <p className="config-error" role="alert">
          Playground configuration could not be loaded: {configError}
        </p>
      )}
      <div className="panels">
        {/* ?? [] guards against a stale config.json (e.g. an old playground
            image) that predates the battery/auxload fields. */}
        {(config?.gensetIds ?? []).map((id) => (
          <SystemPanel key={id} system="genset" id={id} />
        ))}
        {(config?.batteryIds ?? []).map((id) => (
          <SystemPanel key={id} system="battery" id={id} />
        ))}
        {config?.shorePower && <ShorePowerPanel />}
        <SwitchboardPanel />
        <VoyagePanel propulsionIds={config?.propulsionIds ?? []} />
        <ConditionsPanel
          propulsionIds={config?.propulsionIds ?? []}
          auxloadIds={config?.auxloadIds ?? []}
        />
        {(config?.propulsionIds ?? []).map((id) => (
          <SystemPanel key={id} system="propulsion" id={id} />
        ))}
        {(config?.auxloadIds ?? []).map((id) => (
          <SystemPanel key={id} system="auxload" id={id} />
        ))}
      </div>
    </div>
  );
}
