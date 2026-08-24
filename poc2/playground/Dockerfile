# --- build the React app ---
FROM node:20-alpine AS build
WORKDIR /app
COPY package*.json ./
RUN npm install
RUN npm install --save-dev @types/node
COPY . .
RUN npm run build

# --- serve with nginx, proxying /api/* to the genset/propulsion Services ---
FROM nginx:1.27-alpine
COPY --from=build /app/dist /usr/share/nginx/html
# nginx's entrypoint envsubst's *.template into conf.d/ on startup.
COPY nginx.conf.template /etc/nginx/templates/default.conf.template
ENV GENSET_UPSTREAM=genset.default.svc.cluster.local:8000 \
    PROPULSION_UPSTREAM=propulsion.default.svc.cluster.local:8000 \
    SWITCHBOARD_UPSTREAM=switchboard.default.svc.cluster.local:8000
EXPOSE 80
