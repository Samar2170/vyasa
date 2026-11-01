package archivus

type FileInfo struct {
	ID             string  `json:"ID"`
	Name           string  `json:"Name"`
	IsDir          bool    `json:"IsDir"`
	Extension      string  `json:"Extension"`
	SignedURL      string  `json:"SignedUrl"`
	Size           float64 `json:"Size"`
	Path           string  `json:"Path"`
	Thumbnail      string  `json:"Thumbnail"`
	NavigationPath string  `json:"NavigationPath"`
}

type UploadFile struct {
	Name    string
	Content []byte
}

type listResponse struct {
	Files []FileInfo `json:"files"`
}

type ArchivusUploadFileResponse struct {
	Name         string `json:"name"`
	FolderPath   string `json:"folder_path"`
	IsError      bool   `json:"isError"`
	ErrorMessage string `json:"errorMessage"`
}
