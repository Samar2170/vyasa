// Package client defines the interfaces the data source clients implement
// and the storage contract they are wired against.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"vyasa/src/client/archivus"
	"vyasa/src/models"
	"vyasa/src/store"
)

type DataSourceClient interface {
	GetData(ctx context.Context, co models.Company, args json.RawMessage) (json.RawMessage, error)
	RefreshData(ctx context.Context, co models.Company, args json.RawMessage) (json.RawMessage, error)
	SaveToFileCollection(ctx context.Context, co models.Company, raw []byte, args json.RawMessage) error
	SaveToFile(ctx context.Context, co models.Company, raw []byte, args json.RawMessage) error
	Execute(ctx context.Context, co models.Company) error
}

type StorageClient interface {
	UploadFile(symbol string, files []*archivus.UploadFile) ([]archivus.ArchivusUploadFileResponse, error)
	DownloadFile(fileID string) ([]byte, string, error)
	LookupFile(folderPath, fileName string) (*archivus.FileInfo, error)
}

// SaveUploadedPaths records uploaded file locations in saved_files. Upload
// errors are skipped so partially failed batches still record what landed.
func SaveUploadedPaths(ctx context.Context, st *store.Store, co models.Company, paths []archivus.ArchivusUploadFileResponse) error {
	if st == nil || len(paths) == 0 {
		return nil
	}
	now := time.Now()
	b := &pgx.Batch{}
	for _, p := range paths {
		if p.IsError {
			continue
		}
		b.Queue(`INSERT INTO saved_files (company_id, name, extension, folder_path, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $5)`,
			co.ID, p.Name, strings.TrimPrefix(strings.ToLower(filepath.Ext(p.Name)), "."), p.FolderPath, now)
	}
	if err := st.Pool.SendBatch(ctx, b).Close(); err != nil {
		return fmt.Errorf("client: save file paths: %w", err)
	}
	return nil
}
