package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	_ "github.com/mailru/go-clickhouse/v2"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
)

// NativeBackupExecutor implements backup functionality directly within the operator
type NativeBackupExecutor struct {
	namespace string
	chi       *chiv1.ClickHouseInstallation
	backup    *chiv1.ClickHouseBackup
}

// NewNativeBackupExecutor creates a new native backup executor
func NewNativeBackupExecutor(namespace string, chi *chiv1.ClickHouseInstallation, backup *chiv1.ClickHouseBackup) *NativeBackupExecutor {
	return &NativeBackupExecutor{
		namespace: namespace,
		chi:       chi,
		backup:    backup,
	}
}

// BackupResult represents the result of a backup operation
type BackupResult struct {
	BackupName   string
	Tables       []string
	Size         int64
	Duration     time.Duration
	Location     string
	StoragePath  string
	Error        error
}

// ExecuteBackup performs the actual backup operation
func (e *NativeBackupExecutor) ExecuteBackup(ctx context.Context) (*BackupResult, error) {
	startTime := time.Now()
	backupName := e.generateBackupName()
	
	result := &BackupResult{
		BackupName: backupName,
		Tables:     e.backup.Spec.Tables,
	}

	// Connect to ClickHouse
	db, err := e.connectToClickHouse(ctx)
	if err != nil {
		result.Error = fmt.Errorf("failed to connect to ClickHouse: %w", err)
		return result, err
	}
	defer db.Close()

	// Create local backup directory
	backupDir := filepath.Join("/tmp", "backups", backupName)
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		result.Error = fmt.Errorf("failed to create backup directory: %w", err)
		return result, err
	}

	var totalSize int64

	// Backup each table
	for _, tablePattern := range e.backup.Spec.Tables {
		tables, err := e.resolveTables(ctx, db, tablePattern)
		if err != nil {
			result.Error = fmt.Errorf("failed to resolve tables for pattern %s: %w", tablePattern, err)
			return result, err
		}

		for _, table := range tables {
			size, err := e.backupTable(ctx, db, table, backupDir)
			if err != nil {
				result.Error = fmt.Errorf("failed to backup table %s: %w", table, err)
				return result, err
			}
			totalSize += size
		}
	}

	// Backup schema if requested
	if e.backup.Spec.IncludeSchema != nil && e.backup.Spec.IncludeSchema.Value() {
		schemaSize, err := e.backupSchema(ctx, db, backupDir)
		if err != nil {
			result.Error = fmt.Errorf("failed to backup schema: %w", err)
			return result, err
		}
		totalSize += schemaSize
	}

	result.Size = totalSize
	result.Location = "local"
	result.Duration = time.Since(startTime)

	// Upload to remote storage if configured
	if e.backup.Spec.Storage != nil {
		remotePath, err := e.uploadToStorage(ctx, backupDir, backupName)
		if err != nil {
			result.Error = fmt.Errorf("failed to upload to storage: %w", err)
			return result, err
		}
		result.StoragePath = remotePath
		result.Location = "remote"
	}

	return result, nil
}

// connectToClickHouse establishes connection to ClickHouse
func (e *NativeBackupExecutor) connectToClickHouse(ctx context.Context) (*sql.DB, error) {
	// Get ClickHouse connection details from CHI
	host := fmt.Sprintf("%s.%s.svc.cluster.local", 
		e.chi.Name, e.namespace)
	port := "9000"
	
	// Build connection string
	dsn := fmt.Sprintf("tcp://%s:%s?database=default&read_timeout=30&write_timeout=30", host, port)
	
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		return nil, err
	}

	// Test connection
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

// generateBackupName creates a unique backup name
func (e *NativeBackupExecutor) generateBackupName() string {
	timestamp := time.Now().Format("20060102-150405")
	if e.backup.Spec.BackupPolicy.WatchMode.BackupNameTemplate != nil && e.backup.Spec.BackupPolicy.WatchMode.BackupNameTemplate.String() != "" {
		name := strings.ReplaceAll(e.backup.Spec.BackupPolicy.WatchMode.BackupNameTemplate.String(), "{time}", timestamp)
		name = strings.ReplaceAll(name, "{chi}", e.chi.Name)
		name = strings.ReplaceAll(name, "{type}", string(e.backup.Spec.Type))
		return name
	}
	return fmt.Sprintf("%s-%s-%s", e.chi.Name, e.backup.Spec.Type, timestamp)
}

