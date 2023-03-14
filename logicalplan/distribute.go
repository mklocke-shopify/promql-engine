// Copyright (c) The Thanos Community Authors.
// Licensed under the Apache License 2.0.

package logicalplan

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/thanos-community/promql-engine/api"
)

type RemoteExecutions []RemoteExecution

func (rs RemoteExecutions) String() string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.String()
	}
	return strings.Join(parts, ", ")
}

// RemoteExecution is a logical plan that describes a
// remote execution of a Query against the given PromQL Engine.
type RemoteExecution struct {
	Engine          api.RemoteEngine
	Query           string
	QueryRangeStart time.Time
}

func (r RemoteExecution) String() string {
	if r.QueryRangeStart.UnixMilli() == 0 {
		return fmt.Sprintf("remote(%s)", r.Query)
	}
	return fmt.Sprintf("remote(%s) [%s]", r.Query, r.QueryRangeStart.String())
}

func (r RemoteExecution) Pretty(level int) string { return r.String() }

func (r RemoteExecution) PositionRange() parser.PositionRange { return parser.PositionRange{} }

func (r RemoteExecution) Type() parser.ValueType { return parser.ValueTypeMatrix }

func (r RemoteExecution) PromQLExpr() {}

// Deduplicate is a logical plan which deduplicates samples from multiple RemoteExecutions.
type Deduplicate struct {
	Expressions RemoteExecutions
}

func (r Deduplicate) String() string {
	return fmt.Sprintf("dedup(%s)", r.Expressions.String())
}

func (r Deduplicate) Pretty(level int) string { return r.String() }

func (r Deduplicate) PositionRange() parser.PositionRange { return parser.PositionRange{} }

func (r Deduplicate) Type() parser.ValueType { return parser.ValueTypeMatrix }

func (r Deduplicate) PromQLExpr() {}

// distributiveAggregations are all PromQL aggregations which support
// distributed execution.
var distributiveAggregations = map[parser.ItemType]struct{}{
	parser.SUM:     {},
	parser.MIN:     {},
	parser.MAX:     {},
	parser.GROUP:   {},
	parser.COUNT:   {},
	parser.BOTTOMK: {},
	parser.TOPK:    {},
}

// DistributedExecutionOptimizer produces a logical plan suitable for
// distributed Query execution.
type DistributedExecutionOptimizer struct {
	Endpoints api.RemoteEndpoints
}

func (m DistributedExecutionOptimizer) Optimize(plan parser.Expr, opts *Opts) parser.Expr {
	engines := m.Endpoints.Engines()
	traverseBottomUp(nil, &plan, func(parent, current *parser.Expr) (stop bool) {
		// If the current operation is not distributive, stop the traversal.
		if !isDistributive(current) {
			return true
		}

		// If the current node is an aggregation, distribute the operation and
		// stop the traversal.
		if aggr, ok := (*current).(*parser.AggregateExpr); ok {
			localAggregation := aggr.Op
			if aggr.Op == parser.COUNT {
				localAggregation = parser.SUM
			}

			remoteAggregation := newRemoteAggregation(aggr, engines)
			subQueries := m.distributeQuery(&remoteAggregation, engines, opts)
			*current = &parser.AggregateExpr{
				Op:       localAggregation,
				Expr:     subQueries,
				Param:    aggr.Param,
				Grouping: aggr.Grouping,
				Without:  aggr.Without,
				PosRange: aggr.PosRange,
			}
			return true
		}

		// If the parent operation is distributive, continue the traversal.
		if isDistributive(parent) {
			return false
		}

		*current = m.distributeQuery(current, engines, opts)
		return true
	})

	return plan
}

func newRemoteAggregation(rootAggregation *parser.AggregateExpr, engines []api.RemoteEngine) parser.Expr {
	groupingSet := make(map[string]struct{})
	for _, lbl := range rootAggregation.Grouping {
		groupingSet[lbl] = struct{}{}
	}

	for _, engine := range engines {
		for _, lbls := range engine.LabelSets() {
			for _, lbl := range lbls {
				if rootAggregation.Without {
					delete(groupingSet, lbl.Name)
				} else {
					groupingSet[lbl.Name] = struct{}{}
				}
			}
		}
	}

	groupingLabels := make([]string, 0, len(groupingSet))
	for lbl := range groupingSet {
		groupingLabels = append(groupingLabels, lbl)
	}
	sort.Strings(groupingLabels)

	remoteAggregation := *rootAggregation
	remoteAggregation.Grouping = groupingLabels
	return &remoteAggregation
}

// distributeQuery takes a PromQL expression in the form of *parser.Expr and a set of remote engines.
// For each engine which matches the time range of the query, it creates a RemoteExecution scoped to the range of the engine.
// All remote executions are wrapped in a Deduplicate logical node to make sure that results from overlapping engines are deduplicated.
// TODO(fpetkovski): Prune remote engines based on external labels.
func (m DistributedExecutionOptimizer) distributeQuery(expr *parser.Expr, engines []api.RemoteEngine, opts *Opts) Deduplicate {
	remoteQueries := make(RemoteExecutions, 0, len(engines))
	for _, e := range engines {
		if e.MaxT() < opts.Start.UnixMilli()-opts.LookbackDelta.Milliseconds() {
			continue
		}
		if e.MinT() > opts.End.UnixMilli() {
			continue
		}

		start := opts.Start
		if e.MinT() > start.UnixMilli() {
			start = calculateStepAlignedStart(e, opts)
		}

		remoteQueries = append(remoteQueries, RemoteExecution{
			Engine:          e,
			Query:           (*expr).String(),
			QueryRangeStart: start,
		})
	}

	return Deduplicate{
		Expressions: remoteQueries,
	}
}

// calculateStepAlignedStart returns a start time for the query based on the
// engine min time and the query step size.
// The purpose of this alignment is to make sure that the steps for the remote query
// have the same timestamps as the ones for the central query.
func calculateStepAlignedStart(engine api.RemoteEngine, opts *Opts) time.Time {
	originalSteps := numSteps(opts.Start, opts.End, opts.Step)
	remoteQuerySteps := numSteps(time.UnixMilli(engine.MinT()), opts.End, opts.Step)

	stepsToSkip := originalSteps - remoteQuerySteps
	stepAlignedStartTime := opts.Start.UnixMilli() + stepsToSkip*opts.Step.Milliseconds()

	return time.UnixMilli(stepAlignedStartTime)
}

func numSteps(start, end time.Time, step time.Duration) int64 {
	return (end.UnixMilli()-start.UnixMilli())/step.Milliseconds() + 1
}

func isDistributive(expr *parser.Expr) bool {
	if expr == nil {
		return false
	}
	switch aggr := (*expr).(type) {
	case *parser.BinaryExpr:
		// Binary expressions are joins and need to be done across the entire
		// data set. This is why we cannot push down aggregations where
		// the operand is a binary expression.
		return false
	case *parser.AggregateExpr:
		// Certain aggregations are currently not supported.
		if _, ok := distributiveAggregations[aggr.Op]; !ok {
			return false
		}
	case *parser.Call:
		return len(aggr.Args) > 0
	}

	return true
}
