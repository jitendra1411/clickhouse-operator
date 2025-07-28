package backup

import (
	"context"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
)

// BackupController reconciles ClickHouseBackup objects
type BackupController struct {
	client.Client
	Scheme *runtime.Scheme
	
	scheduler      *cron.Cron
	scheduledJobs  map[string]cron.EntryID
	backupExecutor *BackupExecutor
}

// BackupExecutor handles the actual backup operations by interfacing with clickhouse-backup
type BackupExecutor struct {
	client.Client
}

// +kubebuilder:rbac:groups=clickhouse.altinity.com,resources=clickhousebackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clickhouse.altinity.com,resources=clickhousebackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=clickhouse.altinity.com,resources=clickhousebackups/finalizers,verbs=update
// +kubebuilder:rbac:groups=clickhouse.altinity.com,resources=clickhouseinstallations,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func NewBackupController(client client.Client, scheme *runtime.Scheme) *BackupController {
	return &BackupController{
		Client:         client,
		Scheme:         scheme,
		scheduler:      cron.New(cron.WithSeconds()),
		scheduledJobs:  make(map[string]cron.EntryID),
		backupExecutor: &BackupExecutor{Client: client},
	}
}

// Reconcile handles ClickHouseBackup resource changes
func (r *BackupController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	
	// Fetch the ClickHouseBackup instance
	backup := &api.ClickHouseBackup{}
	if err := r.Get(ctx, req.NamespacedName, backup); err != nil {
		if errors.IsNotFound(err) {
			// Backup was deleted, remove from scheduler
			r.removeFromScheduler(req.NamespacedName.String())
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get ClickHouseBackup")
		return ctrl.Result{}, err
	}

	// Handle finalizers
	backupFinalizer := "backup.clickhouse.altinity.com/finalizer"
	if backup.ObjectMeta.DeletionTimestamp.IsZero() {
		// Add finalizer if not present
		if !controllerutil.ContainsFinalizer(backup, backupFinalizer) {
			controllerutil.AddFinalizer(backup, backupFinalizer)
			return ctrl.Result{}, r.Update(ctx, backup)
		}
	} else {
		// Handle deletion
		if controllerutil.ContainsFinalizer(backup, backupFinalizer) {
			r.removeFromScheduler(req.NamespacedName.String())
			controllerutil.RemoveFinalizer(backup, backupFinalizer)
			return ctrl.Result{}, r.Update(ctx, backup)
		}
		return ctrl.Result{}, nil
	}

	// Ensure status is initialized
	if backup.Status == nil {
		backup.EnsureStatus()
		backup.Status.Status = api.BackupStatusPending
		return ctrl.Result{}, r.Status().Update(ctx, backup)
	}

	// Handle suspended backups
	if backup.IsSuspended() {
		r.removeFromScheduler(req.NamespacedName.String())
		if backup.Status.Status != api.BackupStatusSuspended {
			backup.Status.Status = api.BackupStatusSuspended
			backup.Status.Message = "Backup is suspended"
			return ctrl.Result{}, r.Status().Update(ctx, backup)
		}
		return ctrl.Result{}, nil
	}

	// Validate the target ClickHouseInstallation exists
	if err := r.validateCHIExists(ctx, backup); err != nil {
		log.Error(err, "Failed to validate ClickHouseInstallation")
		backup.Status.Status = api.BackupStatusFailed
		backup.Status.Error = err.Error()
		return ctrl.Result{}, r.Status().Update(ctx, backup)
	}

	// Handle scheduling
	if backup.IsScheduled() {
		if err := r.manageSchedule(ctx, backup); err != nil {
			log.Error(err, "Failed to manage backup schedule")
			return ctrl.Result{RequeueAfter: time.Minute * 5}, err
		}
	}

	// Handle watch mode
	if backup.IsWatchMode() {
		if err := r.manageWatchMode(ctx, backup); err != nil {
			log.Error(err, "Failed to manage watch mode")
			return ctrl.Result{RequeueAfter: time.Minute * 5}, err
		}
	}

	// Handle immediate backup execution (non-scheduled)
	if !backup.IsScheduled() && !backup.IsWatchMode() && backup.Status.Status == api.BackupStatusPending {
		return r.executeBackup(ctx, backup)
	}

	return ctrl.Result{RequeueAfter: time.Minute * 10}, nil
}

// validateCHIExists checks if the referenced ClickHouseInstallation exists
func (r *BackupController) validateCHIExists(ctx context.Context, backup *api.ClickHouseBackup) error {
	chiRef := backup.GetCHIRef()
	namespace := chiRef.Namespace
	if namespace == "" {
		namespace = backup.Namespace
	}

	chi := &api.ClickHouseInstallation{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: namespace,
		Name:      chiRef.Name,
	}, chi)
	
	if err != nil {
		return fmt.Errorf("ClickHouseInstallation %s/%s not found: %w", namespace, chiRef.Name, err)
	}

	return nil
}

