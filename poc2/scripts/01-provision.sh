#!/bin/bash

# Simple Terraform automation script
# Usage: ./terraform-auto.sh

set -e  # Exit on any error

# ============================================
# 1. Disable AppArmor for libvirt (if needed)
# ============================================
echo "🔍 Checking AppArmor profile for libvirt..."

# Check if the profile is currently loaded
if aa-status | grep -q "libvirtd"; then
    echo "⚠️  AppArmor libvirt profile is loaded. Unloading it now (requires sudo)."
    # Unload the profile and restart libvirtd
    sudo apparmor_parser -R /etc/apparmor.d/usr.sbin.libvirtd
    sudo systemctl restart libvirtd
    echo "✅ AppArmor profile unloaded and libvirtd restarted."
else
    echo "✅ AppArmor libvirt profile is already unloaded (or not present). Skipping."
fi
echo ""

# ============================================
# Continue with your existing Terraform steps
# ============================================

echo "🚀 Starting Terraform automation..."
echo ""

# Terraform Init
echo "📦 Running terraform init..."
terraform init
if [ $? -ne 0 ]; then
    echo "❌ Terraform init failed!"
    exit 1
fi
echo "✅ Terraform init completed successfully!"
echo ""

# Terraform Plan
echo "📋 Running terraform plan..."
terraform plan -out=tfplan
if [ $? -ne 0 ]; then
    echo "❌ Terraform plan failed!"
    exit 1
fi
echo "✅ Terraform plan completed successfully!"
echo ""

# Confirmation before apply
read -p "🚨 Do you want to apply these changes? (yes/no): " confirmation

if [[ "$confirmation" == "yes" || "$confirmation" == "y" ]]; then
    echo "🚀 Applying Terraform changes..."
    terraform apply tfplan
    if [ $? -ne 0 ]; then
        echo "❌ Terraform apply failed!"
        exit 1
    fi
    echo "✅ Terraform apply completed successfully!"
    rm -f tfplan  # Clean up plan file
else
    echo "❌ Apply cancelled by user"
    rm -f tfplan  # Clean up plan file
    exit 0
fi