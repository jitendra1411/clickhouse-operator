#!/bin/bash

# ClickHouseBackup Test Runner
# Comprehensive test suite for backup functionality

set -e

NAMESPACE=${NAMESPACE:-"clickhouse-backup-test"}
CHI_NAME=${CHI_NAME:-"test-backup-chi"}
TIMEOUT=${TIMEOUT:-300}
VERBOSE=${VERBOSE:-false}

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Test results tracking
TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0
SKIPPED_TESTS=0

# Logging functions
log() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

success() {
    echo -e "${GREEN}[PASS]${NC} $1"
    ((PASSED_TESTS++))
}

fail() {
    echo -e "${RED}[FAIL]${NC} $1"
    ((FAILED_TESTS++))
}

warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

skip() {
    echo -e "${YELLOW}[SKIP]${NC} $1"
    ((SKIPPED_TESTS++))
}

# Test helper functions
wait_for_backup_completion() {
    local backup_name=$1
    local timeout=${2:-$TIMEOUT}
    local namespace=${3:-$NAMESPACE}
    
    log "Waiting for backup $backup_name to complete (timeout: ${timeout}s)..."
    
    local end_time=$((SECONDS + timeout))
    while [ $SECONDS -lt $end_time ]; do
        local status=$(kubectl get chb $backup_name -n $namespace -o jsonpath='{.status.status}' 2>/dev/null || echo "NotFound")
        
        case $status in
            "Completed")
                success "Backup $backup_name completed successfully"
                return 0
                ;;
            "Failed")
                local error=$(kubectl get chb $backup_name -n $namespace -o jsonpath='{.status.error}' 2>/dev/null || echo "Unknown error")
                fail "Backup $backup_name failed: $error"
                return 1
                ;;
            "Running"|"Pending")
                if [[ "$VERBOSE" == "true" ]]; then
                    local phase=$(kubectl get chb $backup_name -n $namespace -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
                    log "Backup $backup_name status: $status, phase: $phase"
                fi
                sleep 5
                ;;
            "NotFound")
                fail "Backup $backup_name not found"
                return 1
                ;;
            *)
                log "Backup $backup_name status: $status (waiting...)"
                sleep 5
                ;;
        esac
    done
    
    fail "Timeout waiting for backup $backup_name to complete"
    return 1
}

