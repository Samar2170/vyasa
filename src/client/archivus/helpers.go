package archivus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func (c *ArchivusClient) list(full string) ([]FileInfo, error) {

	req, err := c.newJSONRequest(http.MethodPost, "/storage/files", map[string]string{
		"path":    full,
		"driveId": c.driveID,
	})
	if err != nil {
		return nil, err
	}

	var out listResponse
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out.Files, nil
}

func (c *ArchivusClient) EnsureFolder(folderPath string) error {
	full := c.resolvePath(folderPath)
	if full == "" {
		return nil
	}

	exists, err := c.folderExists(full)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return c.createFolder(full)
}

func (c *ArchivusClient) folderExists(full string) (bool, error) {
	parent, name := splitPath(full)
	entries, err := c.list(parent)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.IsDir && e.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func splitPath(p string) (parent, name string) {
	p = strings.Trim(p, "/")
	idx := strings.LastIndex(p, "/")
	if idx < 0 {
		return "", p
	}
	return p[:idx], p[idx+1:]
}

func escapeQuotes(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}

func (c *ArchivusClient) createFolder(full string) error {

	req, err := c.newJSONRequest(http.MethodPost, "/storage/folder/create", map[string]string{
		"path":    full,
		"driveId": c.driveID,
	})
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

func (c *ArchivusClient) newJSONRequest(method, path string, payload any) (*http.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)
	return req, nil
}

func (c *ArchivusClient) setAuth(req *http.Request) {
	req.Header.Set("X-API-Key", c.apiKey)
}

func (c *ArchivusClient) do(req *http.Request, out any) error {
	c.setAuth(req)

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("archivus: %s: %s", resp.Status, string(body))
	}
	if out != nil && len(body) > 0 {
		return json.Unmarshal(body, out)
	}
	return nil
}

func (c *ArchivusClient) WithTimeout(d time.Duration) *ArchivusClient {
	c.hc.Timeout = d
	return c
}

func parseFilename(cd string) string {
	for _, part := range strings.Split(cd, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "filename=") {
			return strings.Trim(strings.TrimPrefix(part, "filename="), `"`)
		}
	}
	return ""
}

func (c *ArchivusClient) LookupFile(folderPath, fileName string) (*FileInfo, error) {
	full := c.resolvePath(folderPath)
	entries, err := c.list(full)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir && e.Name == fileName {
			return &e, nil
		}
	}
	return nil, nil
}
