# ClickHouseBackup Testing Guide

This guide walks you through testing the new native ClickHouseBackup functionality in the ClickHouse operator.

## 🎯 Prerequisites

1. **Kubernetes Cluster** (v1.20+)
2. **kubectl** configured
3. **Go 1.19+** for building the operator
4. **Docker** for building container images
5. **MinIO or S3** for storage testing (optional but recommended)

## 🚀 Quick Start Testing

### Step 1: Build Enhanced Operator

```bash
# Clone the repository (if not already done)
cd clickhouse-operator

# Generate the deepcopy code for new backup types
make generate

# Build the operator with backup functionality
make docker-build OPERATOR_IMAGE=my-registry/clickhouse-operator:backup-test
make docker-push OPERATOR_IMAGE=my-registry/clickhouse-operator:backup-test
```

### Step 2: Deploy Enhanced Operator

```bash
# Update the operator image in deployment
kubectl patch deployment clickhouse-operator \
  -n kube-system \
  --type='merge' \
  -p='{"spec":{"template":{"spec":{"containers":[{"name":"clickhouse-operator","image":"my-registry/clickhouse-operator:backup-test"}]}}}}'

# Wait for operator to restart
kubectl rollout status deployment/clickhouse-operator -n kube-system
```

### Step 3: Install CRDs

```bash
# Apply the new backup CRD
kubectl apply -f deploy/operator/parts/crd-backup.yaml

# Verify CRD installation
kubectl get crd clickhousebackups.clickhouse.altinity.com
```

### Step 4: Set Up Test Environment

```bash
# Run the test setup script
./tests/backup/setup-test-environment.sh

# Or manually follow the steps below...
```

## 🧪 Test Scenarios

### 1. Basic Backup Test

```bash
# Create a test CHI with backup sidecar
kubectl apply -f tests/backup/test-chi-with-backup.yaml

# Wait for CHI to be ready
kubectl wait --for=condition=Ready pod -l clickhouse.altinity.com/chi=test-backup-chi --timeout=300s

# Create a simple backup
kubectl apply -f tests/backup/test-backup-simple.yaml

# Monitor backup progress
kubectl get chb test-simple-backup -w
```

### 2. Scheduled Backup Test

```bash
# Create a scheduled backup (runs every 2 minutes for testing)
kubectl apply -f tests/backup/test-backup-scheduled.yaml

# Monitor for multiple executions
watch 'kubectl get chb test-scheduled-backup -o custom-columns="NAME:.metadata.name,STATUS:.status.status,LAST-SUCCESS:.status.lastSuccessfulBackup,NEXT:.status.nextScheduledBackup"'
```

### 3. Watch Mode Test

```bash
# Create a watch mode backup with frequent intervals
kubectl apply -f tests/backup/test-backup-watch.yaml

# Monitor continuous backup execution
kubectl logs -f deployment/clickhouse-operator -n kube-system | grep -i backup
```

### 4. Incremental Backup Test

```bash
# Create full backup first
kubectl apply -f tests/backup/test-backup-full.yaml
kubectl wait --for=condition=Complete chb/test-full-backup --timeout=300s

# Create incremental backup based on full backup
kubectl apply -f tests/backup/test-backup-incremental.yaml
```

### 5. Storage Backend Tests

```bash
# Test S3 storage
kubectl apply -f tests/backup/test-backup-s3.yaml

# Test local storage
kubectl apply -f tests/backup/test-backup-local.yaml

# Test with MinIO (if available)
kubectl apply -f tests/backup/test-backup-minio.yaml
```

## 📊 Monitoring and Validation

### Check Backup Status

```bash
# List all backups
kubectl get chb

# Detailed backup information
kubectl describe chb <backup-name>

# Backup history
kubectl get chb <backup-name> -o jsonpath='{.status.history[*]}' | jq
```

### Validate Backup Execution

```bash
# Check if backup files exist (for pod with backup sidecar)
POD_NAME=$(kubectl get pods -l clickhouse.altinity.com/chi=test-backup-chi -o jsonpath='{.items[0].metadata.name}')

# List local backups
kubectl exec $POD_NAME -c clickhouse-backup -- curl -s http://127.0.0.1:7171/backup/list

# Check backup details
kubectl exec $POD_NAME -c clickhouse-backup -- curl -s http://127.0.0.1:7171/backup/list | jq
```

### Monitor Metrics

