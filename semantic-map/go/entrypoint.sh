#!/bin/bash
set -e

NODE_ID=${NODE_ID:-"node1"}
REGIME=${REGIME:-"bursty"}
PORT=${PORT:-"9090"}  # Add this

cat > /etc/di-agent/env <<EOF
NODE_ID=${NODE_ID}
REGIME=${REGIME}
EOF

echo "Starting di-agent with NODE_ID=${NODE_ID}, REGIME=${REGIME}, PORT=${PORT}"

# Pass the port if your agent supports it
exec /usr/local/bin/di-agent -addr ":${PORT}" "$@"