package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
)

// BackupExecutor executes backup operations by interfacing with clickhouse-backup
type BackupExecutor struct {
	client.Client
	restConfig *rest.Config
	clientset  *kubernetes.Clientset
}

// BackupAPIResponse represents responses from clickhouse-backup API
type BackupAPIResponse struct {
	Status      string            `json:"status"`
	Operation   string            `json:"operation"`
	BackupName  string            `json:"backup_name,omitempty"`
	LocalPath   string            `json:"local_path,omitempty"`
	RemotePath  string            `json:"remote_path,omitempty"`
	DataSize    int64             `json:"data_size,omitempty"`
	Duration    string            `json:"duration,omitempty"`
	Error       string            `json:"error,omitempty"`
	Message     string            `json:"message,omitempty"`
	Tables      []BackupTableInfo `json:"tables,omitempty"`
}

// BackupTableInfo represents table information in backup
type BackupTableInfo struct {
	Database string `json:"database"`
	Table    string `json:"table"`
	Size     int64  `json:"size"`
}

// BackupListResponse represents backup list response
type BackupListResponse struct {
	Local  []BackupInfo `json:"local"`
	Remote []BackupInfo `json:"remote"`
}

// BackupInfo represents backup information
type BackupInfo struct {
	Name           string    `json:"name"`
	Created        time.Time `json:"created"`
	Size           int64     `json:"size"`
	CompressedSize int64     `json:"compressed_size,omitempty"`
	DataSize       int64     `json:"data_size"`
	Tables         int       `json:"tables"`
	Location       string    `json:"location"`
	Desc           string    `json:"desc,omitempty"`
}

// NewBackupExecutor creates a new backup executor
func NewBackupExecutor(client client.Client, restConfig *rest.Config) (*BackupExecutor, error) {
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	return &BackupExecutor{
		Client:     client,
		restConfig: restConfig,
		clientset:  clientset,
	}, nil
}

// ExecuteBackup performs a backup operation
func (e *BackupExecutor) ExecuteBackup(ctx context.Context, backup *api.ClickHouseBackup) error {
	log := log.FromContext(ctx)
	
	// Find backup pod in the target CHI
	backupPod, err := e.findBackupPod(ctx, backup)
	if err != nil {
		return fmt.Errorf("failed to find backup pod: %w", err)
	}
	
	log.Info("Found backup pod", "pod", backupPod.Name, "namespace", backupPod.Namespace)
	
	// Generate backup name if not specified
	backupName := e.generateBackupName(backup)
	backup.Status.BackupName = backupName
	
	// Update status with backup initialization
	backup.Status.Phase = api.BackupPhaseCreating
	backup.Status.Message = fmt.Sprintf("Creating %s backup: %s", backup.GetBackupType(), backupName)
	if err := e.Status().Update(ctx, backup); err != nil {
		log.Error(err, "Failed to update backup status")
	}
	
	// Execute backup creation
	if err := e.createBackup(ctx, backupPod, backup, backupName); err != nil {
		return fmt.Errorf("failed to create backup: %w", err)
	}
	
	// Update status after creation
	backup.Status.Phase = api.BackupPhaseUploading
	backup.Status.Message = fmt.Sprintf("Uploading backup: %s", backupName)
	if err := e.Status().Update(ctx, backup); err != nil {
		log.Error(err, "Failed to update backup status")
	}
	
	// Upload to remote storage if configured
	if backup.Spec.Storage != nil {
		if err := e.uploadBackup(ctx, backupPod, backup, backupName); err != nil {
			return fmt.Errorf("failed to upload backup: %w", err)
		}
	}
	
	// Get backup information and update status
	if err := e.updateBackupInfo(ctx, backupPod, backup, backupName); err != nil {
		log.Error(err, "Failed to update backup info")
		// Don't fail the backup for this error
	}
	
	log.Info("Backup execution completed successfully", "backup", backupName)
	return nil
}

// findBackupPod finds a pod with clickhouse-backup sidecar in the target CHI
func (e *BackupExecutor) findBackupPod(ctx context.Context, backup *api.ClickHouseBackup) (*core.Pod, error) {
	chiRef := backup.GetCHIRef()
	namespace := chiRef.Namespace
	if namespace == "" {
		namespace = backup.Namespace
	}
	
	// Get the ClickHouseInstallation
	chi := &api.ClickHouseInstallation{}
	if err := e.Get(ctx, client.ObjectKey{
		Namespace: namespace,
		Name:      chiRef.Name,
	}, chi); err != nil {
		return nil, fmt.Errorf("failed to get ClickHouseInstallation: %w", err)
	}
	
	// List pods with CHI labels
	pods := &core.PodList{}
	if err := e.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{
		"clickhouse.altinity.com/app": "chop",
		"clickhouse.altinity.com/chi": chi.Name,
	}); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	
	// Find pod with clickhouse-backup container
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			if container.Name == "clickhouse-backup" {
				// Check if pod is ready
				if e.isPodReady(&pod) {
					return &pod, nil
				}
			}
		}
	}
	
	return nil, fmt.Errorf("no ready pod found with clickhouse-backup container")
}

