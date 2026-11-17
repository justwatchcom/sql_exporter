package query

// ExecResponseRowType describes column metadata from a query response.
type ExecResponseRowType struct {
	Name       string          `json:"name"`
	Fields     []FieldMetadata `json:"fields"`
	ByteLength int64           `json:"byteLength"`
	Length     int64           `json:"length"`
	Type       string          `json:"type"`
	Precision  int64           `json:"precision"`
	Scale      int64           `json:"scale"`
	Nullable   bool            `json:"nullable"`
}

// FieldMetadata describes metadata for a field, including nested fields for complex types.
type FieldMetadata struct {
	Name      string          `json:"name,omitempty"`
	Type      string          `json:"type"`
	Nullable  bool            `json:"nullable"`
	Length    int             `json:"length"`
	Scale     int             `json:"scale"`
	Precision int             `json:"precision"`
	Fields    []FieldMetadata `json:"fields,omitempty"`
}

// ExecResponseChunk describes metadata for a chunk of query results, including URL and size information.
type ExecResponseChunk struct {
	URL              string `json:"url"`
	RowCount         int    `json:"rowCount"`
	UncompressedSize int64  `json:"uncompressedSize"`
	CompressedSize   int64  `json:"compressedSize"`
}
