    #!/bin/bash

    # Usage: ./03-uncloud.sh vm1_name vm2_name vm3_name

    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[1;33m'
    NC='\033[0m'

    # Useful for using network = "default"
    get_vm_ip() {
        virsh domifaddr "$1" | awk '/ipv4/ {print $4}' | cut -d'/' -f1 | head -n1
    }

    # get_vm_ip() {
    #     local vm="$1"
        
    #     # Get MAC address from virsh
    #     local mac=$(virsh domiflist "$vm" | grep -oE '[0-9a-f:]{17}' | head -1)
        
    #     if [ -z "$mac" ]; then
    #         echo "Error: Could not find MAC address for VM $vm" >&2
    #         return 1
    #     fi
        
    #     # Get the bridge network CIDR
    #     local network=$(ip -4 addr show nm-bridge | grep -oP '(?<=inet\s)\d+(\.\d+){3}/\d+')
        
    #     if [ -z "$network" ]; then
    #         echo "Error: Could not find network for nm-bridge" >&2
    #         return 1
    #     fi
        
    #     # Scan the network and find the IP matching the MAC
    #     sudo nmap -sn "$network" 2>/dev/null | \
    #         grep -B 2 -i "$mac" | \
    #         grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | \
    #         head -1
    # }

    wait_for_vm_ready() {
        local vm_name="$1"
        local max_attempts=40
        local attempt=1

        echo -e "${YELLOW}Waiting for VM $vm_name to get IP address...${NC}" >&2

        while [ $attempt -le $max_attempts ]; do
            local ip=$(get_vm_ip "$vm_name")
            if [ -n "$ip" ]; then
                echo -e "${GREEN}VM $vm_name has IP: $ip${NC}" >&2
                echo -e "${YELLOW}Waiting for SSH to be ready...${NC}" >&2

                local ssh_attempt=1
                while [ $ssh_attempt -le 10 ]; do
                    if timeout 2 bash -c "echo > /dev/tcp/$ip/22" 2>/dev/null; then
                        echo -e "${GREEN}SSH is ready on $ip${NC}" >&2
                        echo "$ip"   # this is the only stdout output
                        return 0
                    fi
                    sleep 2
                    ((ssh_attempt++))
                done
            fi
            sleep 3
            ((attempt++))
        done

        echo -e "${RED}Timeout waiting for VM $vm_name${NC}" >&2
        return 1
    }

    main() {
        command -v uc &>/dev/null || { echo -e "${RED}Error: 'uc' not found${NC}" >&2; exit 1; }
        [ -f ~/.ssh/id_ed25519 ] || { echo -e "${RED}Error: SSH key missing${NC}" >&2; exit 1; }

        [ $# -eq 0 ] && { echo "Usage: $0 vm1 [vm2 ...]" >&2; exit 1; }

        echo -e "${GREEN}Starting uncloud for VMs: $*${NC}"
        echo "=========================================="

        local first_vm=true

        for vm_name in "$@"; do
            echo -e "\n${GREEN}Processing VM: $vm_name${NC}"

            sudo virsh dominfo "$vm_name" &>/dev/null || { echo -e "${RED}VM $vm_name does not exist${NC}" >&2; continue; }

            state=$(sudo virsh domstate "$vm_name" 2>/dev/null | tr -d '\n')
            if [[ "$state" != "running" ]]; then
                echo -e "${YELLOW}Starting VM $vm_name...${NC}"
                sudo virsh start "$vm_name" &>/dev/null || { echo -e "${RED}Failed to start${NC}" >&2; continue; }
            fi

            # Get IP – now this variable contains ONLY the IP address
            vm_ip=$(wait_for_vm_ready "$vm_name")
            if [ -z "$vm_ip" ]; then
                echo -e "${RED}Skipping $vm_name (no IP)${NC}" >&2
                continue
            fi

            # Extra safety: one more short sleep for final network settling
            sleep 2

            if [ "$first_vm" = true ]; then
                echo -e "${YELLOW}Running: uc machine init ubuntu@$vm_ip --name $vm_name -y --no-caddy --no-dns${NC} --public-ip none"
                uc machine init "ubuntu@$vm_ip" --name "$vm_name" -y --no-caddy --no-dns --public-ip none
                first_vm=false
            else
                echo -e "${YELLOW}Running: uc machine add ubuntu@$vm_ip --name $vm_name -y "
                uc machine add "ubuntu@$vm_ip" --name "$vm_name" -y --no-caddy
            fi

            if [ $? -eq 0 ]; then
                echo -e "${GREEN}✓ Success: $vm_name ($vm_ip)${NC}"
            else
                echo -e "${RED}✗ Failed: $vm_name ($vm_ip)${NC}" >&2
            fi
            echo "------------------------------------------"
        done

        echo -e "\n${GREEN}Done.${NC}"
    }

    main "$@"