// Copyright (c) The Thanos Community Authors.
// Licensed under the Apache License 2.0.

package scan

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/prometheus/prometheus/tsdb/chunkenc"

	"github.com/thanos-community/promql-engine/physicalplan/model"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
)

type vectorScanner struct {
	labels    labels.Labels
	signature uint64
	samples   chunkenc.Iterator

	// Fields used to track the previous seen sample.
	// Used for supporting lookback delta.
	pastFirstIteration bool
	hasPrev            bool
	prevTime           int64
	prevValue          float64
}

func (it *vectorScanner) At() (int64, float64) { return it.samples.At() }

func (it *vectorScanner) Seek(ts int64) bool {
	for {
		if it.pastFirstIteration {
			t, v := it.samples.At()
			it.prevTime = t
			it.prevValue = v
			it.hasPrev = true
		}

		if it.samples.Next() {
			it.pastFirstIteration = true
			t, _ := it.samples.At()
			if t >= ts {
				return true
			}
		} else {
			return false
		}
	}
}

// TODO(fpetkovski): Add error handling and max samples limit.
func (it *vectorScanner) selectPoint(ts, lookbackDelta int64) (int64, float64, bool) {
	refTime := ts
	var t int64
	var v float64

	ok := it.Seek(refTime)
	if ok {
		t, v = it.At()
	}

	if !ok || t > refTime {
		t, v, ok = it.prevTime, it.prevValue, it.hasPrev
		if !ok || t < refTime-lookbackDelta {
			return 0, 0, false
		}
	}
	if value.IsStaleNaN(v) {
		return 0, 0, false
	}
	return t, v, true
}

type vectorSelector struct {
	storage  *seriesSelector
	scanners []vectorScanner
	series   []labels.Labels

	once       sync.Once
	vectorPool *model.VectorPool

	mint          int64
	maxt          int64
	step          int64
	currentStep   int64
	stepsBatch    int
	lookbackDelta int64

	shard     int
	numShards int
}

// NewVectorSelector creates operator which selects vector of series.
func NewVectorSelector(
	pool *model.VectorPool,
	selector *seriesSelector,
	mint, maxt time.Time,
	step, lookbackDelta time.Duration,
	stepsBatch,
	shard,
	numShards int,
) model.VectorOperator {
	// TODO(fpetkovski): Add offset parameter.
	return &vectorSelector{
		storage:    selector,
		vectorPool: pool,

		mint:          mint.UnixMilli(),
		maxt:          maxt.UnixMilli(),
		step:          step.Milliseconds(),
		currentStep:   mint.UnixMilli(),
		stepsBatch:    stepsBatch,
		lookbackDelta: lookbackDelta.Milliseconds(),

		shard:     shard,
		numShards: numShards,
	}
}

func (o *vectorSelector) Explain() (me string, next []model.VectorOperator) {
	return fmt.Sprintf("[*vectorSelector] {%v} %v mod %v", o.storage.matchers, o.shard, o.numShards), nil
}

func (o *vectorSelector) Series(ctx context.Context) ([]labels.Labels, error) {
	if err := o.loadSeries(ctx); err != nil {
		return nil, err
	}
	return o.series, nil
}

func (o *vectorSelector) GetPool() *model.VectorPool {
	return o.vectorPool
}

func (o *vectorSelector) Next(ctx context.Context) ([]model.StepVector, error) {
	if o.currentStep > o.maxt {
		return nil, nil
	}

	if err := o.loadSeries(ctx); err != nil {
		return nil, err
	}

	// Instant evaluation is executed as a range evaluation with one step.
	totalSteps := int64(1)
	if o.step != 0 {
		totalSteps = (o.maxt-o.mint)/o.step + 1
	} else {
		// For instant queries, set the step to a positive value
		// so that the operator can terminate.
		o.step = 1
	}
	numSteps := int(math.Min(float64(o.stepsBatch), float64(totalSteps)))

	vectors := o.vectorPool.GetVectorBatch()
	ts := o.currentStep
	for i := 0; i < len(o.scanners); i++ {
		var (
			series   = &o.scanners[i]
			seriesTs = ts
		)

		for currStep := 0; currStep < numSteps && seriesTs <= o.maxt; currStep++ {
			if len(vectors) <= currStep {
				vectors = append(vectors, o.vectorPool.GetStepVector(seriesTs))
			}
			_, v, ok := series.selectPoint(seriesTs, o.lookbackDelta)
			if ok {
				vectors[currStep].SampleIDs = append(vectors[currStep].SampleIDs, series.signature)
				vectors[currStep].Samples = append(vectors[currStep].Samples, v)
			}
			seriesTs += o.step
		}
	}
	o.currentStep += o.step * int64(numSteps)

	return vectors, nil
}

func (o *vectorSelector) loadSeries(ctx context.Context) error {
	var err error
	o.once.Do(func() {
		series, loadErr := o.storage.getSeries(ctx, o.shard, o.numShards)
		if loadErr != nil {
			err = loadErr
			return
		}

		o.scanners = make([]vectorScanner, len(series))
		o.series = make([]labels.Labels, len(series))
		for i, s := range series {
			o.scanners[i] = vectorScanner{
				labels:    s.Labels(),
				signature: s.signature,
				samples:   s.Iterator(),
			}
			o.series[i] = s.Labels()
		}
		o.vectorPool.SetStepSize(len(series))
	})
	return err
}
