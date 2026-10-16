#!/bin/bash

# 02-stopVMS.sh - Script to stop VMs using virsh

# Check if at least one VM name is provided
if [ $# -eq 0 ]; then
    echo "Error: No VM names provided"
    echo "Usage: $0 <vm1> <vm2> <vm3> ..."
    exit 1
fi

# Function to stop a single VM
stop_vm() {
    local vm_name="$1"
    
    echo "Processing VM: $vm_name"
    
    # Check if VM exists
    if ! virsh dominfo "$vm_name" &>/dev/null; then
        echo "  ✗ VM '$vm_name' does not exist"
        return 1
    fi
    
    # Get VM state
    local state=$(virsh domstate "$vm_name" 2>/dev/null | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
    
    case "$state" in
        "running")
            echo "  → VM is running, stopping it..."
            if virsh destroy "$vm_name" &>/dev/null; then
                echo "  ✓ VM '$vm_name' stopped successfully (forced)"
            else
                echo "  ✗ Failed to stop VM '$vm_name'"
                return 1
            fi
            ;;
        "paused")
            echo "  → VM is paused, resuming and stopping..."
            virsh resume "$vm_name" &>/dev/null
            sleep 2
            if virsh destroy "$vm_name" &>/dev/null; then
                echo "  ✓ VM '$vm_name' stopped successfully"
            else
                echo "  ✗ Failed to stop VM '$vm_name'"
                return 1
            fi
            ;;
        "shut off")
            echo "  ✓ VM '$vm_name' is already stopped"
            ;;
        *)
            echo "  ? VM '$vm_name' is in unknown state: $state"
            return 1
            ;;
    esac
    
    return 0
}

# Main execution
echo "========================================="
echo "Stopping Virtual Machines"
echo "========================================="
echo ""

# Counter for tracking success/failure
success_count=0
fail_count=0

# Loop through all provided VM names
for vm in "$@"; do
    if stop_vm "$vm"; then
        ((success_count++))
    else
        ((fail_count++))
    fi
    echo ""
done

# Summary
echo "========================================="
echo "Summary: $success_count VMs stopped successfully, $fail_count failed"
echo "========================================="

# Exit with error if any VM failed
if [ $fail_count -gt 0 ]; then
    exit 1
fi

exit 0