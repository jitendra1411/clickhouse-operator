#!/bin/bash

# Quick ClickHouseBackup Test Script
# For immediate validation of backup functionality

set -e

NAMESPACE="clickhouse-backup-test"
CHI_NAME="test-backup-chi"

echo "🚀 Quick ClickHouseBackup Test"
echo "=============================="

# Prerequisites check
echo "📋 Checking prerequisites..."

# Check kubectl
if ! command -v kubectl &> /dev/null; then
    echo "❌ kubectl not found. Please install kubectl."
    exit 1
fi

# Check if backup CRD exists
if ! kubectl get crd clickhousebackups.clickhouse.altinity.com &> /dev/null; then
    echo "⚠️  ClickHouseBackup CRD not found. Installing..."
    kubectl apply -f deploy/operator/parts/crd-backup.yaml
    echo "✅ CRD installed"
else
    echo "✅ ClickHouseBackup CRD found"
fi

# Check if test environment exists
if ! kubectl get namespace $NAMESPACE &> /dev/null; then
    echo "⚠️  Test environment not found. Setting up..."
    ./tests/backup/setup-test-environment.sh
    echo "✅ Test environment ready"
else
    echo "✅ Test environment found"
fi

# Check if CHI is ready
echo "🔍 Checking ClickHouse installation..."
if ! kubectl get chi $CHI_NAME -n $NAMESPACE &> /dev/null; then
    echo "❌ Test CHI not found. Run: ./tests/backup/setup-test-environment.sh"
    exit 1
fi

# Wait for CHI to be ready
echo "⏳ Waiting for ClickHouse to be ready..."
kubectl wait --for=condition=Ready pod -l clickhouse.altinity.com/chi=$CHI_NAME -n $NAMESPACE --timeout=300s

POD_NAME=$(kubectl get pods -n $NAMESPACE -l clickhouse.altinity.com/chi=$CHI_NAME -o jsonpath='{.items[0].metadata.name}')
echo "✅ ClickHouse pod ready: $POD_NAME"

# Test backup API
echo "🔍 Testing backup API..."
if kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-backup -- curl -s http://127.0.0.1:7171/ping &> /dev/null; then
    echo "✅ Backup API responding"
else
    echo "❌ Backup API not responding"
    exit 1
fi

# Create a quick test backup
echo "💾 Creating test backup..."
TEST_BACKUP_NAME="quick-test-$(date +%s)"

cat <<EOF | kubectl apply -n $NAMESPACE -f -
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseBackup
metadata:
  name: $TEST_BACKUP_NAME
spec:
  clickHouseInstallation:
    name: $CHI_NAME
    namespace: $NAMESPACE
  type: full
  tables:
    - "test_backup.users"
  includeSchema: true
  compression:
    type: gzip
    level: 1  # Fast compression for testing
EOF

echo "⏳ Waiting for backup to complete..."

# Wait for backup completion (with timeout)
timeout=180
elapsed=0
while [ $elapsed -lt $timeout ]; do
    status=$(kubectl get chb $TEST_BACKUP_NAME -n $NAMESPACE -o jsonpath='{.status.status}' 2>/dev/null || echo "Pending")
    
    case $status in
        "Completed")
            echo "✅ Backup completed successfully!"
            break
            ;;
        "Failed")
            echo "❌ Backup failed:"
            kubectl get chb $TEST_BACKUP_NAME -n $NAMESPACE -o jsonpath='{.status.error}'
            echo ""
            exit 1
            ;;
        *)
            echo "   Status: $status (elapsed: ${elapsed}s)"
            sleep 10
            elapsed=$((elapsed + 10))
            ;;
    esac
done

if [ $elapsed -ge $timeout ]; then
    echo "❌ Timeout waiting for backup to complete"
    exit 1
fi

# Verify backup exists
echo "🔍 Verifying backup exists..."
BACKUP_LIST=$(kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-backup -- curl -s http://127.0.0.1:7171/backup/list)
ACTUAL_BACKUP_NAME=$(kubectl get chb $TEST_BACKUP_NAME -n $NAMESPACE -o jsonpath='{.status.backupName}')

if echo "$BACKUP_LIST" | jq -e ".local[] | select(.name == \"$ACTUAL_BACKUP_NAME\")" > /dev/null 2>&1; then
    echo "✅ Backup found in storage: $ACTUAL_BACKUP_NAME"
else
    echo "❌ Backup not found in storage"
    echo "Available backups:"
    echo "$BACKUP_LIST" | jq -r '.local[].name // "No backups found"' 2>/dev/null || echo "Failed to parse backup list"
    exit 1
fi

# Show backup details
echo "📊 Backup details:"
kubectl get chb $TEST_BACKUP_NAME -n $NAMESPACE -o custom-columns="NAME:.metadata.name,STATUS:.status.status,BACKUP-NAME:.status.backupName,DURATION:.status.duration,SIZE:.status.backupSize"

# Cleanup
echo "🧹 Cleaning up test backup..."
kubectl delete chb $TEST_BACKUP_NAME -n $NAMESPACE

echo ""
echo "🎉 Quick test completed successfully!"
echo ""
echo "📋 What was tested:"
echo "   ✅ CRD installation"
echo "   ✅ Test environment setup"
echo "   ✅ ClickHouse readiness"
echo "   ✅ Backup API connectivity"
echo "   ✅ Backup resource creation"
echo "   ✅ Backup execution"
echo "   ✅ Storage verification"
echo ""
echo "🚀 Next steps:"
echo "   # Run comprehensive tests:"
echo "   ./tests/backup/run-tests.sh"
echo ""
echo "   # Test specific scenarios:"
echo "   kubectl apply -f tests/backup/test-examples/basic-backup.yaml"
echo "   kubectl apply -f tests/backup/test-examples/scheduled-backup.yaml"
echo "   kubectl apply -f tests/backup/test-examples/watch-mode-backup.yaml"
echo ""
echo "   # Monitor backups:"
echo "   kubectl get chb -n $NAMESPACE -w"
echo ""
echo "   # Check backup API:"
echo "   kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-backup -- curl -s http://127.0.0.1:7171/backup/list | jq"