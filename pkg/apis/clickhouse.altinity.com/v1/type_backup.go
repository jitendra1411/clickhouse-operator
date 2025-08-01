// Copyright 2019 Altinity Ltd and/or its affiliates. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1

import (
	"sync"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/altinity/clickhouse-operator/pkg/apis/common/types"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ClickHouseBackup defines a ClickHouse backup resource for declarative backup management
type ClickHouseBackup struct {
	meta.TypeMeta   `json:",inline"            yaml:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	Spec   ClickHouseBackupSpec    `json:"spec"               yaml:"spec"`
	Status *ClickHouseBackupStatus `json:"status,omitempty"   yaml:"status,omitempty"`

	runtime             *ClickHouseBackupRuntime `json:"-" yaml:"-"`
	statusCreatorMutex  sync.Mutex               `json:"-" yaml:"-"`
	runtimeCreatorMutex sync.Mutex               `json:"-" yaml:"-"`
}

// ClickHouseBackupRuntime contains runtime information for backup operations
type ClickHouseBackupRuntime struct {
	attributes        *ComparableAttributes `json:"-" yaml:"-"`
	commonConfigMutex sync.Mutex            `json:"-" yaml:"-"`
}

func newClickHouseBackupRuntime() *ClickHouseBackupRuntime {
	return &ClickHouseBackupRuntime{
		attributes: &ComparableAttributes{},
	}
}

// ClickHouseBackupSpec defines the desired state of ClickHouseBackup
type ClickHouseBackupSpec struct {
	// TaskID specifies unique backup task identifier
	TaskID *types.String `json:"taskID,omitempty" yaml:"taskID,omitempty"`

	// Suspend indicates whether the backup should be suspended
	Suspend *types.StringBool `json:"suspend,omitempty" yaml:"suspend,omitempty"`

	// ClickHouseInstallation specifies the target CHI for backup
	ClickHouseInstallation ClickHouseInstallationRef `json:"clickHouseInstallation" yaml:"clickHouseInstallation"`

	// Type specifies the backup type (full, incremental, schema, etc.)
	Type BackupType `json:"type,omitempty" yaml:"type,omitempty"`

	// Schedule defines when the backup should run (cron format)
	Schedule *types.String `json:"schedule,omitempty" yaml:"schedule,omitempty"`

	// BackupPolicy defines backup behavior and retention settings
	BackupPolicy *BackupPolicy `json:"backupPolicy,omitempty" yaml:"backupPolicy,omitempty"`

	// Storage defines where backups should be stored
	Storage *BackupStorage `json:"storage,omitempty" yaml:"storage,omitempty"`

	// Tables specifies which tables to backup (supports patterns)
	Tables []string `json:"tables,omitempty" yaml:"tables,omitempty"`

	// Partitions specifies which partitions to backup
	Partitions []string `json:"partitions,omitempty" yaml:"partitions,omitempty"`

	// IncludeSchema specifies whether to include schema in backup
	IncludeSchema *types.StringBool `json:"includeSchema,omitempty" yaml:"includeSchema,omitempty"`

	// IncludeRBAC specifies whether to include RBAC objects in backup
	IncludeRBAC *types.StringBool `json:"includeRBAC,omitempty" yaml:"includeRBAC,omitempty"`

	// IncludeConfigs specifies whether to include configuration files in backup
	IncludeConfigs *types.StringBool `json:"includeConfigs,omitempty" yaml:"includeConfigs,omitempty"`

	// DiffFrom specifies the base backup for incremental backups
	DiffFrom *types.String `json:"diffFrom,omitempty" yaml:"diffFrom,omitempty"`

	// Resume enables resumable backups
	Resume *types.StringBool `json:"resume,omitempty" yaml:"resume,omitempty"`

	// Compression defines backup compression settings
	Compression *BackupCompression `json:"compression,omitempty" yaml:"compression,omitempty"`
}

// ClickHouseInstallationRef references a ClickHouseInstallation
type ClickHouseInstallationRef struct {
	Name      string `json:"name" yaml:"name"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
}

// BackupType defines the type of backup
type BackupType string

const (
	BackupTypeFull        BackupType = "full"
	BackupTypeIncremental BackupType = "incremental"
	BackupTypeSchema      BackupType = "schema"
	BackupTypeRBAC        BackupType = "rbac"
	BackupTypeConfigs     BackupType = "configs"
)

// BackupPolicy defines backup behavior and retention
type BackupPolicy struct {
	// RetentionPolicy defines how long to keep backups
	RetentionPolicy *RetentionPolicy `json:"retentionPolicy,omitempty" yaml:"retentionPolicy,omitempty"`

	// FullBackupInterval defines how often to create full backups (for incremental sequences)
	FullBackupInterval *types.String `json:"fullBackupInterval,omitempty" yaml:"fullBackupInterval,omitempty"`

	// MaxConcurrentBackups limits concurrent backup operations
	MaxConcurrentBackups *types.Int32 `json:"maxConcurrentBackups,omitempty" yaml:"maxConcurrentBackups,omitempty"`

	// WatchMode enables continuous backup watching with full+incremental sequences
	WatchMode *WatchModeConfig `json:"watchMode,omitempty" yaml:"watchMode,omitempty"`
}

// RetentionPolicy defines backup retention rules
type RetentionPolicy struct {
	// KeepLocal specifies how many local backups to keep
	KeepLocal *types.Int32 `json:"keepLocal,omitempty" yaml:"keepLocal,omitempty"`

	// KeepRemote specifies how many remote backups to keep
	KeepRemote *types.Int32 `json:"keepRemote,omitempty" yaml:"keepRemote,omitempty"`

	// MaxAge specifies maximum age for backups (duration format)
	MaxAge *types.String `json:"maxAge,omitempty" yaml:"maxAge,omitempty"`
}

// WatchModeConfig defines continuous backup watching configuration
type WatchModeConfig struct {
	// Enabled turns on watch mode
	Enabled *types.StringBool `json:"enabled,omitempty" yaml:"enabled,omitempty"`

	// WatchInterval defines interval for incremental backups
	WatchInterval *types.String `json:"watchInterval,omitempty" yaml:"watchInterval,omitempty"`

	// FullInterval defines interval for full backups in watch mode
	FullInterval *types.String `json:"fullInterval,omitempty" yaml:"fullInterval,omitempty"`

	// BackupNameTemplate defines template for backup names
	BackupNameTemplate *types.String `json:"backupNameTemplate,omitempty" yaml:"backupNameTemplate,omitempty"`

	// DeleteLocal specifies whether to delete local backups after upload
	DeleteLocal *types.StringBool `json:"deleteLocal,omitempty" yaml:"deleteLocal,omitempty"`
}

// BackupStorage defines backup storage configuration
type BackupStorage struct {
	// Type specifies storage type (s3, ftp, sftp, gcs, azblob, local)
	Type StorageType `json:"type" yaml:"type"`

	// S3 configuration for S3-compatible storage
	S3 *S3Config `json:"s3,omitempty" yaml:"s3,omitempty"`

	// GCS configuration for Google Cloud Storage
	GCS *GCSConfig `json:"gcs,omitempty" yaml:"gcs,omitempty"`

	// Azure configuration for Azure Blob Storage
	Azure *AzureConfig `json:"azure,omitempty" yaml:"azure,omitempty"`

	// FTP configuration for FTP storage
	FTP *FTPConfig `json:"ftp,omitempty" yaml:"ftp,omitempty"`

	// SFTP configuration for SFTP storage
	SFTP *SFTPConfig `json:"sftp,omitempty" yaml:"sftp,omitempty"`

	// Local configuration for local storage
	Local *LocalConfig `json:"local,omitempty" yaml:"local,omitempty"`
}

// StorageType defines supported storage types
type StorageType string

const (
	StorageTypeS3    StorageType = "s3"
	StorageTypeGCS   StorageType = "gcs"
	StorageTypeAzure StorageType = "azure"
	StorageTypeFTP   StorageType = "ftp"
	StorageTypeSFTP  StorageType = "sftp"
	StorageTypeLocal StorageType = "local"
)

// S3Config defines S3-compatible storage configuration
type S3Config struct {
	Endpoint                *types.String     `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	Bucket                  *types.String     `json:"bucket" yaml:"bucket"`
	Path                    *types.String     `json:"path,omitempty" yaml:"path,omitempty"`
	Region                  *types.String     `json:"region,omitempty" yaml:"region,omitempty"`
	AccessKey               *types.String     `json:"accessKey,omitempty" yaml:"accessKey,omitempty"`
	SecretKey               *types.String     `json:"secretKey,omitempty" yaml:"secretKey,omitempty"`
	ForcePathStyle          *types.StringBool `json:"forcePathStyle,omitempty" yaml:"forcePathStyle,omitempty"`
	DisableCertVerification *types.StringBool `json:"disableCertVerification,omitempty" yaml:"disableCertVerification,omitempty"`
	DisableSSL              *types.StringBool `json:"disableSSL,omitempty" yaml:"disableSSL,omitempty"`
}

// GCSConfig defines Google Cloud Storage configuration
type GCSConfig struct {
	Bucket      *types.String `json:"bucket" yaml:"bucket"`
	Path        *types.String `json:"path,omitempty" yaml:"path,omitempty"`
	Credentials *types.String `json:"credentials,omitempty" yaml:"credentials,omitempty"`
}

// AzureConfig defines Azure Blob Storage configuration
type AzureConfig struct {
	Container   *types.String `json:"container" yaml:"container"`
	Path        *types.String `json:"path,omitempty" yaml:"path,omitempty"`
	AccountName *types.String `json:"accountName,omitempty" yaml:"accountName,omitempty"`
	AccountKey  *types.String `json:"accountKey,omitempty" yaml:"accountKey,omitempty"`
}

// FTPConfig defines FTP storage configuration
type FTPConfig struct {
	Host     *types.String `json:"host" yaml:"host"`
	Port     *types.Int32  `json:"port,omitempty" yaml:"port,omitempty"`
	Username *types.String `json:"username,omitempty" yaml:"username,omitempty"`
	Password *types.String `json:"password,omitempty" yaml:"password,omitempty"`
	Path     *types.String `json:"path,omitempty" yaml:"path,omitempty"`
}

// SFTPConfig defines SFTP storage configuration
type SFTPConfig struct {
	Host     *types.String `json:"host" yaml:"host"`
	Port     *types.Int32  `json:"port,omitempty" yaml:"port,omitempty"`
	Username *types.String `json:"username,omitempty" yaml:"username,omitempty"`
	Password *types.String `json:"password,omitempty" yaml:"password,omitempty"`
	Key      *types.String `json:"key,omitempty" yaml:"key,omitempty"`
	Path     *types.String `json:"path,omitempty" yaml:"path,omitempty"`
}

// LocalConfig defines local storage configuration
type LocalConfig struct {
	Path *types.String `json:"path" yaml:"path"`
}

// BackupCompression defines backup compression settings
type BackupCompression struct {
	Type  CompressionType `json:"type,omitempty" yaml:"type,omitempty"`
	Level *types.Int32    `json:"level,omitempty" yaml:"level,omitempty"`
}

// CompressionType defines supported compression types
type CompressionType string

const (
	CompressionTypeNone   CompressionType = "none"
	CompressionTypeGZIP   CompressionType = "gzip"
	CompressionTypeLZ4    CompressionType = "lz4"
	CompressionTypeZSTD   CompressionType = "zstd"
	CompressionTypeBrotli CompressionType = "brotli"
)

// ClickHouseBackupStatus defines the observed state of ClickHouseBackup
type ClickHouseBackupStatus struct {
	// TaskID specifies current backup task ID
	TaskID string `json:"taskID,omitempty" yaml:"taskID,omitempty"`

	// Status specifies overall backup status
	Status BackupStatus `json:"status,omitempty" yaml:"status,omitempty"`

	// Phase specifies current backup phase
	Phase BackupPhase `json:"phase,omitempty" yaml:"phase,omitempty"`

	// Message provides human-readable status message
	Message string `json:"message,omitempty" yaml:"message,omitempty"`

	// Error contains error message if backup failed
	Error string `json:"error,omitempty" yaml:"error,omitempty"`

	// BackupName specifies the actual backup name created
	BackupName string `json:"backupName,omitempty" yaml:"backupName,omitempty"`

	// LocalPath specifies local backup path
	LocalPath string `json:"localPath,omitempty" yaml:"localPath,omitempty"`

	// RemotePath specifies remote backup path
	RemotePath string `json:"remotePath,omitempty" yaml:"remotePath,omitempty"`

	// StartTime specifies when backup started
	StartTime *meta.Time `json:"startTime,omitempty" yaml:"startTime,omitempty"`

	// CompletionTime specifies when backup completed
	CompletionTime *meta.Time `json:"completionTime,omitempty" yaml:"completionTime,omitempty"`

	// Duration specifies backup duration
	Duration *meta.Duration `json:"duration,omitempty" yaml:"duration,omitempty"`

	// LocalSizeBytes specifies local backup size in bytes
	LocalSizeBytes *int64 `json:"localSizeBytes,omitempty" yaml:"localSizeBytes,omitempty"`

	// RemoteSizeBytes specifies remote backup size in bytes
	RemoteSizeBytes *int64 `json:"remoteSizeBytes,omitempty" yaml:"remoteSizeBytes,omitempty"`

	// Tables specifies which tables were backed up
	Tables []string `json:"tables,omitempty" yaml:"tables,omitempty"`

	// NextScheduledBackup specifies when next backup is scheduled
	NextScheduledBackup *meta.Time `json:"nextScheduledBackup,omitempty" yaml:"nextScheduledBackup,omitempty"`

	// LastSuccessfulBackup specifies last successful backup time
	LastSuccessfulBackup *meta.Time `json:"lastSuccessfulBackup,omitempty" yaml:"lastSuccessfulBackup,omitempty"`

	// ConsecutiveFailures specifies number of consecutive failures
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty" yaml:"consecutiveFailures,omitempty"`

	// History contains recent backup history
	History []BackupHistoryEntry `json:"history,omitempty" yaml:"history,omitempty"`
}

// BackupStatus defines backup status values
type BackupStatus string

const (
	BackupStatusPending   BackupStatus = "Pending"
	BackupStatusRunning   BackupStatus = "Running"
	BackupStatusCompleted BackupStatus = "Completed"
	BackupStatusFailed    BackupStatus = "Failed"
	BackupStatusSuspended BackupStatus = "Suspended"
	BackupStatusScheduled BackupStatus = "Scheduled"
)

// BackupPhase defines backup operation phases
type BackupPhase string

const (
	BackupPhasePending      BackupPhase = "Pending"
	BackupPhaseInitializing BackupPhase = "Initializing"
	BackupPhaseCreating     BackupPhase = "Creating"
	BackupPhaseBackingUp    BackupPhase = "BackingUp"
	BackupPhaseUploading    BackupPhase = "Uploading"
	BackupPhaseCompleted    BackupPhase = "Completed"
	BackupPhaseFailed       BackupPhase = "Failed"
	BackupPhaseScheduled    BackupPhase = "Scheduled"
	BackupPhaseWatching     BackupPhase = "Watching"
)

// BackupHistoryEntry represents a single backup history entry
type BackupHistoryEntry struct {
	BackupName     string         `json:"backupName" yaml:"backupName"`
	Status         BackupStatus   `json:"status" yaml:"status"`
	StartTime      meta.Time      `json:"startTime" yaml:"startTime"`
	CompletionTime *meta.Time     `json:"completionTime,omitempty" yaml:"completionTime,omitempty"`
	Duration       *meta.Duration `json:"duration,omitempty" yaml:"duration,omitempty"`
	SizeBytes      *int64         `json:"sizeBytes,omitempty" yaml:"sizeBytes,omitempty"`
	Error          string         `json:"error,omitempty" yaml:"error,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ClickHouseBackupList contains a list of ClickHouseBackup resources
type ClickHouseBackupList struct {
	meta.TypeMeta `json:",inline" yaml:",inline"`
	meta.ListMeta `json:"metadata" yaml:"metadata"`
	Items         []ClickHouseBackup `json:"items" yaml:"items"`
}

// EnsureRuntime ensures runtime is initialized
func (backup *ClickHouseBackup) EnsureRuntime() *ClickHouseBackupRuntime {
	if backup == nil {
		return nil
	}

	if backup.runtime != nil {
		return backup.runtime
	}

	backup.runtimeCreatorMutex.Lock()
	defer backup.runtimeCreatorMutex.Unlock()

	if backup.runtime == nil {
		backup.runtime = newClickHouseBackupRuntime()
	}
	return backup.runtime
}

// EnsureStatus ensures status is initialized
func (backup *ClickHouseBackup) EnsureStatus() *ClickHouseBackupStatus {
	if backup == nil {
		return nil
	}

	if backup.Status != nil {
		return backup.Status
	}

	backup.statusCreatorMutex.Lock()
	defer backup.statusCreatorMutex.Unlock()

	if backup.Status == nil {
		backup.Status = &ClickHouseBackupStatus{}
	}
	return backup.Status
}

// GetStatus gets status
func (backup *ClickHouseBackup) GetStatus() *ClickHouseBackupStatus {
	if backup == nil {
		return nil
	}
	return backup.Status
}

// HasStatus checks whether backup has status
func (backup *ClickHouseBackup) HasStatus() bool {
	if backup == nil {
		return false
	}
	return backup.Status != nil
}

// IsCompleted checks if backup is completed
func (backup *ClickHouseBackup) IsCompleted() bool {
	if !backup.HasStatus() {
		return false
	}
	return backup.Status.Status == BackupStatusCompleted
}

// IsFailed checks if backup failed
func (backup *ClickHouseBackup) IsFailed() bool {
	if !backup.HasStatus() {
		return false
	}
	return backup.Status.Status == BackupStatusFailed
}

// IsRunning checks if backup is currently running
func (backup *ClickHouseBackup) IsRunning() bool {
	if !backup.HasStatus() {
		return false
	}
	return backup.Status.Status == BackupStatusRunning
}

// IsSuspended checks if backup is suspended
func (backup *ClickHouseBackup) IsSuspended() bool {
	if backup.Spec.Suspend != nil && backup.Spec.Suspend.Value() {
		return true
	}
	if !backup.HasStatus() {
		return false
	}
	return backup.Status.Status == BackupStatusSuspended
}

// GetTaskID gets task ID
func (backup *ClickHouseBackup) GetTaskID() string {
	if backup.Spec.TaskID != nil {
		return backup.Spec.TaskID.Value()
	}
	return ""
}

// GetBackupType gets backup type
func (backup *ClickHouseBackup) GetBackupType() BackupType {
	if backup.Spec.Type == "" {
		return BackupTypeFull
	}
	return backup.Spec.Type
}

// IsScheduled checks if backup is scheduled
func (backup *ClickHouseBackup) IsScheduled() bool {
	return backup.Spec.Schedule != nil && backup.Spec.Schedule.Value() != ""
}

// IsWatchMode checks if backup is in watch mode
func (backup *ClickHouseBackup) IsWatchMode() bool {
	return backup.Spec.BackupPolicy != nil &&
		backup.Spec.BackupPolicy.WatchMode != nil &&
		backup.Spec.BackupPolicy.WatchMode.Enabled != nil &&
		backup.Spec.BackupPolicy.WatchMode.Enabled.Value()
}

// GetCHIRef gets the ClickHouseInstallation reference
func (backup *ClickHouseBackup) GetCHIRef() ClickHouseInstallationRef {
	return backup.Spec.ClickHouseInstallation
}
