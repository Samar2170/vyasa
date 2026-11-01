// Package models defines the persisted domain types and their PostgreSQL
// table names. Payload-style data lives in jsonb columns; the fields the
// clients filter on are real columns.
package models

import "time"

// Table names.
const (
	TableCompanies        = "companies"
	TableCompanyLinks     = "company_links"
	TableCompanyResources = "company_research_resources"
	TableSavedFiles       = "saved_files"
	TableDocumentChunks   = "document_chunks"
)

// BaseModel carries the columns shared by every table.
type BaseModel struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Company struct {
	BaseModel
	Symbol   string `json:"symbol"`
	Exchange string `json:"exchange,omitempty"`
	Name     string `json:"name,omitempty"`
	Industry string `json:"industry,omitempty"`
	Series   string `json:"series,omitempty"`
	ISINCode string `json:"isin_code,omitempty"`
}

// CompanyLinks caches the resolved official website and investor relations
// page of a company (see the ir package).
type CompanyLinks struct {
	BaseModel
	CompanyID             string `json:"company_id"`
	OfficialWebsite       string `json:"official_website"`
	InvestorRelationsLink string `json:"investor_relations_link"`
}

// CompanyResearchResource is one archived research document (or link) for a
// company, keyed by (company_id, analysis_segment).
type CompanyResearchResource struct {
	BaseModel
	CompanyID            string         `json:"company_id"`
	AnalysisSegment      string         `json:"analysis_segment"`
	Source               string         `json:"source"`
	ResearchResourceLink string         `json:"research_resource_link"`
	RawDataUrl           string         `json:"raw_data_url"`
	ExtractedData        map[string]any `json:"extracted_data,omitempty"`
}

// SavedFile records one archived file: where it came from and where it is
// stored.
type SavedFile struct {
	BaseModel
	CompanyID  string  `json:"company_id"`
	Name       string  `json:"name"`
	Extension  string  `json:"extension,omitempty"`
	Size       float64 `json:"size,omitempty"`
	FolderPath string  `json:"folder_path"`
}

// DocumentChunk is one searchable slice of a parsed research document. Offset
// is the rune offset of Text within the source document, so read tools can
// jump straight to a match.
type DocumentChunk struct {
	BaseModel
	CompanyID string `json:"company_id"`
	Symbol    string `json:"symbol"`
	Segment   string `json:"segment"`
	Ordinal   int    `json:"ordinal"`
	Offset    int    `json:"offset"`
	Text      string `json:"text"`
}