// resolveTables resolves table patterns to actual table names
func (e *NativeBackupExecutor) resolveTables(ctx context.Context, db *sql.DB, pattern string) ([]string, error) {
	var tables []string
	
	// Handle wildcard patterns
	if strings.Contains(pattern, "*") {
		parts := strings.Split(pattern, ".")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid table pattern: %s", pattern)
		}
		
		dbPattern := parts[0]
		tablePattern := parts[1]
		
		query := `
			SELECT database, name 
			FROM system.tables 
			WHERE database LIKE ? AND name LIKE ?
			ORDER BY database, name`
		
		dbLike := strings.ReplaceAll(dbPattern, "*", "%")
		tableLike := strings.ReplaceAll(tablePattern, "*", "%")
		
		rows, err := db.QueryContext(ctx, query, dbLike, tableLike)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		
		for rows.Next() {
			var database, tableName string
			if err := rows.Scan(&database, &tableName); err != nil {
				return nil, err
			}
			tables = append(tables, fmt.Sprintf("%s.%s", database, tableName))
		}
	} else {
		// Exact table name
		tables = []string{pattern}
	}
	
	return tables, nil
}

// backupTable backs up a single table
func (e *NativeBackupExecutor) backupTable(ctx context.Context, db *sql.DB, tableName, backupDir string) (int64, error) {
	// Get table structure
	createQuery, err := e.getTableCreateStatement(ctx, db, tableName)
	if err != nil {
		return 0, fmt.Errorf("failed to get table structure: %w", err)
	}

	// Create table structure file
	structureFile := filepath.Join(backupDir, fmt.Sprintf("%s.sql", strings.ReplaceAll(tableName, ".", "_")))
	if err := os.WriteFile(structureFile, []byte(createQuery), 0644); err != nil {
		return 0, err
	}

	// Export table data
	dataFile := filepath.Join(backupDir, fmt.Sprintf("%s.data", strings.ReplaceAll(tableName, ".", "_")))
	size, err := e.exportTableData(ctx, db, tableName, dataFile)
	if err != nil {
		return 0, err
	}

	return size, nil
}

// getTableCreateStatement gets the CREATE TABLE statement
func (e *NativeBackupExecutor) getTableCreateStatement(ctx context.Context, db *sql.DB, tableName string) (string, error) {
	parts := strings.Split(tableName, ".")
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid table name format: %s", tableName)
	}
	
	query := "SHOW CREATE TABLE " + tableName
	var createStatement string
	
	err := db.QueryRowContext(ctx, query).Scan(&createStatement)
	if err != nil {
		return "", err
	}
	
	return createStatement, nil
}

