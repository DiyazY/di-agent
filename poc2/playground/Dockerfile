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
ENV SWITCHBOARD_UPSTREAM=switchboard.default.svc.cluster.local:8000 \
    SHORE_POWER_UPSTREAM=shore-power.default.svc.cluster.local:8000 \
    GENSET_LOCATIONS="location /api/genset/genset/ { proxy_pass http://genset.default.svc.cluster.local:8000/; proxy_set_header Host $host; }" \
    PROPULSION_LOCATIONS="location /api/propulsion/propulsion/ { proxy_pass http://propulsion.default.svc.cluster.local:8000/; proxy_set_header Host $host; }" \
    BATTERY_LOCATIONS="location /api/battery/battery/ { proxy_pass http://battery.default.svc.cluster.local:8000/; proxy_set_header Host $host; }" \
    AUXLOAD_LOCATIONS="location /api/auxload/auxload/ { proxy_pass http://auxload.default.svc.cluster.local:8000/; proxy_set_header Host $host; }" \
    CONFIG_JSON="{\"gensetIds\":[\"genset\"],\"propulsionIds\":[\"propulsion\"],\"batteryIds\":[\"battery\"],\"auxloadIds\":[\"auxload\"],\"shorePower\":true}"
EXPOSE 80
