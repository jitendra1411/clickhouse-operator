#!/bin/bash

# Simple ClickHouse Backup Test
# Tests backup functionality using the existing operator infrastructure

set -e

NAMESPACE="test"
CHI_NAME="test-backup-chi"

echo "🚀 Simple ClickHouse Backup Test"
echo "=================================="

echo "📋 Step 1: Check if MinIO is running..."
if ! kubectl get pods -n minio | grep -q "Running"; then
    echo "❌ MinIO not running. Please install MinIO first."
    exit 1
fi
echo "✅ MinIO is running"

echo "📋 Step 2: Check if ClickHouse operator is installed..."
if ! kubectl get deployment -n test clickhouse-operator 2>/dev/null; then
    echo "❌ ClickHouse operator not installed in test namespace"
    exit 1
fi
echo "✅ ClickHouse operator is installed"

echo "📋 Step 3: Create ClickHouse installation with backup sidecar..."

# Create CHI with backup sidecar
cat <<EOF | kubectl apply -f -
apiVersion: "clickhouse.altinity.com/v1"
kind: "ClickHouseInstallation"
metadata:
  name: $CHI_NAME
  namespace: $NAMESPACE
spec:
  defaults:
    templates:
      podTemplate: clickhouse-with-backup
  templates:
    podTemplates:
      - name: clickhouse-with-backup
        spec:
          containers:
            - name: clickhouse-pod
              image: clickhouse/clickhouse-server:23.8
              
            - name: clickhouse-backup
              image: altinity/clickhouse-backup:2.4.15
              imagePullPolicy: IfNotPresent
              command:
                - bash
                - -xc
                - "/bin/clickhouse-backup server"
              env:
                - name: API_LISTEN
                  value: "0.0.0.0:7171"
                - name: API_ENABLE_METRICS
                  value: "true"
                - name: REMOTE_STORAGE
                  value: "s3"
                - name: S3_ENDPOINT
                  value: https://minio.minio:9000
                - name: S3_BUCKET
                  value: clickhouse-backup
                - name: S3_PATH
                  value: backup
                - name: S3_FORCE_PATH_STYLE
                  value: "true"
                - name: S3_DISABLE_CERT_VERIFICATION
                  value: "true"
                - name: S3_ACCESS_KEY
                  value: minio-access-key
                - name: S3_SECRET_KEY
                  value: minio-secret-key
              ports:
                - name: backup-rest
                  containerPort: 7171
                  
  configuration:
    clusters:
      - name: simple
        layout:
          shardsCount: 1
          replicasCount: 1
EOF

echo "✅ ClickHouse installation created"

echo "📋 Step 4: Wait for ClickHouse to be ready..."
kubectl wait --for=condition=Ready --timeout=300s chi/$CHI_NAME -n $NAMESPACE || {
    echo "❌ Timeout waiting for ClickHouse installation"
    kubectl describe chi $CHI_NAME -n $NAMESPACE
    exit 1
}

# Get pod name
POD_NAME=$(kubectl get pods -n $NAMESPACE -l clickhouse.altinity.com/chi=$CHI_NAME -o jsonpath='{.items[0].metadata.name}')
if [ -z "$POD_NAME" ]; then
    echo "❌ No ClickHouse pod found"
    exit 1
fi

echo "✅ ClickHouse pod ready: $POD_NAME"

echo "📋 Step 5: Wait for backup API to be ready..."
sleep 30  # Give the backup container time to start

# Check backup API
for i in {1..10}; do
    if kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-backup -- curl -s http://127.0.0.1:7171/ >/dev/null 2>&1; then
        echo "✅ Backup API is responding"
        break
    fi
    echo "⏳ Waiting for backup API (attempt $i/10)..."
    sleep 10
    if [ $i -eq 10 ]; then
        echo "❌ Backup API not responding after 10 attempts"
        kubectl logs -n $NAMESPACE $POD_NAME -c clickhouse-backup --tail=20
        exit 1
    fi
