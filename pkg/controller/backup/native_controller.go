package backup

import (
	"context"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
)

// NativeBackupReconciler reconciles ClickHouseBackup objects natively
type NativeBackupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	
	// Cron scheduler for scheduled backups
	scheduler *cron.Cron
	
	// Active backup jobs
	activeJobs map[string]context.CancelFunc
}

// NewNativeBackupReconciler creates a new native backup reconciler
func NewNativeBackupReconciler(client client.Client, scheme *runtime.Scheme, recorder record.EventRecorder) *NativeBackupReconciler {
	return &NativeBackupReconciler{
		Client:     client,
		Scheme:     scheme,
		Recorder:   recorder,
		scheduler:  cron.New(),
		activeJobs: make(map[string]context.CancelFunc),
	}
}

// Reconcile handles ClickHouseBackup resources
func (r *NativeBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	// Fetch the ClickHouseBackup instance
	backup := &chiv1.ClickHouseBackup{}
	err := r.Get(ctx, req.NamespacedName, backup)
	if err != nil {
		if errors.IsNotFound(err) {
			// Backup was deleted, cleanup any scheduled jobs
			r.cleanupBackupJob(req.NamespacedName.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Initialize status if needed
	if backup.Status.Phase == "" {
		backup.Status.Phase = chiv1.BackupPhasePending
		backup.Status.Status = chiv1.BackupStatusPending
		if err := r.Status().Update(ctx, backup); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Get the referenced ClickHouse installation
	chi := &chiv1.ClickHouseInstallation{}
	chiKey := client.ObjectKey{
		Namespace: backup.Spec.ClickHouseInstallation.Namespace,
		Name:      backup.Spec.ClickHouseInstallation.Name,
	}
	
	if err := r.Get(ctx, chiKey, chi); err != nil {
		if errors.IsNotFound(err) {
			r.updateBackupStatus(ctx, backup, chiv1.BackupStatusFailed, chiv1.BackupPhaseFailed, 
				fmt.Sprintf("ClickHouse installation not found: %s/%s", 
					backup.Spec.ClickHouseInstallation.Namespace,
					backup.Spec.ClickHouseInstallation.Name))
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle scheduled backups
	if backup.Spec.Schedule != nil && backup.Spec.Schedule.String() != "" {
		return r.handleScheduledBackup(ctx, backup, chi)
	}

	// Handle watch mode backups
	if backup.Spec.BackupPolicy != nil && backup.Spec.BackupPolicy.WatchMode != nil && 
		backup.Spec.BackupPolicy.WatchMode.Enabled != nil && backup.Spec.BackupPolicy.WatchMode.Enabled.Value() {
		return r.handleWatchModeBackup(ctx, backup, chi)
	}

	// Handle one-time backup
	return r.handleOneTimeBackup(ctx, backup, chi)
}

// handleOneTimeBackup processes a single backup execution
func (r *NativeBackupReconciler) handleOneTimeBackup(ctx context.Context, backup *chiv1.ClickHouseBackup, chi *chiv1.ClickHouseInstallation) (ctrl.Result, error) {
	
	// Skip if already completed or failed
	if backup.Status.Status == chiv1.BackupStatusCompleted || backup.Status.Status == chiv1.BackupStatusFailed {
		return ctrl.Result{}, nil
	}

	// Skip if already running
	if backup.Status.Status == chiv1.BackupStatusRunning {
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// Start the backup
	log.FromContext(ctx).Info("Starting one-time backup", "backup", backup.Name)
	
	// Update status to running
	if err := r.updateBackupStatus(ctx, backup, chiv1.BackupStatusRunning, chiv1.BackupPhaseBackingUp, ""); err != nil {
		return ctrl.Result{}, err
	}

	// Execute backup in background
	go r.executeBackupAsync(backup, chi)

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// handleScheduledBackup manages cron-based scheduled backups
func (r *NativeBackupReconciler) handleScheduledBackup(ctx context.Context, backup *chiv1.ClickHouseBackup, chi *chiv1.ClickHouseInstallation) (ctrl.Result, error) {
	jobKey := fmt.Sprintf("%s/%s", backup.Namespace, backup.Name)

	// Check if job is already scheduled
	existingJob := r.getScheduledJob(jobKey)
	if existingJob == nil {
		// Schedule new job
		job := &ScheduledBackupJob{
			backup: backup.DeepCopy(),
			chi:    chi.DeepCopy(),
			reconciler: r,
		}

		entryID, err := r.scheduler.AddFunc(backup.Spec.Schedule.String(), job.Execute)
		if err != nil {
			log.FromContext(ctx).Error(err, "Failed to schedule backup", "backup", backup.Name, "schedule", backup.Spec.Schedule)
			return ctrl.Result{}, err
		}

		job.entryID = entryID
		r.setScheduledJob(jobKey, job)

		log.FromContext(ctx).Info("Scheduled backup job", "backup", backup.Name, "schedule", backup.Spec.Schedule)

		// Update next scheduled time in status
		next := r.scheduler.Entry(entryID).Next
		backup.Status.NextScheduledBackup = &metav1.Time{Time: next}
		if err := r.Status().Update(ctx, backup); err != nil {
			return ctrl.Result{}, err
		}

		r.Recorder.Event(backup, corev1.EventTypeNormal, "Scheduled", 
			fmt.Sprintf("Backup scheduled with cron expression: %s", backup.Spec.Schedule))
	}

	// Update status to scheduled if not already
	if backup.Status.Status != chiv1.BackupStatusScheduled {
		if err := r.updateBackupStatus(ctx, backup, chiv1.BackupStatusScheduled, chiv1.BackupPhaseScheduled, ""); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: time.Hour}, nil
}

// handleWatchModeBackup manages continuous watch mode backups
func (r *NativeBackupReconciler) handleWatchModeBackup(ctx context.Context, backup *chiv1.ClickHouseBackup, chi *chiv1.ClickHouseInstallation) (ctrl.Result, error) {
	jobKey := fmt.Sprintf("%s/%s", backup.Namespace, backup.Name)

	// Check if watch job is already running
	if _, exists := r.activeJobs[jobKey]; !exists {
		// Start watch mode
		watchCtx, cancel := context.WithCancel(context.Background())
		r.activeJobs[jobKey] = cancel

		go r.runWatchMode(watchCtx, backup.DeepCopy(), chi.DeepCopy())

		log.FromContext(ctx).Info("Started watch mode backup", "backup", backup.Name)
		r.Recorder.Event(backup, corev1.EventTypeNormal, "WatchStarted", "Watch mode backup started")
	}

	// Update status to running if not already
	if backup.Status.Status != chiv1.BackupStatusRunning {
		if err := r.updateBackupStatus(ctx, backup, chiv1.BackupStatusRunning, chiv1.BackupPhaseWatching, ""); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: time.Hour}, nil
}

// executeBackupAsync runs backup execution in a separate goroutine
func (r *NativeBackupReconciler) executeBackupAsync(backup *chiv1.ClickHouseBackup, chi *chiv1.ClickHouseInstallation) {
	ctx := context.Background()

	// Create backup executor
	executor := NewNativeBackupExecutor(backup.Namespace, chi, backup)

	// Record start time
	startTime := time.Now()
	backup.Status.StartTime = &metav1.Time{Time: startTime}
	r.Status().Update(ctx, backup)

	// Execute the backup
	result, err := executor.ExecuteBackup(ctx)
	
	// Update backup status based on result
	if err != nil {
		log.FromContext(ctx).Error(err, "Backup execution failed", "backup", backup.Name)
		r.updateBackupStatus(ctx, backup, chiv1.BackupStatusFailed, chiv1.BackupPhaseFailed, err.Error())
		r.Recorder.Event(backup, corev1.EventTypeWarning, "BackupFailed", err.Error())
		return
	}

	// Success - update status
	backup.Status.Status = chiv1.BackupStatusCompleted
	backup.Status.Phase = chiv1.BackupPhaseCompleted
	backup.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	backup.Status.Duration = &metav1.Duration{Duration: result.Duration}
	backup.Status.BackupName = result.BackupName
	backup.Status.LocalSizeBytes = &result.Size
	backup.Status.LocalPath = result.StoragePath

	// Add to history
	historyEntry := chiv1.BackupHistoryEntry{
		BackupName:     result.BackupName,
		StartTime:      metav1.Time{Time: startTime},
		CompletionTime: &metav1.Time{Time: time.Now()},
		Duration:       &metav1.Duration{Duration: result.Duration},
		Status:         chiv1.BackupStatusCompleted,
		SizeBytes:      &result.Size,
	}

	backup.Status.History = append(backup.Status.History, historyEntry)
	
	// Keep only recent history entries
	if len(backup.Status.History) > 10 {
		backup.Status.History = backup.Status.History[len(backup.Status.History)-10:]
	}

	backup.Status.LastSuccessfulBackup = &metav1.Time{Time: time.Now()}

	if err := r.Status().Update(ctx, backup); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update backup status")
		return
	}

	log.FromContext(ctx).Info("Backup completed successfully", 
		"backup", backup.Name, 
		"backupName", result.BackupName,
		"size", result.Size,
		"duration", result.Duration)

	r.Recorder.Event(backup, corev1.EventTypeNormal, "BackupCompleted", 
		fmt.Sprintf("Backup %s completed successfully (size: %d bytes, duration: %v)", 
			result.BackupName, result.Size, result.Duration))
}

// runWatchMode runs continuous backup monitoring
func (r *NativeBackupReconciler) runWatchMode(ctx context.Context, backup *chiv1.ClickHouseBackup, chi *chiv1.ClickHouseInstallation) {
	watchConfig := backup.Spec.BackupPolicy.WatchMode

	watchInterval, err := time.ParseDuration(watchConfig.WatchInterval.String())
	if err != nil {
		log.FromContext(ctx).Error(err, "Invalid watch interval", "interval", watchConfig.WatchInterval.String())
		return
	}

	fullInterval, err := time.ParseDuration(watchConfig.FullInterval.String())
	if err != nil {
		log.FromContext(ctx).Error(err, "Invalid full interval", "interval", watchConfig.FullInterval.String())
		return
	}

	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()

	lastFullBackup := time.Now()

	for {
		select {
		case <-ctx.Done():
			log.FromContext(ctx).Info("Watch mode stopped", "backup", backup.Name)
			return
		case <-ticker.C:
			// Determine backup type
			backupType := chiv1.BackupTypeIncremental
			if time.Since(lastFullBackup) >= fullInterval {
				backupType = chiv1.BackupTypeFull
				lastFullBackup = time.Now()
			}

			// Create a temporary backup spec for execution
			watchBackup := backup.DeepCopy()
			watchBackup.Spec.Type = backupType

			// Generate backup name
			timestamp := time.Now().Format("20060102-150405")
			backupName := fmt.Sprintf("watch-%s-%s-%s", backup.Name, backupType, timestamp)
			if watchConfig.BackupNameTemplate != nil && watchConfig.BackupNameTemplate.String() != "" {
				backupName = watchConfig.BackupNameTemplate.String()
				backupName = fmt.Sprintf(backupName, backup.Name, backupType, timestamp)
			}

			log.FromContext(ctx).Info("Executing watch mode backup", 
				"backup", backup.Name, 
				"type", backupType,
				"backupName", backupName)

			// Execute backup
			executor := NewNativeBackupExecutor(backup.Namespace, chi, watchBackup)
			result, err := executor.ExecuteBackup(ctx)

			if err != nil {
				log.FromContext(ctx).Error(err, "Watch mode backup failed", "backup", backup.Name)
				r.Recorder.Event(backup, corev1.EventTypeWarning, "WatchBackupFailed", err.Error())
				continue
			}

			log.FromContext(ctx).Info("Watch mode backup completed", 
				"backup", backup.Name,
				"backupName", result.BackupName,
				"size", result.Size)

			// Update backup status with latest execution
			backup.Status.LastSuccessfulBackup = &metav1.Time{Time: time.Now()}
			backup.Status.BackupName = result.BackupName
			backup.Status.LocalSizeBytes = &result.Size

			// Add to history
			historyEntry := chiv1.BackupHistoryEntry{
				BackupName:     result.BackupName,
				StartTime:      metav1.Time{Time: time.Now().Add(-result.Duration)},
				CompletionTime: &metav1.Time{Time: time.Now()},
				Duration:       &metav1.Duration{Duration: result.Duration},
				Status:         chiv1.BackupStatusCompleted,
				SizeBytes:      &result.Size,
			}

			backup.Status.History = append(backup.Status.History, historyEntry)
			if len(backup.Status.History) > 50 { // Keep more history for watch mode
				backup.Status.History = backup.Status.History[len(backup.Status.History)-50:]
			}

			r.Status().Update(context.Background(), backup)
		}
	}
}

// updateBackupStatus updates the backup status
func (r *NativeBackupReconciler) updateBackupStatus(ctx context.Context, backup *chiv1.ClickHouseBackup, 
	status chiv1.BackupStatus, phase chiv1.BackupPhase, message string) error {
	
	backup.Status.Status = status
	backup.Status.Phase = phase
	if message != "" {
		backup.Status.Error = message
	}
	
	return r.Status().Update(ctx, backup)
}

// cleanupBackupJob removes scheduled jobs and active watch jobs
func (r *NativeBackupReconciler) cleanupBackupJob(jobKey string) {
	// Cancel any active watch job
	if cancel, exists := r.activeJobs[jobKey]; exists {
		cancel()
		delete(r.activeJobs, jobKey)
	}

	// Remove from scheduler if exists
	if job := r.getScheduledJob(jobKey); job != nil {
		r.scheduler.Remove(job.entryID)
		r.removeScheduledJob(jobKey)
	}
}

// ScheduledBackupJob represents a scheduled backup job
type ScheduledBackupJob struct {
	backup     *chiv1.ClickHouseBackup
	chi        *chiv1.ClickHouseInstallation
	reconciler *NativeBackupReconciler
	entryID    cron.EntryID
}

// Execute runs the scheduled backup
func (job *ScheduledBackupJob) Execute() {
	ctx := context.Background()

	log.FromContext(ctx).Info("Executing scheduled backup", "backup", job.backup.Name)

	// Execute the backup
	job.reconciler.executeBackupAsync(job.backup, job.chi)
}

// Scheduler job management (these would be implemented with proper locking in production)
var scheduledJobs = make(map[string]*ScheduledBackupJob)

func (r *NativeBackupReconciler) getScheduledJob(key string) *ScheduledBackupJob {
	return scheduledJobs[key]
}

func (r *NativeBackupReconciler) setScheduledJob(key string, job *ScheduledBackupJob) {
	scheduledJobs[key] = job
}

func (r *NativeBackupReconciler) removeScheduledJob(key string) {
	delete(scheduledJobs, key)
}

// SetupWithManager sets up the controller with the Manager
func (r *NativeBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Start the cron scheduler
	r.scheduler.Start()

	return ctrl.NewControllerManagedBy(mgr).
		For(&chiv1.ClickHouseBackup{}).
		Complete(r)
}