package main

import (
	"math"
	"strings"
)

func float64ToBits(v float64) uint64   { return math.Float64bits(v) }
func float64FromBits(b uint64) float64 { return math.Float64frombits(b) }

func joinPath(base, extra string) string {
	if extra == "" {
		return base
	}
	base = strings.TrimSuffix(base, "/")
	if !strings.HasPrefix(extra, "/") {
		extra = "/" + extra
	}
	return base + extra
}