// manageSchedule handles cron-based backup scheduling
func (r *BackupController) manageSchedule(ctx context.Context, backup *api.ClickHouseBackup) error {
	jobKey := fmt.Sprintf("%s/%s", backup.Namespace, backup.Name)
	
	// Remove existing job if it exists
	r.removeFromScheduler(jobKey)
	
	// Add new scheduled job
	schedule := backup.Spec.Schedule.Value()
	entryID, err := r.scheduler.AddFunc(schedule, func() {
		// Execute backup in a goroutine to avoid blocking the scheduler
		go func() {
			ctx := context.Background()
			log := log.FromContext(ctx)
			
			// Get fresh backup object
			currentBackup := &api.ClickHouseBackup{}
			if err := r.Get(ctx, client.ObjectKey{
				Namespace: backup.Namespace,
				Name:      backup.Name,
			}, currentBackup); err != nil {
				log.Error(err, "Failed to get backup for scheduled execution")
				return
			}
			
			// Skip if backup is suspended or already running
			if currentBackup.IsSuspended() || currentBackup.IsRunning() {
				return
			}
			
			// Execute the backup
			if _, err := r.executeBackup(ctx, currentBackup); err != nil {
				log.Error(err, "Scheduled backup execution failed")
			}
		}()
	})
	
	if err != nil {
		return fmt.Errorf("failed to schedule backup: %w", err)
	}
	
	r.scheduledJobs[jobKey] = entryID
	
	// Update next scheduled backup time
	next := r.scheduler.Entry(entryID).Next
	backup.Status.NextScheduledBackup = &meta.Time{Time: next}
	
	return nil
}

// manageWatchMode handles continuous backup watch mode
func (r *BackupController) manageWatchMode(ctx context.Context, backup *api.ClickHouseBackup) error {
	watchConfig := backup.Spec.BackupPolicy.WatchMode
	
	// Create watch-mode specific scheduling
	watchInterval := watchConfig.WatchInterval.Value()
	if watchInterval == "" {
		watchInterval = "1h" // Default to 1 hour
	}
	
	fullInterval := watchConfig.FullInterval.Value()
	if fullInterval == "" {
		fullInterval = "24h" // Default to 24 hours
	}
	
	// Schedule incremental backups
	incrementalJobKey := fmt.Sprintf("%s/%s-incremental", backup.Namespace, backup.Name)
	r.removeFromScheduler(incrementalJobKey)
	
	// Convert interval to cron expression (simplified - every N minutes/hours)
	cronExpr, err := r.intervalToCron(watchInterval)
	if err != nil {
		return fmt.Errorf("invalid watch interval: %w", err)
	}
	
	entryID, err := r.scheduler.AddFunc(cronExpr, func() {
		go r.executeWatchBackup(ctx, backup, api.BackupTypeIncremental)
	})
	
	if err != nil {
		return fmt.Errorf("failed to schedule watch incremental backup: %w", err)
	}
	
	r.scheduledJobs[incrementalJobKey] = entryID
	
	// Schedule full backups
	fullJobKey := fmt.Sprintf("%s/%s-full", backup.Namespace, backup.Name)
	r.removeFromScheduler(fullJobKey)
	
	fullCronExpr, err := r.intervalToCron(fullInterval)
	if err != nil {
		return fmt.Errorf("invalid full interval: %w", err)
	}
	
	fullEntryID, err := r.scheduler.AddFunc(fullCronExpr, func() {
		go r.executeWatchBackup(ctx, backup, api.BackupTypeFull)
	})
	
	if err != nil {
		return fmt.Errorf("failed to schedule watch full backup: %w", err)
	}
	
	r.scheduledJobs[fullJobKey] = fullEntryID
	
	return nil
}

// executeWatchBackup executes a backup in watch mode
func (r *BackupController) executeWatchBackup(ctx context.Context, backup *api.ClickHouseBackup, backupType api.BackupType) {
	log := log.FromContext(ctx)
	
	// Get fresh backup object
	currentBackup := &api.ClickHouseBackup{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: backup.Namespace,
		Name:      backup.Name,
	}, currentBackup); err != nil {
		log.Error(err, "Failed to get backup for watch execution")
		return
	}
	
	// Skip if backup is suspended or already running
	if currentBackup.IsSuspended() || currentBackup.IsRunning() {
		return
	}
	
	// Temporarily override backup type for this execution
	originalType := currentBackup.Spec.Type
	currentBackup.Spec.Type = backupType
	
	// Execute the backup
	if _, err := r.executeBackup(ctx, currentBackup); err != nil {
		log.Error(err, "Watch mode backup execution failed", "type", backupType)
	}
	
	// Restore original type
	currentBackup.Spec.Type = originalType
}

