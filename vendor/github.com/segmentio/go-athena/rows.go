package athena

import (
	"database/sql/driver"
	"io"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/athena"
	"github.com/aws/aws-sdk-go/service/athena/athenaiface"
)

type rows struct {
	athena  athenaiface.AthenaAPI
	queryID string

	done          bool
	skipHeaderRow bool
	out           *athena.GetQueryResultsOutput
}

type rowsConfig struct {
	Athena     athenaiface.AthenaAPI
	QueryID    string
	SkipHeader bool
}

func newRows(cfg rowsConfig) (*rows, error) {
	r := rows{
		athena:        cfg.Athena,
		queryID:       cfg.QueryID,
		skipHeaderRow: cfg.SkipHeader,
	}

	shouldContinue, err := r.fetchNextPage(nil)
	if err != nil {
		return nil, err
	}

	r.done = !shouldContinue
	return &r, nil
}

func (r *rows) Columns() []string {
	var columns []string
	for _, colInfo := range r.out.ResultSet.ResultSetMetadata.ColumnInfo {
		columns = append(columns, *colInfo.Name)
	}

	return columns
}

func (r *rows) ColumnTypeDatabaseTypeName(index int) string {
	colInfo := r.out.ResultSet.ResultSetMetadata.ColumnInfo[index]
	if colInfo.Type != nil {
		return *colInfo.Type
	}
	return ""
}

func (r *rows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}

	// If nothing left to iterate...
	if len(r.out.ResultSet.Rows) == 0 {
		// And if nothing more to paginate...
		if r.out.NextToken == nil || *r.out.NextToken == "" {
			return io.EOF
		}

		cont, err := r.fetchNextPage(r.out.NextToken)
		if err != nil {
			return err
		}

		if !cont {
			return io.EOF
		}
	}

	// Shift to next row
	cur := r.out.ResultSet.Rows[0]
	columns := r.out.ResultSet.ResultSetMetadata.ColumnInfo
	if err := convertRow(columns, cur.Data, dest); err != nil {
		return err
	}

	r.out.ResultSet.Rows = r.out.ResultSet.Rows[1:]
	return nil
}

func (r *rows) fetchNextPage(token *string) (bool, error) {
	var err error
	r.out, err = r.athena.GetQueryResults(&athena.GetQueryResultsInput{
		QueryExecutionId: aws.String(r.queryID),
		NextToken:        token,
	})
	if err != nil {
		return false, err
	}

	var rowOffset = 0
	// First row of the first page contains header if the query is not DDL.
	// These are also available in *athena.Row.ResultSetMetadata.
	if r.skipHeaderRow {
		rowOffset = 1
		r.skipHeaderRow = false
	}

	if len(r.out.ResultSet.Rows) < rowOffset+1 {
		return false, nil
	}

	r.out.ResultSet.Rows = r.out.ResultSet.Rows[rowOffset:]
	return true, nil
}

func (r *rows) Close() error {
	r.done = true
	return nil
}
