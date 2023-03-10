// Copyright (c) The Thanos Community Authors.
// Licensed under the Apache License 2.0.

package scan

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"

	"github.com/thanos-community/promql-engine/execution/function"
	"github.com/thanos-community/promql-engine/execution/model"
	engstore "github.com/thanos-community/promql-engine/execution/storage"
	"github.com/thanos-community/promql-engine/query"
)

type matrixScanner struct {
	labels         labels.Labels
	signature      uint64
	previousPoints []promql.Point
	samples        *storage.BufferedSeriesIterator
}

type matrixSelector struct {
	funcExpr *parser.Call
	storage  engstore.SeriesSelector
	call     function.FunctionCall
	scanners []matrixScanner
	series   []labels.Labels
	once     sync.Once

	vectorPool *model.VectorPool

	numSteps    int
	mint        int64
	maxt        int64
	step        int64
	selectRange int64
	offset      int64
	currentStep int64

	shard     int
	numShards int

	// Lookback delta for extended range functions.
	extLookbackDelta int64
}

// NewMatrixSelector creates operator which selects vector of series over time.
func NewMatrixSelector(
	pool *model.VectorPool,
	selector engstore.SeriesSelector,
	call function.FunctionCall,
	funcExpr *parser.Call,
	opts *query.Options,
	selectRange, offset time.Duration,
	shard, numShard int,
	extLookbackDelta time.Duration,
) model.VectorOperator {
	// TODO(fpetkovski): Add offset parameter.
	return &matrixSelector{
		storage:    selector,
		call:       call,
		funcExpr:   funcExpr,
		vectorPool: pool,

		numSteps: opts.NumSteps(),
		mint:     opts.Start.UnixMilli(),
		maxt:     opts.End.UnixMilli(),
		step:     opts.Step.Milliseconds(),

		selectRange: selectRange.Milliseconds(),
		offset:      offset.Milliseconds(),
		currentStep: opts.Start.UnixMilli(),

		shard:     shard,
		numShards: numShard,

		extLookbackDelta: extLookbackDelta.Milliseconds(),
	}
}

func (o *matrixSelector) Explain() (me string, next []model.VectorOperator) {
	r := time.Duration(o.selectRange) * time.Millisecond
	if o.call != nil {
		return fmt.Sprintf("[*matrixSelector] %v({%v}[%s] %v mod %v)", o.funcExpr.Func.Name, o.storage.Matchers(), r, o.shard, o.numShards), nil
	}
	return fmt.Sprintf("[*matrixSelector] {%v}[%s] %v mod %v", o.storage.Matchers(), r, o.shard, o.numShards), nil
}

func (o *matrixSelector) Series(ctx context.Context) ([]labels.Labels, error) {
	if err := o.loadSeries(ctx); err != nil {
		return nil, err
	}
	return o.series, nil
}

func (o *matrixSelector) GetPool() *model.VectorPool {
	return o.vectorPool
}

func (o *matrixSelector) Next(ctx context.Context) ([]model.StepVector, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if o.currentStep > o.maxt {
		return nil, nil
	}

	if err := o.loadSeries(ctx); err != nil {
		return nil, err
	}

	vectors := o.vectorPool.GetVectorBatch()
	ts := o.currentStep
	for i := 0; i < len(o.scanners); i++ {
		var (
			series   = o.scanners[i]
			seriesTs = ts
		)

		for currStep := 0; currStep < o.numSteps && seriesTs <= o.maxt; currStep++ {
			if len(vectors) <= currStep {
				vectors = append(vectors, o.vectorPool.GetStepVector(seriesTs))
			}
			maxt := seriesTs - o.offset
			mint := maxt - o.selectRange
			rangePoints, err := selectPoints(series.samples, mint, maxt, o.scanners[i].previousPoints, o.funcExpr.Func.Name, o.extLookbackDelta)
			if err != nil {
				return nil, err
			}

			// TODO(saswatamcode): Handle multi-arg functions for matrixSelectors.
			// Also, allow operator to exist independently without being nested
			// under parser.Call by implementing new data model.
			// https://github.com/thanos-community/promql-engine/issues/39
			result := o.call(function.FunctionArgs{
				Labels:      series.labels,
				Points:      rangePoints,
				StepTime:    seriesTs,
				SelectRange: o.selectRange,
				Offset:      o.offset,
			})

			if result.Point != function.InvalidSample.Point {
				vectors[currStep].T = result.T
				if result.H != nil {
					vectors[currStep].AppendHistogram(o.vectorPool, series.signature, result.H)
				} else {
					vectors[currStep].AppendSample(o.vectorPool, series.signature, result.V)
				}
			}

			o.scanners[i].previousPoints = rangePoints

			// Only buffer stepRange milliseconds from the second step on.
			stepRange := o.selectRange
			if stepRange > o.step {
				stepRange = o.step
			}
			series.samples.ReduceDelta(stepRange)

			seriesTs += o.step
		}
	}
	// For instant queries, set the step to a positive value
	// so that the operator can terminate.
	if o.step == 0 {
		o.step = 1
	}
	o.currentStep += o.step * int64(o.numSteps)

	return vectors, nil
}

