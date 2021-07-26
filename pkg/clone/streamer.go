package clone

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/pkg/errors"
	"github.com/platinummonkey/go-concurrency-limits/core"
	"github.com/prometheus/client_golang/prometheus"
	"strings"
)

// DBReader is an interface that can be implemented by sql.Conn or sql.Tx or sql.DB so that we can
// easily change synchronization method
type DBReader interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
}

type Row struct {
	Table *Table
	ID    int64
	Data  []interface{}
}

// PkAfterOrEqual returns true if the pk of the row is higher or equal to the PK of the receiver row
func (r *Row) PkAfterOrEqual(row []interface{}) bool {
	return r.ID >= r.Table.PkOfRow(row)
}

// PkEqual returns true if the pk of the row is equal to the PK of the receiver row
func (r *Row) PkEqual(row []interface{}) bool {
	return r.ID == r.Table.PkOfRow(row)
}

func (r *Row) Updated(row []interface{}) *Row {
	if r.Table.PkOfRow(row) != r.ID {
		panic("updating row with another ID")
	}
	return &Row{
		Table: r.Table,
		ID:    r.ID,
		Data:  row,
	}
}

type limitingDBReader struct {
	limiter       core.Limiter
	acquireMetric prometheus.Observer
	reader        DBReader
}

func (l *limitingDBReader) QueryContext(ctx context.Context, query string, args ...interface{}) (rows *sql.Rows, err error) {
	acquireTimer := prometheus.NewTimer(l.acquireMetric)
	token, ok := l.limiter.Acquire(ctx)
	if !ok {
		if token != nil {
			token.OnDropped()
		}
		if ctx.Err() != nil {
			return nil, errors.Wrap(ctx.Err(), "context deadline exceeded")
		} else {
			return nil, errors.New("context deadline exceeded")
		}
	}
	acquireTimer.ObserveDuration()

	defer func() {
		if err == nil {
			token.OnSuccess()
		} else {
			token.OnDropped()
		}
	}()

	rows, err = l.reader.QueryContext(ctx, query, args...)
	return rows, errors.WithStack(err)
}

func Limit(db DBReader, limiter core.Limiter, acquireMetric prometheus.Observer) DBReader {
	return &limitingDBReader{
		limiter:       limiter,
		acquireMetric: acquireMetric,
		reader:        db,
	}
}

type bufferStream struct {
	rows []*Row
}

func (b *bufferStream) Next() (*Row, error) {
	if len(b.rows) == 0 {
		return nil, nil
	}
	row := b.rows[0]
	b.rows = b.rows[1:]
	return row, nil
}

func (b *bufferStream) Close() error {
	// nothing to do here
	return nil
}

// buffer buffers all of the rows into memory
func buffer(stream RowStream) (RowStream, error) {
	rows, err := readAll(stream)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &bufferStream{rows}, nil
}

// stream converts buffered rows to a stream
func stream(rows []*Row) RowStream {
	return &bufferStream{rows}
}

// buffer buffers all of the rows into memory
func readAll(stream RowStream) ([]*Row, error) {
	defer stream.Close()
	var rows []*Row
	for {
		row, err := stream.Next()
		if err != nil {
			return nil, errors.WithStack(err)
		}
		if row == nil {
			break
		}
		rows = append(rows, row)
	}
	return rows, nil
}

type RowStream interface {
	// Next returns the next row or nil if we're done
	Next() (*Row, error)
	// Close releases any potential underlying resources
	Close() error
}

type rowStream struct {
	table   *Table
	rows    *sql.Rows
	columns []string
}

func newRowStream(table *Table, rows *sql.Rows) (*rowStream, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &rowStream{table, rows, columns}, nil
}

func (s *rowStream) Next() (*Row, error) {
	if !s.rows.Next() {
		return nil, nil
	}
	cols, err := s.rows.Columns()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	row := make([]interface{}, len(cols))

	var id int64

	scanArgs := make([]interface{}, len(row))
	for i := range row {
		if i == s.table.IDColumnIndex {
			scanArgs[i] = &id
		} else {
			scanArgs[i] = &row[i]
		}
	}
	err = s.rows.Scan(scanArgs...)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// We replaced the data in the row slice with pointers to the local vars, so lets put this back after the read
	row[s.table.IDColumnIndex] = id
	return &Row{
		Table: s.table,
		ID:    id,
		Data:  row,
	}, nil
}

func (s *rowStream) Close() error {
	return s.rows.Close()
}

func StreamChunk(ctx context.Context, conn DBReader, chunk Chunk, hint string, extraWhereClause string) (RowStream, error) {
	table := chunk.Table
	columns := table.ColumnList

	where := chunkWhere(chunk, extraWhereClause)
	stmt := fmt.Sprintf("select %s %s from %s %s order by %s asc", columns, hint, table.Name, where, table.IDColumn)
	rows, err := conn.QueryContext(ctx, stmt)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return newRowStream(table, rows)
}

func chunkWhere(chunk Chunk, extraWhereClause string) string {
	table := chunk.Table
	var clauses []string
	if extraWhereClause != "" {
		clauses = append(clauses, "("+extraWhereClause+")")
	}
	if chunk.First && chunk.Last {
		// this chunk is the full table, no where clause
	} else {
		if chunk.First {
			clauses = append(clauses, fmt.Sprintf("%s < %d", table.IDColumn, chunk.End))
		} else if chunk.Last {
			// TODO This means the tail chunk is "infinite" which could cause issues with the retrying checksummer
			//      since it's very likely we add new rows to the tail chunk. There might be very few moments when the
			//      tail chunk is fully in sync with the replication source.
			//      A better option would be to keep the tail chunk "fixed size" from the moment of time of chunking
			//      but our chunks extend from the Start row until just before the End row so we don't get "gaps" in
			//      the non-tail chunks. Since this is a tail chunk we don't know the End row. So we would need to
			//      rethink this whole thing.
			//      Let's see how we go, maybe it's fine.
			clauses = append(clauses, fmt.Sprintf("%s >= %d", table.IDColumn, chunk.Start))
		} else {
			clauses = append(clauses,
				fmt.Sprintf("%s >= %d", table.IDColumn, chunk.Start),
				fmt.Sprintf("%s < %d", table.IDColumn, chunk.End))
		}
	}
	if len(clauses) == 0 {
		return ""
	} else {
		return "where " + strings.Join(clauses, " and ")
	}
}
