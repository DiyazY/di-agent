#!/bin/bash

# 02-startVMS.sh - Start virtual machines using virsh

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Function to start a single VM
start_vm() {
    local vm_name="$1"
    
    if [ -z "$vm_name" ]; then
        return
    fi
    
    # Check if VM exists
    if ! virsh dominfo "$vm_name" &>/dev/null; then
        echo -e "${RED}✗ VM '$vm_name' does not exist${NC}"
        return 1
    fi
    
    # Check VM state
    state=$(virsh domstate "$vm_name" 2>/dev/null | tr -d '\n')
    
    case "$state" in
        "running")
            echo -e "${YELLOW}⚠ VM '$vm_name' is already running${NC}"
            return 0
            ;;
        "paused")
            echo -e "${YELLOW}⚠ VM '$vm_name' is paused, resuming...${NC}"
            virsh resume "$vm_name" && \
                echo -e "${GREEN}✓ VM '$vm_name' resumed${NC}"
            return $?
            ;;
        "shut off")
            echo -e "Starting VM '$vm_name'..."
            virsh start "$vm_name" && \
                echo -e "${GREEN}✓ VM '$vm_name' started successfully${NC}"
            return $?
            ;;
        *)
            echo -e "${YELLOW}⚠ VM '$vm_name' is in unknown state: $state${NC}"
            return 1
            ;;
    esac
}

# Main execution
echo "========================================"
echo "  Starting Virtual Machines"
echo "========================================"

# Count number of VMs provided
vm_count=0
failed_count=0

# Loop through all arguments
for vm in "$@"; do
    if [ -n "$vm" ]; then
        ((vm_count++))
        echo ""
        echo "[$vm_count] Processing: $vm"
        if ! start_vm "$vm"; then
            ((failed_count++))
        fi
    fi
done

# Summary
echo ""
echo "========================================"
if [ $vm_count -eq 0 ]; then
    echo -e "${RED}✗ No VM names provided${NC}"
    exit 1
else
    echo -e "Total VMs: $vm_count"
    if [ $failed_count -eq 0 ]; then
        echo -e "${GREEN}✓ All VMs started successfully${NC}"
        exit 0
    else
        echo -e "${RED}✗ $failed_count VM(s) failed to start${NC}"
        exit 1
    fi
fi