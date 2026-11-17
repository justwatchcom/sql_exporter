package query

// ToFieldMetadata transforms ExecResponseRowType to FieldMetadata.
func (ex *ExecResponseRowType) ToFieldMetadata() FieldMetadata {
	return FieldMetadata{
		ex.Name,
		ex.Type,
		ex.Nullable,
		int(ex.Length),
		int(ex.Scale),
		int(ex.Precision),
		ex.Fields,
	}
}