verify_backup_exists() {
    local backup_name=$1
    local namespace=${2:-$NAMESPACE}
    
    log "Verifying backup $backup_name exists in storage..."
    
    local pod_name=$(kubectl get pods -n $namespace -l clickhouse.altinity.com/chi=$CHI_NAME -o jsonpath='{.items[0].metadata.name}')
    local backup_list=$(kubectl exec -n $namespace $pod_name -c clickhouse-backup -- curl -s http://127.0.0.1:7171/backup/list 2>/dev/null || echo "{}")
    
    if echo "$backup_list" | jq -e ".local[] | select(.name == \"$backup_name\")" >/dev/null 2>&1; then
        success "Backup $backup_name found in local storage"
        return 0
    else
        fail "Backup $backup_name not found in storage"
        if [[ "$VERBOSE" == "true" ]]; then
            echo "Available backups:"
            echo "$backup_list" | jq -r '.local[].name // "No backups found"' 2>/dev/null || echo "Failed to parse backup list"
        fi
        return 1
    fi
}

cleanup_backup() {
    local backup_name=$1
    local namespace=${2:-$NAMESPACE}
    
    log "Cleaning up backup resource: $backup_name"
    kubectl delete chb $backup_name -n $namespace --ignore-not-found=true >/dev/null 2>&1 || true
}

# Test functions
test_basic_backup() {
    ((TOTAL_TESTS++))
    log "🧪 Testing basic backup functionality..."
    
    local backup_name="test-basic-$(date +%s)"
    
    # Create backup resource
    cat <<EOF | kubectl apply -n $NAMESPACE -f - >/dev/null
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseBackup
metadata:
  name: $backup_name
spec:
  clickHouseInstallation:
    name: $CHI_NAME
    namespace: $NAMESPACE
  type: full
  tables:
    - "test_backup.*"
  includeSchema: true
EOF

    if wait_for_backup_completion $backup_name; then
        verify_backup_exists $backup_name
    fi
    
    cleanup_backup $backup_name
}

test_scheduled_backup() {
    ((TOTAL_TESTS++))
    log "🧪 Testing scheduled backup functionality..."
    
    local backup_name="test-scheduled-$(date +%s)"
    
    # Create scheduled backup (every minute for testing)
    cat <<EOF | kubectl apply -n $NAMESPACE -f - >/dev/null
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseBackup
metadata:
  name: $backup_name
spec:
  clickHouseInstallation:
    name: $CHI_NAME
    namespace: $NAMESPACE
  type: full
  schedule: "* * * * *"  # Every minute
  backupPolicy:
    retentionPolicy:
      keepLocal: 2
  tables:
    - "test_backup.users"
EOF

    log "Waiting for scheduled backup to execute..."
    sleep 65  # Wait just over a minute for the first execution
    
    # Check if backup was scheduled and executed
    local next_backup=$(kubectl get chb $backup_name -n $NAMESPACE -o jsonpath='{.status.nextScheduledBackup}' 2>/dev/null || echo "")
    local last_success=$(kubectl get chb $backup_name -n $NAMESPACE -o jsonpath='{.status.lastSuccessfulBackup}' 2>/dev/null || echo "")
    
    if [[ -n "$next_backup" ]]; then
        success "Scheduled backup has next execution time set: $next_backup"
        if [[ -n "$last_success" ]]; then
            success "Scheduled backup executed successfully at: $last_success"
        else
            warn "Scheduled backup scheduled but not yet executed"
        fi
    else
        fail "Scheduled backup not properly scheduled"
    fi
    
    cleanup_backup $backup_name
}

test_watch_mode() {
    ((TOTAL_TESTS++))
    log "🧪 Testing watch mode functionality..."
    
    local backup_name="test-watch-$(date +%s)"
    
    # Create watch mode backup with short intervals
    cat <<EOF | kubectl apply -n $NAMESPACE -f - >/dev/null
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseBackup
metadata:
  name: $backup_name
spec:
  clickHouseInstallation:
    name: $CHI_NAME
    namespace: $NAMESPACE
  type: incremental
  backupPolicy:
    watchMode:
      enabled: true
      watchInterval: "2m"
      fullInterval: "10m"
      backupNameTemplate: "watch-{type}-{time:20060102150405}"
    retentionPolicy:
      keepLocal: 5
  tables:
    - "test_backup.events"
EOF

    log "Waiting for watch mode to initialize and create first backup..."
    sleep 30
    
    # Check if watch mode is active
    local status=$(kubectl get chb $backup_name -n $NAMESPACE -o jsonpath='{.status.status}' 2>/dev/null || echo "NotFound")
    if [[ "$status" == "Running" ]] || [[ "$status" == "Completed" ]]; then
        success "Watch mode backup is active with status: $status"
    else
        fail "Watch mode backup failed to activate, status: $status"
    fi
    
    cleanup_backup $backup_name
}

test_incremental_backup() {
    ((TOTAL_TESTS++))
    log "🧪 Testing incremental backup functionality..."
    
    local full_backup_name="test-full-$(date +%s)"
    local incremental_backup_name="test-incremental-$(date +%s)"
    
    # Create full backup first
    cat <<EOF | kubectl apply -n $NAMESPACE -f - >/dev/null
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseBackup
metadata:
  name: $full_backup_name
spec:
  clickHouseInstallation:
    name: $CHI_NAME
    namespace: $NAMESPACE
  type: full
  tables:
    - "test_backup.*"
EOF

    if wait_for_backup_completion $full_backup_name 180; then
        # Get the actual backup name for diff-from
        local actual_backup_name=$(kubectl get chb $full_backup_name -n $NAMESPACE -o jsonpath='{.status.backupName}' 2>/dev/null || echo "")
        
        if [[ -n "$actual_backup_name" ]]; then
            # Create incremental backup
            cat <<EOF | kubectl apply -n $NAMESPACE -f - >/dev/null
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseBackup
metadata:
  name: $incremental_backup_name
spec:
  clickHouseInstallation:
    name: $CHI_NAME
    namespace: $NAMESPACE
  type: incremental
  diffFrom: "$actual_backup_name"
  tables:
    - "test_backup.*"
EOF

            if wait_for_backup_completion $incremental_backup_name 180; then
                success "Incremental backup completed successfully"
            fi
        else
            fail "Could not determine full backup name for incremental backup"
        fi
    fi
    
    cleanup_backup $full_backup_name
    cleanup_backup $incremental_backup_name
}

test_storage_backends() {
    ((TOTAL_TESTS++))
    log "🧪 Testing different storage backends..."
    
    # Test S3 (MinIO) storage
    local s3_backup_name="test-s3-$(date +%s)"
    cat <<EOF | kubectl apply -n $NAMESPACE -f - >/dev/null
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseBackup
metadata:
  name: $s3_backup_name
spec:
  clickHouseInstallation:
    name: $CHI_NAME
    namespace: $NAMESPACE
  type: full
  storage:
    type: s3
    s3:
      endpoint: "http://minio.minio-test:9000"
      bucket: "clickhouse-backups"
      path: "test-s3"
      accessKey: "testuser"
      secretKey: "testpass123"
      forcePathStyle: true
      disableSSL: true
  tables:
    - "test_backup.users"
EOF

    if wait_for_backup_completion $s3_backup_name 180; then
        success "S3 storage backend test passed"
    fi
    
    cleanup_backup $s3_backup_name
}

test_backup_validation() {
    ((TOTAL_TESTS++))
    log "🧪 Testing backup validation and error handling..."
    
    # Test backup with invalid CHI reference
    local invalid_backup_name="test-invalid-$(date +%s)"
    cat <<EOF | kubectl apply -n $NAMESPACE -f - >/dev/null
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseBackup
metadata:
  name: $invalid_backup_name
spec:
  clickHouseInstallation:
    name: "non-existent-chi"
    namespace: $NAMESPACE
  type: full
EOF

    sleep 10
    local status=$(kubectl get chb $invalid_backup_name -n $NAMESPACE -o jsonpath='{.status.status}' 2>/dev/null || echo "NotFound")
    local error=$(kubectl get chb $invalid_backup_name -n $NAMESPACE -o jsonpath='{.status.error}' 2>/dev/null || echo "")
    
    if [[ "$status" == "Failed" ]] && [[ "$error" == *"not found"* ]]; then
        success "Invalid CHI reference properly detected and handled"
    else
        fail "Invalid CHI reference not properly handled. Status: $status, Error: $error"
    fi
    
    cleanup_backup $invalid_backup_name
}

test_backup_status_tracking() {
    ((TOTAL_TESTS++))
    log "🧪 Testing backup status tracking..."
    
    local backup_name="test-status-$(date +%s)"
    
    cat <<EOF | kubectl apply -n $NAMESPACE -f - >/dev/null
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseBackup
metadata:
  name: $backup_name
spec:
  clickHouseInstallation:
    name: $CHI_NAME
    namespace: $NAMESPACE
  type: full
  tables:
    - "test_backup.users"
EOF

    sleep 5
    
    # Check status fields
    local status=$(kubectl get chb $backup_name -n $NAMESPACE -o jsonpath='{.status.status}' 2>/dev/null || echo "")
    local phase=$(kubectl get chb $backup_name -n $NAMESPACE -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    local start_time=$(kubectl get chb $backup_name -n $NAMESPACE -o jsonpath='{.status.startTime}' 2>/dev/null || echo "")
    
    if [[ -n "$status" ]] && [[ -n "$phase" ]] && [[ -n "$start_time" ]]; then
        success "Backup status tracking is working (Status: $status, Phase: $phase)"
    else
        fail "Backup status tracking not working properly"
    fi
    
    if wait_for_backup_completion $backup_name 180; then
        local completion_time=$(kubectl get chb $backup_name -n $NAMESPACE -o jsonpath='{.status.completionTime}' 2>/dev/null || echo "")
        local duration=$(kubectl get chb $backup_name -n $NAMESPACE -o jsonpath='{.status.duration}' 2>/dev/null || echo "")
        
        if [[ -n "$completion_time" ]] && [[ -n "$duration" ]]; then
            success "Backup completion tracking is working"
        else
            warn "Backup completion tracking may have issues"
        fi
    fi
    
    cleanup_backup $backup_name
}

# Environment validation
validate_environment() {
    log "🔍 Validating test environment..."
    
    # Check if namespace exists
    if ! kubectl get namespace $NAMESPACE >/dev/null 2>&1; then
        fail "Test namespace $NAMESPACE not found. Run setup-test-environment.sh first."
        exit 1
    fi
    
    # Check if CHI exists
    if ! kubectl get chi $CHI_NAME -n $NAMESPACE >/dev/null 2>&1; then
        fail "Test CHI $CHI_NAME not found in namespace $NAMESPACE"
        exit 1
    fi
    
    # Check if backup CRD exists
    if ! kubectl get crd clickhousebackups.clickhouse.altinity.com >/dev/null 2>&1; then
        fail "ClickHouseBackup CRD not found. Install it first."
        exit 1
    fi
    
    # Check if pods are ready
    local pod_name=$(kubectl get pods -n $NAMESPACE -l clickhouse.altinity.com/chi=$CHI_NAME -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
    if [[ -z "$pod_name" ]]; then
        fail "No pods found for CHI $CHI_NAME"
        exit 1
    fi
    
    # Check backup API
    if ! kubectl exec -n $NAMESPACE $pod_name -c clickhouse-backup -- curl -s http://127.0.0.1:7171/ping >/dev/null 2>&1; then
        fail "Backup API not responding"
        exit 1
    fi
    
    success "Environment validation passed"
}

# Test suite runner
run_test_suite() {
    local test_type=${1:-"all"}
    
    log "🚀 Starting ClickHouseBackup test suite..."
    log "Test type: $test_type"
    log "Namespace: $NAMESPACE"
    log "CHI: $CHI_NAME"
    log "Timeout: ${TIMEOUT}s"
    echo ""
    
    validate_environment
    echo ""
    
    case $test_type in
        "basic"|"all")
            test_basic_backup
            ;;
    esac
    
    case $test_type in
        "scheduled"|"all")
            test_scheduled_backup
            ;;
    esac
    
    case $test_type in
        "watch"|"all")
            test_watch_mode
            ;;
    esac
    
    case $test_type in
        "incremental"|"all")
            test_incremental_backup
            ;;
    esac
    
    case $test_type in
        "storage"|"all")
            test_storage_backends
            ;;
    esac
    
    case $test_type in
        "validation"|"all")
            test_backup_validation
            ;;
    esac
    
    case $test_type in
        "status"|"all")
            test_backup_status_tracking
            ;;
    esac
}