// exportTableData exports table data to a file
func (e *NativeBackupExecutor) exportTableData(ctx context.Context, db *sql.DB, tableName, filePath string) (int64, error) {
	// Create the data file
	file, err := os.Create(filePath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	// Export data using SELECT INTO OUTFILE equivalent
	query := fmt.Sprintf("SELECT * FROM %s FORMAT TabSeparated", tableName)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	// Get column information
	columns, err := rows.Columns()
	if err != nil {
		return 0, err
	}

	var totalSize int64
	
	// Write data rows
	for rows.Next() {
		values := make([]interface{}, len(columns))
		valuePtrs := make([]interface{}, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}
		
		if err := rows.Scan(valuePtrs...); err != nil {
			return 0, err
		}
		
		// Convert to tab-separated string
		stringValues := make([]string, len(values))
		for i, v := range values {
			if v == nil {
				stringValues[i] = "\\N"
			} else {
				stringValues[i] = fmt.Sprintf("%v", v)
			}
		}
		
		line := strings.Join(stringValues, "\t") + "\n"
		n, err := file.WriteString(line)
		if err != nil {
			return 0, err
		}
		totalSize += int64(n)
	}

	return totalSize, nil
}

// backupSchema backs up database schema
func (e *NativeBackupExecutor) backupSchema(ctx context.Context, db *sql.DB, backupDir string) (int64, error) {
	// Get all databases that match our backup patterns
	databases := make(map[string]bool)
	for _, pattern := range e.backup.Spec.Tables {
		parts := strings.Split(pattern, ".")
		if len(parts) == 2 {
			databases[parts[0]] = true
		}
	}

	var totalSize int64
	
	// Backup schema for each database
	for dbName := range databases {
		if dbName == "*" {
			// Get all databases
			allDbs, err := e.getAllDatabases(ctx, db)
			if err != nil {
				return 0, err
			}
			for _, actualDb := range allDbs {
				size, err := e.backupDatabaseSchema(ctx, db, actualDb, backupDir)
				if err != nil {
					return 0, err
				}
				totalSize += size
			}
		} else {
			size, err := e.backupDatabaseSchema(ctx, db, dbName, backupDir)
			if err != nil {
				return 0, err
			}
			totalSize += size
		}
	}

	return totalSize, nil
}

// getAllDatabases gets all database names
func (e *NativeBackupExecutor) getAllDatabases(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var databases []string
	for rows.Next() {
		var dbName string
		if err := rows.Scan(&dbName); err != nil {
			return nil, err
		}
		// Skip system databases
		if dbName != "system" && dbName != "information_schema" && dbName != "INFORMATION_SCHEMA" {
			databases = append(databases, dbName)
		}
	}

	return databases, nil
}

// backupDatabaseSchema backs up schema for a specific database
func (e *NativeBackupExecutor) backupDatabaseSchema(ctx context.Context, db *sql.DB, dbName, backupDir string) (int64, error) {
	schemaFile := filepath.Join(backupDir, fmt.Sprintf("schema_%s.sql", dbName))
	file, err := os.Create(schemaFile)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	var totalSize int64

	// Write database creation
	createDbSQL := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s;\n\n", dbName)
	n, err := file.WriteString(createDbSQL)
	if err != nil {
		return 0, err
	}
	totalSize += int64(n)

	// Get all tables in the database
	query := "SELECT name FROM system.tables WHERE database = ? ORDER BY name"
	rows, err := db.QueryContext(ctx, query, dbName)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var tableName string
		if err := rows.Scan(&tableName); err != nil {
			return 0, err
		}
		tables = append(tables, tableName)
	}

	// Write CREATE TABLE statements
	for _, tableName := range tables {
		fullTableName := fmt.Sprintf("%s.%s", dbName, tableName)
		createSQL, err := e.getTableCreateStatement(ctx, db, fullTableName)
		if err != nil {
			return 0, err
		}
		
		statement := fmt.Sprintf("-- Table: %s\n%s;\n\n", fullTableName, createSQL)
		n, err := file.WriteString(statement)
		if err != nil {
			return 0, err
		}
		totalSize += int64(n)
	}

	return totalSize, nil
}

// uploadToStorage uploads backup to configured storage
func (e *NativeBackupExecutor) uploadToStorage(ctx context.Context, backupDir, backupName string) (string, error) {
	if e.backup.Spec.Storage == nil {
		return "", fmt.Errorf("no storage configuration provided")
	}

	switch e.backup.Spec.Storage.Type {
	case "s3":
		return e.uploadToS3(ctx, backupDir, backupName)
	default:
		return "", fmt.Errorf("unsupported storage type: %s", e.backup.Spec.Storage.Type)
	}
}

