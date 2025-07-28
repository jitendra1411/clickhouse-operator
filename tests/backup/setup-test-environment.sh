#!/bin/bash

# ClickHouseBackup Test Environment Setup Script
# This script sets up a complete test environment for backup functionality

set -e

NAMESPACE=${NAMESPACE:-"clickhouse-backup-test"}
CHI_NAME=${CHI_NAME:-"test-backup-chi"}
MINIO_NAMESPACE=${MINIO_NAMESPACE:-"minio-test"}

echo "🚀 Setting up ClickHouseBackup test environment..."

# Create test namespace
echo "📁 Creating test namespace: $NAMESPACE"
kubectl create namespace $NAMESPACE --dry-run=client -o yaml | kubectl apply -f -

# Install MinIO for storage testing (optional)
setup_minio() {
    echo "🗄️  Setting up MinIO for storage testing..."
    
    kubectl create namespace $MINIO_NAMESPACE --dry-run=client -o yaml | kubectl apply -f -
    
    # Deploy MinIO
    cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: minio
  namespace: $MINIO_NAMESPACE
spec:
  replicas: 1
  selector:
    matchLabels:
      app: minio
  template:
    metadata:
      labels:
        app: minio
    spec:
      containers:
      - name: minio
        image: minio/minio:latest
        args:
        - server
        - /data
        - --console-address
        - ":9001"
        env:
        - name: MINIO_ACCESS_KEY
          value: "testuser"
        - name: MINIO_SECRET_KEY
          value: "testpass123"
        ports:
        - containerPort: 9000
        - containerPort: 9001
        volumeMounts:
        - name: data
          mountPath: /data
      volumes:
      - name: data
        emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: minio
  namespace: $MINIO_NAMESPACE
spec:
  selector:
    app: minio
  ports:
  - name: api
    port: 9000
    targetPort: 9000
  - name: console
    port: 9001
    targetPort: 9001
  type: ClusterIP
EOF

    # Wait for MinIO to be ready
    echo "⏳ Waiting for MinIO to be ready..."
    kubectl wait --for=condition=available deployment/minio -n $MINIO_NAMESPACE --timeout=300s
    
    # Create test bucket
    MINIO_POD=$(kubectl get pods -n $MINIO_NAMESPACE -l app=minio -o jsonpath='{.items[0].metadata.name}')
    kubectl exec -n $MINIO_NAMESPACE $MINIO_POD -- mc alias set local http://localhost:9000 testuser testpass123
    kubectl exec -n $MINIO_NAMESPACE $MINIO_POD -- mc mb local/clickhouse-backups || true
    
    echo "✅ MinIO setup complete"
    echo "   Access Key: testuser"
    echo "   Secret Key: testpass123"
    echo "   Endpoint: http://minio.${MINIO_NAMESPACE}:9000"
}

# Create ClickHouse installation with backup sidecar
create_test_chi() {
    echo "🏗️  Creating test ClickHouse installation for native backup testing..."
    
    cat <<EOF | kubectl apply -n $NAMESPACE -f -
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseInstallation
metadata:
  name: $CHI_NAME
spec:
  defaults:
    templates:
      podTemplate: clickhouse-pod
      volumeClaimTemplate: storage-template
  templates:
    podTemplates:
      - name: clickhouse-pod
        spec:
          containers:
            - name: clickhouse-pod
              image: clickhouse/clickhouse-server:23.8
              ports:
              - containerPort: 8123
              - containerPort: 9000
              volumeMounts:
              - name: storage-template
                mountPath: /var/lib/clickhouse

    volumeClaimTemplates:
      - name: storage-template
        spec:
          accessModes:
          - ReadWriteOnce
          resources:
            requests:
              storage: 2Gi
          storageClassName: standard

  configuration:
    clusters:
      - name: test-cluster
        layout:
          shardsCount: 1
          replicasCount: 1
EOF

    echo "⏳ Waiting for ClickHouse installation to be ready..."
    kubectl wait --for=condition=Ready pod -l clickhouse.altinity.com/chi=$CHI_NAME -n $NAMESPACE --timeout=600s
    
    echo "✅ ClickHouse installation ready"
}

# Create test data
create_test_data() {
    echo "📊 Creating test data..."
    
    POD_NAME=$(kubectl get pods -n $NAMESPACE -l clickhouse.altinity.com/chi=$CHI_NAME -o jsonpath='{.items[0].metadata.name}')
    
    kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-pod -- clickhouse-client --query="
    CREATE DATABASE IF NOT EXISTS test_backup;
    
    CREATE TABLE IF NOT EXISTS test_backup.users (
        user_id UInt32,
        username String,
        email String,
        created_at DateTime DEFAULT now()
    ) ENGINE = MergeTree()
    ORDER BY user_id;
    
    CREATE TABLE IF NOT EXISTS test_backup.events (
        event_id UInt64,
        user_id UInt32,
        event_type String,
        timestamp DateTime DEFAULT now(),
        properties String
    ) ENGINE = MergeTree()
    ORDER BY (timestamp, event_id);
    
    INSERT INTO test_backup.users 
    SELECT 
        number as user_id,
        concat('user_', toString(number)) as username,
        concat('user_', toString(number), '@example.com') as email,
        now() - number * 86400 as created_at
    FROM numbers(1000);
    
    INSERT INTO test_backup.events 
    SELECT 
        number as event_id,
        (number % 1000) + 1 as user_id,
        ['login', 'logout', 'click', 'purchase', 'view'][number % 5 + 1] as event_type,
        now() - number * 60 as timestamp,
        concat('{\"page\": \"page_', toString(number % 100), '\"}') as properties
    FROM numbers(10000);
    "
    
    echo "✅ Test data created:"
    kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-pod -- clickhouse-client --query="
    SELECT 'Users table: ' || toString(count()) as info FROM test_backup.users
    UNION ALL
    SELECT 'Events table: ' || toString(count()) as info FROM test_backup.events;
    "
}

