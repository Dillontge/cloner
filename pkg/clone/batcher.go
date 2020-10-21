package clone

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	writesEnqueued = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "writes_enqueued",
			Help: "How many writes, partitioned by table and type (insert, update, delete).",
		},
		[]string{"table", "type"},
	)
)

func init() {
	prometheus.MustRegister(writesEnqueued)
}

type Batch struct {
	Type  DiffType
	Table *Table
	Rows  []*Row
}

// BatchWrites consumes diffs and batches them up into batches by type and table
func BatchWrites(ctx context.Context, batchSize int, diffs chan Diff, batches chan Batch) error {
	batchesByType := make(map[DiffType]map[string]Batch)

readChannel:
	for {
		select {
		case diff, more := <-diffs:
			if more {
				batchesByTable, ok := batchesByType[diff.Type]
				if !ok {
					batchesByTable = make(map[string]Batch)
					batchesByType[diff.Type] = batchesByTable
				}
				batch, ok := batchesByTable[diff.Row.Table.Name]
				if !ok {
					batch = Batch{diff.Type, diff.Row.Table, nil}
				}
				batch.Rows = append(batch.Rows, diff.Row)

				if len(batch.Rows) >= batchSize {
					// Batch is full send it
					enqueueBatch(batches, batch)
					// and clear it
					batch.Rows = nil
				}

				batchesByTable[diff.Row.Table.Name] = batch
			} else {
				break readChannel
			}
		case <-ctx.Done():
			break readChannel
		}
	}

	// Write the final unfilled batches
	for _, batchesByTable := range batchesByType {
		for _, batch := range batchesByTable {
			if len(batch.Rows) > 0 {
				enqueueBatch(batches, batch)
			}
		}
	}
	close(batches)

	return nil
}

// BatchTableWrites consumes diffs for a single table and batches them up into batches by type
func BatchTableWrites(ctx context.Context, batchSize int, diffs chan Diff, batches chan Batch) error {
	batchesByType := make(map[DiffType]Batch)

	for {
		select {
		case diff, more := <-diffs:
			if !more {
				// Write the final unfilled batches
				for _, batch := range batchesByType {
					if len(batch.Rows) > 0 {
						enqueueBatch(batches, batch)
					}
				}
				return nil
			}

			batch, ok := batchesByType[diff.Type]
			if !ok {
				batch = Batch{diff.Type, diff.Row.Table, nil}
			}
			batch.Rows = append(batch.Rows, diff.Row)

			if len(batch.Rows) >= batchSize {
				// Batch is full send it
				enqueueBatch(batches, batch)
				// and clear it
				batch.Rows = nil
			}

			batchesByType[diff.Type] = batch
		case <-ctx.Done():
			return nil
		}
	}
}

func enqueueBatch(batches chan Batch, batch Batch) {
	writesEnqueued.WithLabelValues(batch.Table.Name, string(batch.Type)).Add(float64(len(batch.Rows)))
	batches <- batch
}
