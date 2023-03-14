package parse

import (
	"github.com/prometheus/prometheus/promql/parser"
)

var Functions = map[string]*parser.Function{
	"xdelta": {
		Name:       "xdelta",
		ArgTypes:   []parser.ValueType{parser.ValueTypeMatrix},
		ReturnType: parser.ValueTypeVector,
	},
	"xincrease": {
		Name:       "xincrease",
		ArgTypes:   []parser.ValueType{parser.ValueTypeMatrix},
		ReturnType: parser.ValueTypeVector,
	},
	"xrate": {
		Name:       "xrate",
		ArgTypes:   []parser.ValueType{parser.ValueTypeMatrix},
		ReturnType: parser.ValueTypeVector,
	},
}