func (o *matrixSelector) loadSeries(ctx context.Context) error {
	var err error
	o.once.Do(func() {
		series, loadErr := o.storage.GetSeries(ctx, o.shard, o.numShards)
		if loadErr != nil {
			err = loadErr
			return
		}

		o.scanners = make([]matrixScanner, len(series))
		o.series = make([]labels.Labels, len(series))
		for i, s := range series {
			lbls := s.Labels()
			if o.funcExpr.Func.Name != "last_over_time" {
				// This modifies the array in place. Because labels.Labels
				// can be re-used between different Select() calls, it means that
				// we have to copy it here.
				// TODO(GiedriusS): could we identify somehow whether labels.Labels
				// is reused between Select() calls?
				lbls, _ = function.DropMetricName(lbls.Copy())
			}

			// If we are dealing with an extended range function we need to search further in the past for valid series.
			var selectRange = o.selectRange
			if function.IsExtFunction(o.funcExpr.Func.Name) {
				selectRange += o.extLookbackDelta
			}

			sort.Sort(lbls)

			o.scanners[i] = matrixScanner{
				labels:    lbls,
				signature: s.Signature,
				samples:   storage.NewBufferIterator(s.Iterator(nil), selectRange),
			}
			o.series[i] = lbls
		}
		o.vectorPool.SetStepSize(len(series))
	})
	return err
}

// matrixIterSlice populates a matrix vector covering the requested range for a
// single time series, with points retrieved from an iterator.
//
// As an optimization, the matrix vector may already contain points of the same
// time series from the evaluation of an earlier step (with lower mint and maxt
// values). Any such points falling before mint are discarded; points that fall
// into the [mint, maxt] range are retained; only points with later timestamps
// are populated from the iterator.
// TODO(fpetkovski): Add max samples limit.
func selectPoints(it *storage.BufferedSeriesIterator, mint, maxt int64, out []promql.Point, functionName string, extLookbackDelta int64) ([]promql.Point, error) {
	extRange := function.IsExtFunction(functionName)
	var extMint int64
	if extRange {
		extMint = mint - extLookbackDelta
	}

	if len(out) > 0 && out[len(out)-1].T >= mint {
		// There is an overlap between previous and current ranges, retain common
		// points. In most such cases:
		//   (a) the overlap is significantly larger than the eval step; and/or
		//   (b) the number of samples is relatively small.
		// so a linear search will be as fast as a binary search.
		var drop int
		if !extRange {
			for drop = 0; out[drop].T < mint; drop++ {
			}
			// Only append points with timestamps after the last timestamp we have.
			mint = out[len(out)-1].T + 1
		} else {
			// This is an argument to an extended range function, first go past mint.
			for drop = 0; drop < len(out) && out[drop].T <= mint; drop++ {

			}
			// Then, go back one sample if within lookbackDelta of mint.
			if drop > 0 && out[drop-1].T >= extMint {
				drop--
			}
			if out[len(out)-1].T >= mint {
				// Only append points with timestamps after the last timestamp we have.
				mint = out[len(out)-1].T + 1
			}
		}
		copy(out, out[drop:])
		out = out[:len(out)-drop]
	} else {
		out = out[:0]
	}

	soughtValueType := it.Seek(maxt)
	if soughtValueType == chunkenc.ValNone {
		if it.Err() != nil {
			return nil, it.Err()
		}
	}

	appendedPointBeforeMint := len(out) > 0
	buf := it.Buffer()
loop:
	for {
		switch buf.Next() {
		case chunkenc.ValNone:
			break loop
		case chunkenc.ValHistogram, chunkenc.ValFloatHistogram:
			t, h := buf.AtFloatHistogram()
			if t >= mint {
				out = append(out, promql.Point{T: t, H: h})
			}
		case chunkenc.ValFloat:
			t, v := buf.At()
			if value.IsStaleNaN(v) {
				continue loop
			}
			if !extRange {
				// Values in the buffer are guaranteed to be smaller than maxt.
				if t >= mint {
					out = append(out, promql.Point{T: t, V: v})
				}
			} else {
				// This is the argument to an extended range function: if any point
				// exists at or before range start, add it and then keep replacing
				// it with later points while not yet (strictly) inside the range.
				if t > mint || !appendedPointBeforeMint {
					out = append(out, promql.Point{T: t, V: v})
					appendedPointBeforeMint = true
				} else {
					out[len(out)-1] = promql.Point{T: t, V: v}
				}
			}

		}
	}

	// The sought sample might also be in the range.
	switch soughtValueType {
	case chunkenc.ValHistogram, chunkenc.ValFloatHistogram:
		t, h := it.AtFloatHistogram()
		if t == maxt {
			out = append(out, promql.Point{T: t, H: h})
		}
	case chunkenc.ValFloat:
		t, v := it.At()
		if t == maxt && !value.IsStaleNaN(v) {
			out = append(out, promql.Point{T: t, V: v})
		}
	}

	return out, nil
}