// isPodReady checks if a pod is ready
func (e *BackupExecutor) isPodReady(pod *core.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == core.PodReady && condition.Status == core.ConditionTrue {
			return true
		}
	}
	return false
}

// generateBackupName generates a backup name based on the template
func (e *BackupExecutor) generateBackupName(backup *api.ClickHouseBackup) string {
	template := "backup"
	
	// Use template from watch mode if available
	if backup.IsWatchMode() && backup.Spec.BackupPolicy.WatchMode.BackupNameTemplate != nil {
		template = backup.Spec.BackupPolicy.WatchMode.BackupNameTemplate.Value()
	}
	
	// Replace template variables
	name := strings.ReplaceAll(template, "{type}", string(backup.GetBackupType()))
	name = strings.ReplaceAll(name, "{time:20060102150405}", time.Now().Format("20060102150405"))
	name = strings.ReplaceAll(name, "{chi}", backup.GetCHIRef().Name)
	
	// If no template variables were found, generate a default name
	if name == template {
		name = fmt.Sprintf("%s-%s-%s", 
			backup.GetCHIRef().Name,
			string(backup.GetBackupType()),
			time.Now().Format("20060102150405"))
	}
	
	return name
}

// createBackup creates a backup using clickhouse-backup API
func (e *BackupExecutor) createBackup(ctx context.Context, pod *core.Pod, backup *api.ClickHouseBackup, backupName string) error {
	log := log.FromContext(ctx)
	
	// Build backup creation parameters
	params := make(map[string]string)
	params["name"] = backupName
	
	// Add table filters if specified
	if len(backup.Spec.Tables) > 0 {
		params["tables"] = strings.Join(backup.Spec.Tables, ",")
	}
	
	// Add partitions if specified
	if len(backup.Spec.Partitions) > 0 {
		params["partitions"] = strings.Join(backup.Spec.Partitions, ",")
	}
	
	// Add backup type specific parameters
	switch backup.GetBackupType() {
	case api.BackupTypeSchema:
		params["schema"] = "true"
	case api.BackupTypeRBAC:
		params["rbac"] = "true"
	case api.BackupTypeConfigs:
		params["configs"] = "true"
	case api.BackupTypeIncremental:
		if backup.Spec.DiffFrom != nil && backup.Spec.DiffFrom.Value() != "" {
			params["diff-from"] = backup.Spec.DiffFrom.Value()
		}
	}
	
	// Add compression if specified
	if backup.Spec.Compression != nil {
		params["compression"] = string(backup.Spec.Compression.Type)
		if backup.Spec.Compression.Level != nil {
			params["compression-level"] = strconv.Itoa(int(backup.Spec.Compression.Level.Value()))
		}
	}
	
	// Add resume if enabled
	if backup.Spec.Resume != nil && backup.Spec.Resume.Value() {
		params["resume"] = "true"
	}
	
	// Call clickhouse-backup API
	url := "http://127.0.0.1:7171/backup/create"
	response, err := e.callBackupAPI(ctx, pod, "POST", url, params, nil)
	if err != nil {
		return fmt.Errorf("backup creation API call failed: %w", err)
	}
	
	if response.Status != "success" {
		return fmt.Errorf("backup creation failed: %s", response.Error)
	}
	
	log.Info("Backup created successfully", "backup", backupName, "localPath", response.LocalPath)
	
	// Update backup status with creation info
	backup.Status.LocalPath = response.LocalPath
	if response.DataSize > 0 {
		backup.Status.LocalSizeBytes = &response.DataSize
	}
	
	return nil
}