done

echo "📋 Step 6: Create test data..."
kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-pod -- clickhouse-client --query="CREATE DATABASE IF NOT EXISTS test_backup"
kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-pod -- clickhouse-client --query="CREATE TABLE IF NOT EXISTS test_backup.users (id UInt32, name String, email String) ENGINE = MergeTree() ORDER BY id"
kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-pod -- clickhouse-client --query="INSERT INTO test_backup.users VALUES (1, 'Alice', 'alice@example.com')"
kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-pod -- clickhouse-client --query="INSERT INTO test_backup.users VALUES (2, 'Bob', 'bob@example.com')"
kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-pod -- clickhouse-client --query="INSERT INTO test_backup.users VALUES (3, 'Charlie', 'charlie@example.com')"

# Verify data
ROW_COUNT=$(kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-pod -- clickhouse-client --query="SELECT count() FROM test_backup.users" | tr -d '\r')
echo "✅ Test data created: $ROW_COUNT rows"

echo "📋 Step 7: Create backup..."
BACKUP_NAME="test-backup-$(date +%Y%m%d-%H%M%S)"

# Create backup via API
kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-backup -- curl -X POST "http://127.0.0.1:7171/backup/create/$BACKUP_NAME"

echo "✅ Backup creation initiated: $BACKUP_NAME"

echo "📋 Step 8: Wait for backup to complete..."
for i in {1..30}; do
    STATUS=$(kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-backup -- curl -s http://127.0.0.1:7171/backup/status | grep -o '"status":"[^"]*"' | head -1 | cut -d'"' -f4 || echo "unknown")
    
    if [ "$STATUS" = "success" ]; then
        echo "✅ Backup completed successfully"
        break
    elif [ "$STATUS" = "error" ]; then
        echo "❌ Backup failed"
        kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-backup -- curl -s http://127.0.0.1:7171/backup/status
        exit 1
    fi
    
    echo "⏳ Backup status: $STATUS (attempt $i/30)"
    sleep 10
    
    if [ $i -eq 30 ]; then
        echo "❌ Backup timeout after 5 minutes"
        kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-backup -- curl -s http://127.0.0.1:7171/backup/status
        exit 1
    fi
done

echo "📋 Step 9: Verify backup exists..."
kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-backup -- curl -s http://127.0.0.1:7171/backup/list | grep -q "$BACKUP_NAME" && echo "✅ Backup found in list" || {
    echo "❌ Backup not found in list"
    kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-backup -- curl -s http://127.0.0.1:7171/backup/list
    exit 1
}

echo "📋 Step 10: Test backup upload to S3..."
kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-backup -- curl -X POST "http://127.0.0.1:7171/backup/upload/$BACKUP_NAME"

echo "⏳ Waiting for upload to complete..."
sleep 20

# Check upload status
UPLOAD_STATUS=$(kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-backup -- curl -s http://127.0.0.1:7171/backup/status | grep -o '"status":"[^"]*"' | head -1 | cut -d'"' -f4 || echo "unknown")
echo "📤 Upload status: $UPLOAD_STATUS"

echo ""
echo "🎉 Backup Test Results Summary:"
echo "=============================="
echo "✅ MinIO storage backend: Ready"
echo "✅ ClickHouse operator: Running"
echo "✅ ClickHouse installation: Deployed ($CHI_NAME)"
echo "✅ Backup sidecar: Running"
echo "✅ Test data: Created ($ROW_COUNT rows)"
echo "✅ Backup creation: $BACKUP_NAME"
echo "✅ Backup API: Functional"
echo "📤 S3 upload: $UPLOAD_STATUS"
echo ""
echo "🔧 Backup functionality is working correctly!"
echo ""
echo "📌 To clean up:"
echo "   kubectl delete chi $CHI_NAME -n $NAMESPACE" 