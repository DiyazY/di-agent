FROM ubuntu:22.04

# Install curl for health checks (optional but useful)
RUN apt-get update && apt-get install -y curl && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy the pre-built binary from your host (adjust the source path if needed)
# The script used BINARY_OUT; here we assume it's ./cmd/agent/tmp/di-agent-poc
COPY ./cmd/agent/tmp/di-agent-poc /tmp/di-agent

# Install the binary to /usr/local/bin and make it executable
RUN mv /tmp/di-agent /usr/local/bin/di-agent && \
    chmod +x /usr/local/bin/di-agent

# Create the directory for the environment file
RUN mkdir -p /etc/di-agent

# Copy an entrypoint script that generates /etc/di-agent/env from environment variables
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Expose the port the agent listens on
EXPOSE 9090

# Health check (matches your verification loop)
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -sf http://localhost:9090/healthz || exit 1

# Use the entrypoint to set up environment and then run the binary
ENTRYPOINT ["/entrypoint.sh"]