// uploadToS3 uploads backup files to S3-compatible storage
func (e *NativeBackupExecutor) uploadToS3(ctx context.Context, backupDir, backupName string) (string, error) {
	s3Config := e.backup.Spec.Storage.S3
	if s3Config == nil {
		return "", fmt.Errorf("S3 configuration is required")
	}

	// Create AWS session
	awsConfig := &aws.Config{
		Region:           aws.String("us-east-1"), // Default region
		Credentials:      credentials.NewStaticCredentials(s3Config.AccessKey.String(), s3Config.SecretKey.String(), ""),
		S3ForcePathStyle: aws.Bool(s3Config.ForcePathStyle.Value()),
	}

	if s3Config.Endpoint != nil && s3Config.Endpoint.String() != "" {
		awsConfig.Endpoint = aws.String(s3Config.Endpoint.String())
	}

	if s3Config.DisableSSL != nil && s3Config.DisableSSL != nil && s3Config.DisableSSL.Value() && s3Config.DisableSSL.Value() {
		awsConfig.DisableSSL = aws.Bool(true)
	}

	sess, err := session.NewSession(awsConfig)
	if err != nil {
		return "", fmt.Errorf("failed to create AWS session: %w", err)
	}

	uploader := s3manager.NewUploader(sess)

	// Upload all files in the backup directory
	return e.uploadDirectoryToS3(ctx, uploader, backupDir, s3Config.Bucket.String(), s3Config.Path.String(), backupName)
}

// uploadDirectoryToS3 uploads all files in a directory to S3
func (e *NativeBackupExecutor) uploadDirectoryToS3(ctx context.Context, uploader *s3manager.Uploader, 
	localDir, bucket, s3Path, backupName string) (string, error) {
	
	err := filepath.Walk(localDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		// Open the file
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		// Calculate S3 key
		relPath, err := filepath.Rel(localDir, path)
		if err != nil {
			return err
		}
		
		s3Key := filepath.Join(s3Path, backupName, relPath)
		if s3Path == "" {
			s3Key = filepath.Join(backupName, relPath)
		}

		// Convert to forward slashes for S3
		s3Key = strings.ReplaceAll(s3Key, "\\", "/")

		// Upload file
		_, err = uploader.UploadWithContext(ctx, &s3manager.UploadInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(s3Key),
			Body:   file,
		})

		return err
	})

	if err != nil {
		return "", err
	}

	// Return the S3 path
	if s3Path == "" {
		return fmt.Sprintf("s3://%s/%s", bucket, backupName), nil
	}
	return fmt.Sprintf("s3://%s/%s/%s", bucket, s3Path, backupName), nil
}

// ExecuteRestore performs backup restoration
func (e *NativeBackupExecutor) ExecuteRestore(ctx context.Context, backupName string) error {
	// Implementation for restore functionality
	// This would reverse the backup process
	return fmt.Errorf("restore functionality not yet implemented")
}

// ListBackups lists available backups
func (e *NativeBackupExecutor) ListBackups(ctx context.Context) ([]BackupInfo, error) {
	var backups []BackupInfo
	
	// List local backups
	localBackups, err := e.listLocalBackups()
	if err != nil {
		return nil, err
	}
	backups = append(backups, localBackups...)
	
	// List remote backups if storage is configured
	if e.backup.Spec.Storage != nil {
		remoteBackups, err := e.listRemoteBackups(ctx)
		if err != nil {
			// Don't fail if remote listing fails, just log
			// Failed to list remote backups, continuing
		} else {
			backups = append(backups, remoteBackups...)
		}
	}
	
	// Sort by creation time
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].Created.After(backups[j].Created)
	})
	
	return backups, nil
}

// BackupInfo represents information about a backup
type BackupInfo struct {
	Name     string
	Created  time.Time
	Size     int64
	Location string
	Path     string
}

// listLocalBackups lists backups stored locally
func (e *NativeBackupExecutor) listLocalBackups() ([]BackupInfo, error) {
	backupRoot := "/tmp/backups"
	if _, err := os.Stat(backupRoot); os.IsNotExist(err) {
		return []BackupInfo{}, nil
	}
	
	entries, err := os.ReadDir(backupRoot)
	if err != nil {
		return nil, err
	}
	
	var backups []BackupInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		
		info, err := entry.Info()
		if err != nil {
			continue
		}
		
		backup := BackupInfo{
			Name:     entry.Name(),
			Created:  info.ModTime(),
			Location: "local",
			Path:     filepath.Join(backupRoot, entry.Name()),
		}
		
		// Calculate directory size
		backup.Size = e.calculateDirectorySize(backup.Path)
		backups = append(backups, backup)
	}
	
	return backups, nil
}

