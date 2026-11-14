# di-agent Playground

The React/Vite playground controls the optional ship simulators and, when
enabled, displays and controls the synthetic and recorded engine dataset
stream. The dataset controls do not operate a physical engine. See the
[poc2 deployment guide](../README.md#engine-dataset-playground) for enabling
the panel, restricting access, and accessing its Grafana dashboard.

## Local development

Run the services locally or port-forward them from the cluster, then start:

```bash
VITE_GENSET_TARGET=http://localhost:8001 \
VITE_PROPULSION_TARGET=http://localhost:8002 \
VITE_DATASET_TARGET=http://localhost:8000 \
VITE_DATASET_COLLECTOR_TARGET=http://localhost:8003 \
npm run dev
```

The development server proxies the dataset streamer and collector APIs.
To show the dataset panel locally, set `dataset.enabled` to `true` in
`public/config.json` and provide `collectorEnabled`, `agentNodeName` and
`grafanaDashboardUrl`. This development proxy does **not** authenticate writes;
only use it on a trusted local interface.

Run stubbed Cypress tests against a local preview server:

```bash
npm run build
npm run preview
# In another terminal:
npm run cy:run
```

`npm run cy:run:live` uses real ship-simulator APIs and requires the deployed
services. The dataset tests are stubbed; they do not prove end-to-end Kafka,
InfluxDB, or agent ingestion.

The container uses nginx and the Helm chart to expose configured services.
Dataset replay writes are unauthenticated; restrict access to trusted
developers and networks.
