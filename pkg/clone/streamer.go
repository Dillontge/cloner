package clone

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"vitess.io/vitess/go/vt/key"
	"vitess.io/vitess/go/vt/proto/topodata"
)

type Row struct {
	Table      *Table
	ID         int64
	ShardingID int64
	Data       []interface{}
}

type RowStream interface {
	// Next returns the next row or nil if we're done
	Next(context.Context) (*Row, error)
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

func (r *rowStream) Next(ctx context.Context) (*Row, error) {
	if !r.rows.Next() {
		return nil, nil
	}
	cols, err := r.rows.Columns()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	row := make([]interface{}, len(cols))
	row[r.table.IDColumnIndex] = int64(0)
	row[r.table.ShardingColumnIndex] = int64(0)

	scanArgs := make([]interface{}, len(row))
	for i := range row {
		scanArgs[i] = &row[i]
	}
	err = r.rows.Scan(scanArgs...)

	id := row[r.table.IDColumnIndex].(int64)
	shardingID := row[r.table.ShardingColumnIndex].(int64)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &Row{
		Table:      r.table,
		ID:         id,
		ShardingID: shardingID,
		Data:       row,
	}, nil
}

type rejectStream struct {
	source RowStream
	reject func(row *Row) (bool, error)
}

func (f *rejectStream) Next(ctx context.Context) (*Row, error) {
	for {
		next, err := f.source.Next(ctx)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		if next == nil {
			return next, nil
		}
		reject, err := f.reject(next)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		if !reject {
			return next, nil
		}
	}
}

func newRejectStream(stream RowStream, filter func(row *Row) (bool, error)) RowStream {
	return &rejectStream{stream, filter}
}

func filterStreamByShard(stream RowStream, table *Table, targetFilter []*topodata.KeyRange) RowStream {
	return newRejectStream(stream, func(row *Row) (bool, error) {
		shardingValue := row.ShardingID
		return !InShard(uint64(shardingValue), targetFilter), nil
	})
}

func InShard(id uint64, shard []*topodata.KeyRange) bool {
	destination := key.DestinationKeyspaceID(vhash(id))
	for _, keyRange := range shard {
		if key.KeyRangeContains(keyRange, destination) {
			return true
		}
	}
	return false
}

func StreamChunk(ctx context.Context, conn *sql.Conn, chunk Chunk) (RowStream, error) {
	table := chunk.Table
	columns := table.ColumnList

	logger := log.WithField("table", chunk.Table.Name).WithField("task", "reader")
	if chunk.First {
		logger.Debugf("reading chunk -%v", chunk.End)
		rows, err := conn.QueryContext(ctx, fmt.Sprintf("select %s from %s where %s < ? order by %s asc",
			columns, table.Name, table.IDColumn, table.IDColumn), chunk.End)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		return newRowStream(table, rows)
	} else if chunk.Last {
		logger.Debugf("reading chunk %v-", chunk.Start)
		rows, err := conn.QueryContext(ctx, fmt.Sprintf("select %s from %s where %s >= ? order by %s asc",
			columns, table.Name, table.IDColumn, table.IDColumn), chunk.Start)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		return newRowStream(table, rows)
	} else {
		logger.Debugf("reading chunk [%v-%v)", chunk.Start, chunk.End)
		rows, err := conn.QueryContext(ctx,
			fmt.Sprintf("select %s from %s where %s >= ? and %s < ? order by %s asc",
				columns, table.Name, table.IDColumn, table.IDColumn, table.IDColumn), chunk.Start, chunk.End)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		return newRowStream(table, rows)
	}
}
