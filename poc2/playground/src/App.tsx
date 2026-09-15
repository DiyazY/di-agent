import { useEffect, useState } from "react";
import "./App.css";
import { fetchConfig, PlaygroundConfig } from "./api";
import SystemPanel from "./components/SystemPanel";
import SwitchboardPanel from "./components/SwitchboardPanel";

export default function App() {
  // Which genset/propulsion/battery/auxload instances are deployed,
  // discovered from config.json (see nginx.conf.template and
  // scripts/09-playground.sh).
  const [config, setConfig] = useState<PlaygroundConfig | null>(null);

  useEffect(() => {
    fetchConfig()
      .then(setConfig)
      .catch(() =>
        setConfig({ gensetIds: [], propulsionIds: [], batteryIds: [], auxloadIds: [] })
      );
  }, []);

  return (
    <div className="app">
      <h1>di-agent Playground</h1>
      <p className="subtitle">
        Live control panel for the genset, battery, propulsion and auxiliary-load simulators.
      </p>
      <div className="panels">
        {/* ?? [] guards against a stale config.json (e.g. an old playground
            image) that predates the battery/auxload fields. */}
        {(config?.gensetIds ?? []).map((id) => (
          <SystemPanel key={id} system="genset" id={id} />
        ))}
        {(config?.batteryIds ?? []).map((id) => (
          <SystemPanel key={id} system="battery" id={id} />
        ))}
        <SwitchboardPanel />
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