```bash
# Check backup metrics (if Prometheus is available)
kubectl port-forward $POD_NAME 7171:7171
curl http://localhost:7171/metrics | grep clickhouse_backup
```

## 🔧 Test Validation Scripts

Run the automated validation:

```bash
# Run comprehensive test suite
./tests/backup/run-tests.sh

# Run specific test categories
./tests/backup/run-tests.sh --basic
./tests/backup/run-tests.sh --scheduled
./tests/backup/run-tests.sh --watch
./tests/backup/run-tests.sh --storage
```

## 🐛 Troubleshooting

### Common Issues

1. **CRD Not Found**
   ```bash
   kubectl get crd | grep backup
   # If missing, apply: kubectl apply -f deploy/operator/parts/crd-backup.yaml
   ```

2. **Operator Not Recognizing Backup Resources**
   ```bash
   kubectl logs deployment/clickhouse-operator -n kube-system | grep -i backup
   # Check if backup controller is registered
   ```

3. **Backup Pod Not Found**
   ```bash
   # Verify CHI has backup sidecar
   kubectl get pods -l clickhouse.altinity.com/chi=<chi-name> -o yaml | grep clickhouse-backup
   ```

4. **API Connection Issues**
   ```bash
   # Test backup API directly
   kubectl exec <pod-name> -c clickhouse-backup -- curl -s http://127.0.0.1:7171/ping
   ```

### Debug Commands

```bash
# Enable verbose logging in operator
kubectl set env deployment/clickhouse-operator -n kube-system GLOG_v=2

# Check operator logs for backup events
kubectl logs deployment/clickhouse-operator -n kube-system --tail=100 | grep -i backup

# Check backup controller events
kubectl get events --field-selector involvedObject.kind=ClickHouseBackup

# Inspect backup resource status
kubectl get chb <backup-name> -o yaml
```

## 🎯 Expected Results

### Successful Test Indicators

1. **CRD Installation**: `kubectl get crd clickhousebackups.clickhouse.altinity.com` shows the CRD
2. **Operator Recognition**: Operator logs show backup controller initialization
3. **Backup Creation**: `kubectl get chb` lists backup resources
4. **Status Updates**: Backup status progresses through phases (Pending → Running → Completed)
5. **File Creation**: Actual backup files appear in local/remote storage
6. **Scheduling**: Scheduled backups execute at expected times
7. **Watch Mode**: Continuous backup sequences run automatically

### Performance Expectations

- **Simple Backup**: 30-120 seconds for small test data
- **Scheduled Backup**: Executes within 30 seconds of scheduled time
- **Watch Mode**: Initiates incremental backups at configured intervals
- **Storage Upload**: Completes without timeout errors

## 📈 Test Data Generation

To create meaningful test data for backup validation:

```bash
# Generate test data in ClickHouse
kubectl exec $POD_NAME -c clickhouse-pod -- clickhouse-client --query="
CREATE DATABASE IF NOT EXISTS test_backup;
CREATE TABLE test_backup.events (
    id UInt64,
    timestamp DateTime,
    message String,
    user_id UInt32
) ENGINE = MergeTree()
ORDER BY (timestamp, id);

INSERT INTO test_backup.events 
SELECT 
    number as id,
    now() - number as timestamp,
    concat('Message ', toString(number)) as message,
    number % 1000 as user_id
FROM numbers(100000);
"

# Verify data
kubectl exec $POD_NAME -c clickhouse-pod -- clickhouse-client --query="SELECT count() FROM test_backup.events"
```

## 🔍 Cleanup

```bash
# Remove test resources
kubectl delete -f tests/backup/

# Remove test data
kubectl exec $POD_NAME -c clickhouse-pod -- clickhouse-client --query="DROP DATABASE IF EXISTS test_backup"

# Reset operator (optional)
kubectl rollout restart deployment/clickhouse-operator -n kube-system
```

## 📋 Test Checklist

- [ ] Operator builds and deploys successfully
- [ ] CRD installs without errors
- [ ] Simple backup creates and completes
- [ ] Backup files appear in storage
- [ ] Scheduled backup executes on time
- [ ] Watch mode runs continuous backups
- [ ] Incremental backup chains work
- [ ] Different storage backends function
- [ ] Status updates correctly
- [ ] Error handling works properly
- [ ] Cleanup completes successfully

For detailed test scenarios and automation, see the individual test files in this directory.