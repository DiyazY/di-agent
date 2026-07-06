#!/bin/bash

# Script to run ROS Ansible playbooks
# Usage: ./run_ros_playbooks.sh

set -e  # Exit immediately if any command fails

echo "========================================="
echo "Starting ROS installation playbook..."
echo "========================================="

# Run the installation playbook
ansible-playbook -i inventory.ini install_ros.yml

# Check if the first playbook was successful
if [ $? -eq 0 ]; then
    echo "========================================="
    echo "ROS installation completed successfully!"
    echo "========================================="
    echo ""
    echo "========================================="
    echo "Starting ROS launch playbook..."
    echo "========================================="
    
    # Run the launch playbook
    ansible-playbook -i inventory.ini launch_ros.yml
    
    # Check if the second playbook was successful
    if [ $? -eq 0 ]; then
        echo "========================================="
        echo "ROS launch completed successfully!"
        echo "========================================="
    else
        echo "========================================="
        echo "ERROR: ROS launch playbook failed!"
        echo "========================================="
        exit 1
    fi
else
    echo "========================================="
    echo "ERROR: ROS installation playbook failed!"
    echo "========================================="
    exit 1
fi

echo ""
echo "All playbooks executed successfully!"