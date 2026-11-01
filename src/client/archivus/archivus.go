package archivus

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type ArchivusClient struct {
	baseURL string
	apiKey  string
	folder  string

	hc      *http.Client
	mu      sync.Mutex
	driveID string
}

// NewArchivusClient creates a client scoped to parentFolder. The base URL is
// read from ARCHIVUS_BASE_URL in the environment (via fetcher/config).
// Use WithTimeout for large uploads that outgrow the default HTTP timeout.
func NewArchivusClient(apiKey, baseUrl, parentFolder string, driveID string) *ArchivusClient {
	baseURL := strings.TrimRight(baseUrl, "/")

	return &ArchivusClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		folder:  parentFolder,
		hc:      &http.Client{Timeout: 30 * time.Second},
		driveID: driveID,
	}
}
func (c *ArchivusClient) resolvePath(symbol string) string {
	return filepath.Join(c.folder, symbol)
}

func (c *ArchivusClient) UploadFile(symbol string, files []*UploadFile) ([]ArchivusUploadFileResponse, error) {
	if len(files) == 0 {
		return []ArchivusUploadFileResponse{}, nil
	}
	folderPath := c.resolvePath(symbol)

	// EnsureFolder resolves its argument against the parent folder, so it
	// needs the symbol-relative path, not the already-resolved one.
	if err := c.EnsureFolder(symbol); err != nil {
		return []ArchivusUploadFileResponse{}, err
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	if err := w.WriteField("folderPath", folderPath); err != nil {
		return []ArchivusUploadFileResponse{}, err
	}
	if err := w.WriteField("driveId", c.driveID); err != nil {
		return []ArchivusUploadFileResponse{}, err
	}
	resp := make([]ArchivusUploadFileResponse, 0, len(files))

	for _, f := range files {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition",
			fmt.Sprintf(`form-data; name="files"; filename="%s"`, escapeQuotes(f.Name)))
		h.Set("Content-Type", "application/octet-stream")

		part, err := w.CreatePart(h)
		if err != nil {
			resp = append(resp, ArchivusUploadFileResponse{
				Name:         f.Name,
				FolderPath:   folderPath,
				IsError:      true,
				ErrorMessage: err.Error(),
			})
			continue
		}
		if _, err := part.Write(f.Content); err != nil {
			return resp, err
		}
		resp = append(resp, ArchivusUploadFileResponse{
			Name:         f.Name,
			FolderPath:   folderPath,
			IsError:      false,
			ErrorMessage: "",
		})
	}
	if err := w.Close(); err != nil {
		return resp, err
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/storage/file/upload", &buf)
	if err != nil {
		return resp, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	err = c.do(req, nil)
	if err != nil {
		return resp, err
	}
	return resp, nil
}

func (c *ArchivusClient) DownloadFile(fileID string) ([]byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/storage/file/download", nil)
	if err != nil {
		return nil, "", err
	}
	q := req.URL.Query()
	q.Set("fileId", fileID)
	q.Set("driveId", c.driveID)
	req.URL.RawQuery = q.Encode()

	c.setAuth(req)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("archivus: %s: %s", resp.Status, string(body))
	}

	return body, parseFilename(resp.Header.Get("Content-Disposition")), nil
}