// listRemoteBackups lists backups stored in remote storage
func (e *NativeBackupExecutor) listRemoteBackups(ctx context.Context) ([]BackupInfo, error) {
	if e.backup.Spec.Storage.Type != "s3" {
		return []BackupInfo{}, nil
	}
	
	s3Config := e.backup.Spec.Storage.S3
	
	// Create AWS session (similar to uploadToS3)
	awsConfig := &aws.Config{
		Region:           aws.String("us-east-1"),
		Credentials:      credentials.NewStaticCredentials(s3Config.AccessKey.String(), s3Config.SecretKey.String(), ""),
		S3ForcePathStyle: aws.Bool(s3Config.ForcePathStyle.Value()),
	}
	
	if s3Config.Endpoint != nil && s3Config.Endpoint.String() != "" {
		awsConfig.Endpoint = aws.String(s3Config.Endpoint.String())
	}
	
	if s3Config.DisableSSL != nil && s3Config.DisableSSL != nil && s3Config.DisableSSL.Value() && s3Config.DisableSSL.Value() {
		awsConfig.DisableSSL = aws.Bool(true)
	}
	
	sess, err := session.NewSession(awsConfig)
	if err != nil {
		return nil, err
	}
	
	svc := s3.New(sess)
	
	// List objects in the backup path
	prefix := s3Config.Path.String()
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	
	input := &s3.ListObjectsV2Input{
		Bucket:    aws.String(s3Config.Bucket.String()),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
	}
	
	result, err := svc.ListObjectsV2WithContext(ctx, input)
	if err != nil {
		return nil, err
	}
	
	var backups []BackupInfo
	backupSizes := make(map[string]int64)
	
	// Process common prefixes (backup directories)
	for _, commonPrefix := range result.CommonPrefixes {
		if commonPrefix.Prefix == nil {
			continue
		}
		
		prefixStr := *commonPrefix.Prefix
		backupName := strings.TrimSuffix(strings.TrimPrefix(prefixStr, prefix), "/")
		
		if backupName == "" {
			continue
		}
		
		// Get the size of this backup by listing its contents
		size, created := e.getBackupSizeAndTime(ctx, svc, s3Config.Bucket.String(), prefixStr)
		backupSizes[backupName] = size
		
		backup := BackupInfo{
			Name:     backupName,
			Created:  created,
			Size:     size,
			Location: "remote",
			Path:     fmt.Sprintf("s3://%s/%s", s3Config.Bucket.String(), prefixStr),
		}
		
		backups = append(backups, backup)
	}
	
	return backups, nil
}

// getBackupSizeAndTime gets the total size and creation time of a backup in S3
func (e *NativeBackupExecutor) getBackupSizeAndTime(ctx context.Context, svc *s3.S3, bucket, prefix string) (int64, time.Time) {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	}
	
	var totalSize int64
	var earliestTime time.Time
	
	err := svc.ListObjectsV2PagesWithContext(ctx, input, func(page *s3.ListObjectsV2Output, lastPage bool) bool {
		for _, obj := range page.Contents {
			if obj.Size != nil {
				totalSize += *obj.Size
			}
			if obj.LastModified != nil {
				if earliestTime.IsZero() || obj.LastModified.Before(earliestTime) {
					earliestTime = *obj.LastModified
				}
			}
		}
		return true
	})
	
	if err != nil {
		return 0, time.Now()
	}
	
	if earliestTime.IsZero() {
		earliestTime = time.Now()
	}
	
	return totalSize, earliestTime
}

// calculateDirectorySize calculates the total size of a directory
func (e *NativeBackupExecutor) calculateDirectorySize(dirPath string) int64 {
	var size int64
	
	filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	
	return size
}