# Verify native backup functionality
verify_native_backup() {
    echo "🔍 Verifying native backup functionality..."
    
    POD_NAME=$(kubectl get pods -n $NAMESPACE -l clickhouse.altinity.com/chi=$CHI_NAME -o jsonpath='{.items[0].metadata.name}')
    
    # Test ClickHouse connectivity
    if kubectl exec -n $NAMESPACE $POD_NAME -c clickhouse-pod -- clickhouse-client --query="SELECT 1" >/dev/null 2>&1; then
        echo "✅ ClickHouse is accessible"
    else
        echo "❌ ClickHouse is not accessible"
        return 1
    fi
    
    # Check if operator recognizes ClickHouseBackup CRD
    if kubectl get crd clickhousebackups.clickhouse.altinity.com >/dev/null 2>&1; then
        echo "✅ ClickHouseBackup CRD is installed"
    else
        echo "❌ ClickHouseBackup CRD not found"
        return 1
    fi
    
    echo "✅ Native backup functionality ready"
}

# Create sample backup resources
create_sample_backups() {
    echo "📝 Creating sample backup resources for native testing..."
    
    # Simple backup without external storage
    cat <<EOF | kubectl apply -n $NAMESPACE -f -
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseBackup
metadata:
  name: test-simple-backup
spec:
  clickHouseInstallation:
    name: $CHI_NAME
    namespace: $NAMESPACE
  type: full
  tables:
    - "test_backup.*"
  includeSchema: true
  compression:
    type: gzip
    level: 6
EOF

    # Scheduled backup (every 5 minutes for testing)
    cat <<EOF | kubectl apply -n $NAMESPACE -f -
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseBackup
metadata:
  name: test-scheduled-backup
spec:
  clickHouseInstallation:
    name: $CHI_NAME
    namespace: $NAMESPACE
  type: full
  schedule: "*/5 * * * *"  # Every 5 minutes
  backupPolicy:
    retentionPolicy:
      keepLocal: 3
  tables:
    - "test_backup.users"
EOF

    echo "✅ Sample backup resources created"
}

# Main setup flow
main() {
    echo "🎯 ClickHouseBackup Test Environment Setup"
    echo "Namespace: $NAMESPACE"
    echo "CHI Name: $CHI_NAME"
    echo ""
    
    # Check if MinIO should be installed
    if [[ "${SETUP_MINIO:-true}" == "true" ]]; then
        setup_minio
    fi
    
    create_test_chi
    create_test_data
    verify_native_backup
    create_sample_backups
    
    echo ""
    echo "🎉 Test environment setup complete!"
    echo ""
    echo "📋 Quick Start Commands:"
    echo "  # Check backup resources:"
    echo "  kubectl get chb -n $NAMESPACE"
    echo ""
    echo "  # Monitor backup execution:"
    echo "  kubectl get chb test-simple-backup -n $NAMESPACE -w"
    echo ""
    echo "  # Execute manual backup:"
    echo "  kubectl apply -f - <<EOF"
    echo "apiVersion: clickhouse.altinity.com/v1"
    echo "kind: ClickHouseBackup"
    echo "metadata:"
    echo "  name: manual-test-backup"
    echo "  namespace: $NAMESPACE"
    echo "spec:"
    echo "  clickHouseInstallation:"
    echo "    name: $CHI_NAME"
    echo "    namespace: $NAMESPACE"
    echo "  type: full"
    echo "EOF"
    echo ""
    echo "  # Check backup status:"
    echo "  kubectl describe chb manual-test-backup -n $NAMESPACE"
    echo ""
    echo "  # View backup API directly:"
    echo "  POD=\$(kubectl get pods -n $NAMESPACE -l clickhouse.altinity.com/chi=$CHI_NAME -o jsonpath='{.items[0].metadata.name}')"
    echo "  kubectl exec -n $NAMESPACE \$POD -c clickhouse-backup -- curl -s http://127.0.0.1:7171/backup/list"
    echo ""
    echo "🧹 Cleanup:"
    echo "  kubectl delete namespace $NAMESPACE"
    echo "  kubectl delete namespace $MINIO_NAMESPACE"
}

# Handle command line arguments
case "${1:-}" in
    --minio-only)
        setup_minio
        ;;
    --no-minio)
        SETUP_MINIO=false
        main
        ;;
    --help)
        echo "Usage: $0 [--minio-only|--no-minio|--help]"
        echo "  --minio-only  Only setup MinIO"
        echo "  --no-minio    Skip MinIO setup"
        echo "  --help        Show this help"
        ;;
    *)
        main
        ;;
esac