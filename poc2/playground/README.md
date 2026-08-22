# di-agent Playground

A small React + Vite dashboard for interacting with the genset and
propulsion simulator APIs from [poc2](..). It shows live load-ratio
status and lets you set a new target load ratio for each system.

## Local development

Run the genset/propulsion APIs locally (or port-forward them from the
cluster) and start the dev server:

```bash
npm install
VITE_GENSET_TARGET=http://<genset-ip>:8000 \
VITE_PROPULSION_TARGET=http://<propulsion-ip>:8000 \
npm run dev
```

The dev server proxies `/api/genset/*` and `/api/propulsion/*` to those
targets (see [vite.config.ts](vite.config.ts)), so the browser only ever
talks to the same origin (no CORS setup needed).

## Container image

The [Dockerfile](Dockerfile) builds the app and serves it via nginx.
nginx renders [nginx.conf.template](nginx.conf.template) at container
start, substituting `GENSET_UPSTREAM` / `PROPULSION_UPSTREAM` env vars
(defaulting to the in-cluster Service DNS names) to reverse-proxy
`/api/genset/*` and `/api/propulsion/*`.

## Deploying to the poc2 cluster

The playground runs as its own pod in the same kubeadm cluster as
genset/propulsion (see [poc2/ARCHITECTURE.md](../ARCHITECTURE.md)),
exposed via a NodePort Service so it's reachable from outside the cluster.

```bash
cd ..
make playground            # builds the image, imports it, applies the manifest
```

This runs [scripts/09-playground.sh](../scripts/09-playground.sh),
which:
1. Builds the `playground` Docker image from this directory.
2. Imports it into containerd on the target VM (no registry, matching
   `06-genset.sh` / `06b-propulsion.sh`).
3. Applies `config/genset-service.yaml` and
   `config/propulsion-service.yaml` (ClusterIP Services so the frontend can
   reach them by stable DNS name).
4. Renders and applies `config/playground-deployment.yaml` (Deployment +
   NodePort Service).

Once ready, open `http://<vm-ip>:${PLAYGROUND_NODE_PORT}` (default port
`30080`, see [poc2/config/.env](../config/.env)).