# Results summary
print_summary() {
    echo ""
    echo "📊 Test Results Summary"
    echo "======================="
    echo "Total Tests:  $TOTAL_TESTS"
    echo -e "Passed:       ${GREEN}$PASSED_TESTS${NC}"
    echo -e "Failed:       ${RED}$FAILED_TESTS${NC}"
    echo -e "Skipped:      ${YELLOW}$SKIPPED_TESTS${NC}"
    echo ""
    
    if [[ $FAILED_TESTS -eq 0 ]]; then
        echo -e "${GREEN}🎉 All tests passed!${NC}"
        exit 0
    else
        echo -e "${RED}❌ Some tests failed.${NC}"
        exit 1
    fi
}

# Help function
show_help() {
    echo "ClickHouseBackup Test Runner"
    echo ""
    echo "Usage: $0 [OPTIONS] [TEST_TYPE]"
    echo ""
    echo "Test Types:"
    echo "  all          Run all tests (default)"
    echo "  basic        Basic backup functionality"
    echo "  scheduled    Scheduled backup tests"
    echo "  watch        Watch mode tests"
    echo "  incremental  Incremental backup tests"
    echo "  storage      Storage backend tests"
    echo "  validation   Validation and error handling"
    echo "  status       Status tracking tests"
    echo ""
    echo "Options:"
    echo "  --namespace NAME    Test namespace (default: $NAMESPACE)"
    echo "  --chi NAME          CHI name (default: $CHI_NAME)"
    echo "  --timeout SECONDS   Test timeout (default: $TIMEOUT)"
    echo "  --verbose           Verbose output"
    echo "  --help              Show this help"
    echo ""
    echo "Environment Variables:"
    echo "  NAMESPACE           Test namespace"
    echo "  CHI_NAME            CHI name"
    echo "  TIMEOUT             Test timeout in seconds"
    echo "  VERBOSE             Enable verbose output (true/false)"
    echo ""
    echo "Examples:"
    echo "  $0                  # Run all tests"
    echo "  $0 basic            # Run only basic tests"
    echo "  $0 --verbose all    # Run all tests with verbose output"
    echo "  TIMEOUT=600 $0      # Run with extended timeout"
}

# Main script
main() {
    local test_type="all"
    
    # Parse command line arguments
    while [[ $# -gt 0 ]]; do
        case $1 in
            --namespace)
                NAMESPACE="$2"
                shift 2
                ;;
            --chi)
                CHI_NAME="$2"
                shift 2
                ;;
            --timeout)
                TIMEOUT="$2"
                shift 2
                ;;
            --verbose)
                VERBOSE=true
                shift
                ;;
            --help)
                show_help
                exit 0
                ;;
            basic|scheduled|watch|incremental|storage|validation|status|all)
                test_type="$1"
                shift
                ;;
            *)
                echo "Unknown option: $1"
                show_help
                exit 1
                ;;
        esac
    done
    
    run_test_suite $test_type
    print_summary
}

main "$@"