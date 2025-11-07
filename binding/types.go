package binding

type Config struct {
	DbId          string   `json:"dbId"`
	Version       Version  `json:"version"`
	BigIntEnabled bool     `json:"bigIntEnabled"`
	OpfsEnabled   bool     `json:"opfsEnabled"`
	VfsList       []string `json:"vfsList"`
}

type Version struct {
	LibVersion       string  `json:"libVersion"`
	SourceId         string  `json:"sourceId"`
	LibVersionNumber float64 `json:"libVersionNumber"`
	DownloadVersion  float64 `json:"downloadVersion"`
}

type OpenResult struct {
	DbId       string `json:"dbId"`
	Filename   string `json:"filename"`
	Persistent bool   `json:"persistent"`
	Vfs        string `json:"vfs"`
}

type CloseResult struct {
	Filename string `json:"filename"`
}

type RowResult struct {
	Type        string   `json:"type"`
	RowNumber   int      `json:"rowNumber"` // 1 based.
	Row         []any    `json:"row"`
	ColumnNames []string `json:"columnNames"`

	Error error `json:"-"`
}
