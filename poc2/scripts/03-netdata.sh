#!/bin/bash
# Netdata Local Installation Script
# Usage: ./install_netdata.sh

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

echo -e "${GREEN}Installing Netdata...${NC}"

# Check if running as root
if [[ $EUID -ne 0 ]]; then
   echo -e "${RED}This script must be run as root${NC}" 
   exit 1
fi

# Install dependencies
echo "Installing dependencies..."
if command -v apt-get &>/dev/null; then
    apt-get update -qq
    apt-get install -y -qq curl wget git
elif command -v yum &>/dev/null; then
    yum install -y -q curl wget git
elif command -v dnf &>/dev/null; then
    dnf install -y -q curl wget git
else
    echo -e "${RED}Unsupported package manager${NC}"
    exit 1
fi

# Download and install Netdata
echo "Downloading Netdata installer..."
curl -s https://my-netdata.io/kickstart.sh > /tmp/netdata-kickstart.sh
chmod +x /tmp/netdata-kickstart.sh

echo "Installing Netdata..."
/tmp/netdata-kickstart.sh --non-interactive --stable-channel --disable-telemetry

# Start and enable Netdata
echo "Starting Netdata..."
systemctl start netdata
systemctl enable netdata

# Check status
if systemctl is-active --quiet netdata; then
    echo -e "${GREEN}Netdata installed successfully!${NC}"
    echo "Access Netdata at: http://localhost:19999"
else
    echo -e "${RED}Netdata installation failed${NC}"
    exit 1
fi

# Cleanup
rm -f /tmp/netdata-kickstart.sh

echo -e "${GREEN}Installation complete!${NC}"