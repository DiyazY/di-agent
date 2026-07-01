#!/bin/bash

# Simple Terraform automation script
# Usage: ./terraform-auto.sh

set -e  # Exit on any error

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