// executeBackup performs the actual backup operation
func (r *BackupController) executeBackup(ctx context.Context, backup *api.ClickHouseBackup) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	
	// Update status to running
	backup.Status.Status = api.BackupStatusRunning
	backup.Status.Phase = api.BackupPhaseInitializing
	backup.Status.StartTime = &meta.Time{Time: time.Now()}
	backup.Status.Message = "Starting backup execution"
	backup.Status.Error = ""
	
	if err := r.Status().Update(ctx, backup); err != nil {
		return ctrl.Result{}, err
	}
	
	log.Info("Starting backup execution", "backup", backup.Name, "type", backup.GetBackupType())
	
	// Execute backup using the BackupExecutor
	if err := r.backupExecutor.ExecuteBackup(ctx, backup); err != nil {
		log.Error(err, "Backup execution failed")
		
		backup.Status.Status = api.BackupStatusFailed
		backup.Status.Phase = api.BackupPhaseFailed
		backup.Status.Error = err.Error()
		backup.Status.CompletionTime = &meta.Time{Time: time.Now()}
		backup.Status.ConsecutiveFailures++
		
		// Add to history
		r.addToHistory(backup, api.BackupStatusFailed, err.Error())
		
		return ctrl.Result{RequeueAfter: time.Minute * 30}, r.Status().Update(ctx, backup)
	}
	
	// Update success status
	backup.Status.Status = api.BackupStatusCompleted
	backup.Status.Phase = api.BackupPhaseCompleted
	backup.Status.CompletionTime = &meta.Time{Time: time.Now()}
	backup.Status.LastSuccessfulBackup = backup.Status.CompletionTime
	backup.Status.ConsecutiveFailures = 0
	backup.Status.Message = "Backup completed successfully"
	
	// Calculate duration
	if backup.Status.StartTime != nil {
		duration := backup.Status.CompletionTime.Time.Sub(backup.Status.StartTime.Time)
		backup.Status.Duration = &meta.Duration{Duration: duration}
	}
	
	// Add to history
	r.addToHistory(backup, api.BackupStatusCompleted, "")
	
	log.Info("Backup completed successfully", "backup", backup.Name)
	
	return ctrl.Result{}, r.Status().Update(ctx, backup)
}

// addToHistory adds an entry to the backup history
func (r *BackupController) addToHistory(backup *api.ClickHouseBackup, status api.BackupStatus, errorMsg string) {
	entry := api.BackupHistoryEntry{
		BackupName:     backup.Status.BackupName,
		Status:         status,
		StartTime:      *backup.Status.StartTime,
		CompletionTime: backup.Status.CompletionTime,
		Duration:       backup.Status.Duration,
		SizeBytes:      backup.Status.LocalSizeBytes,
		Error:          errorMsg,
	}
	
	// Add to beginning of history
	backup.Status.History = append([]api.BackupHistoryEntry{entry}, backup.Status.History...)
	
	// Keep only last 10 entries
	if len(backup.Status.History) > 10 {
		backup.Status.History = backup.Status.History[:10]
	}
}

// removeFromScheduler removes a job from the scheduler
func (r *BackupController) removeFromScheduler(jobKey string) {
	if entryID, exists := r.scheduledJobs[jobKey]; exists {
		r.scheduler.Remove(entryID)
		delete(r.scheduledJobs, jobKey)
	}
}

// intervalToCron converts time intervals to cron expressions
func (r *BackupController) intervalToCron(interval string) (string, error) {
	duration, err := time.ParseDuration(interval)
	if err != nil {
		return "", err
	}
	
	switch {
	case duration < time.Hour:
		// Convert to minutes
		minutes := int(duration.Minutes())
		return fmt.Sprintf("0 */%d * * * *", minutes), nil
	case duration < 24*time.Hour:
		// Convert to hours
		hours := int(duration.Hours())
		return fmt.Sprintf("0 0 */%d * * *", hours), nil
	default:
		// Convert to days
		days := int(duration.Hours() / 24)
		return fmt.Sprintf("0 0 0 */%d * *", days), nil
	}
}

// Start starts the backup controller
func (r *BackupController) Start(ctx context.Context) error {
	r.scheduler.Start()
	return nil
}

// Stop stops the backup controller
func (r *BackupController) Stop() {
	ctx := r.scheduler.Stop()
	<-ctx.Done()
}

// SetupWithManager sets up the controller with the Manager.
func (r *BackupController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.ClickHouseBackup{}).
		Complete(r)
}