// uploadBackup uploads a backup to remote storage
func (e *BackupExecutor) uploadBackup(ctx context.Context, pod *core.Pod, backup *api.ClickHouseBackup, backupName string) error {
	log := log.FromContext(ctx)
	
	// Skip upload if watch mode is enabled and deleteLocal is false
	if backup.IsWatchMode() && backup.Spec.BackupPolicy.WatchMode.DeleteLocal != nil && 
	   !backup.Spec.BackupPolicy.WatchMode.DeleteLocal.Value() {
		log.Info("Skipping upload in watch mode (deleteLocal=false)")
		return nil
	}
	
	url := fmt.Sprintf("http://127.0.0.1:7171/backup/upload/%s", backupName)
	response, err := e.callBackupAPI(ctx, pod, "POST", url, nil, nil)
	if err != nil {
		return fmt.Errorf("backup upload API call failed: %w", err)
	}
	
	if response.Status != "success" {
		return fmt.Errorf("backup upload failed: %s", response.Error)
	}
	
	log.Info("Backup uploaded successfully", "backup", backupName, "remotePath", response.RemotePath)
	
	// Update backup status with upload info
	backup.Status.RemotePath = response.RemotePath
	if response.DataSize > 0 {
		backup.Status.RemoteSizeBytes = &response.DataSize
	}
	
	return nil
}

// updateBackupInfo updates backup status with detailed information
func (e *BackupExecutor) updateBackupInfo(ctx context.Context, pod *core.Pod, backup *api.ClickHouseBackup, backupName string) error {
	// Get backup list to find our backup
	url := "http://127.0.0.1:7171/backup/list"
	response, err := e.callBackupAPIRaw(ctx, pod, "GET", url, nil, nil)
	if err != nil {
		return fmt.Errorf("failed to get backup list: %w", err)
	}
	
	var listResponse BackupListResponse
	if err := json.Unmarshal(response, &listResponse); err != nil {
		return fmt.Errorf("failed to parse backup list response: %w", err)
	}
	
	// Find our backup in the list
	for _, backupInfo := range listResponse.Local {
		if backupInfo.Name == backupName {
			backup.Status.LocalSizeBytes = &backupInfo.DataSize
			
			// Extract table information
			backup.Status.Tables = make([]string, 0)
			// Note: This would need more detailed API to get table info
			break
		}
	}
	
	for _, backupInfo := range listResponse.Remote {
		if backupInfo.Name == backupName {
			backup.Status.RemoteSizeBytes = &backupInfo.DataSize
			break
		}
	}
	
	return nil
}

// callBackupAPI calls the clickhouse-backup REST API
func (e *BackupExecutor) callBackupAPI(ctx context.Context, pod *core.Pod, method, url string, params map[string]string, body io.Reader) (*BackupAPIResponse, error) {
	responseBytes, err := e.callBackupAPIRaw(ctx, pod, method, url, params, body)
	if err != nil {
		return nil, err
	}
	
	var response BackupAPIResponse
	if err := json.Unmarshal(responseBytes, &response); err != nil {
		return nil, fmt.Errorf("failed to parse API response: %w", err)
	}
	
	return &response, nil
}

// callBackupAPIRaw calls the clickhouse-backup REST API and returns raw response
func (e *BackupExecutor) callBackupAPIRaw(ctx context.Context, pod *core.Pod, method, url string, params map[string]string, body io.Reader) ([]byte, error) {
	log := log.FromContext(ctx)
	
	// Build URL with parameters
	if len(params) > 0 {
		parts := make([]string, 0, len(params))
		for key, value := range params {
			parts = append(parts, fmt.Sprintf("%s=%s", key, value))
		}
		url = fmt.Sprintf("%s?%s", url, strings.Join(parts, "&"))
	}
	
	// Execute the API call via kubectl exec
	cmd := []string{
		"curl", "-s", "-X", method, url,
	}
	
	if body != nil {
		cmd = append(cmd, "-d", "@-")
	}
	
	log.Info("Executing backup API call", "pod", pod.Name, "method", method, "url", url)
	
	// Use kubectl exec to call the API
	result, err := e.execInPod(ctx, pod, "clickhouse-backup", cmd, body)
	if err != nil {
		return nil, fmt.Errorf("failed to execute API call: %w", err)
	}
	
	return result, nil
}

// execInPod executes a command in a pod container
func (e *BackupExecutor) execInPod(ctx context.Context, pod *core.Pod, container string, cmd []string, stdin io.Reader) ([]byte, error) {
	req := e.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec")
	
	req.VersionedParams(&core.PodExecOptions{
		Container: container,
		Command:   cmd,
		Stdin:     stdin != nil,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}, scheme.ParameterCodec)
	
	exec, err := remotecommand.NewSPDYExecutor(e.restConfig, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("failed to create executor: %w", err)
	}
	
	var stdout, stderr bytes.Buffer
	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})
	
	if err != nil {
		return nil, fmt.Errorf("command execution failed: %w, stderr: %s", err, stderr.String())
	}
	
	return stdout.Bytes(), nil
}