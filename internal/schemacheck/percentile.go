package main

import (
	"fmt"
	"math"
	"sort"
)

// nearestRankPercentile computes the nearest-rank percentile of values: the
// value at 1-based index ceil(percentile/100 * N) in the ascending-sorted
// copy of values. It never mutates the caller's slice and treats an empty
// input as a hard error, since nearest-rank has no index to select from an
// empty list and a zero-value result could be mistaken for a real complexity
// score of 0.
func nearestRankPercentile(values []int, percentile int) (int, error) {
	if len(values) == 0 {
		return 0, fmt.Errorf("compute nearest-rank percentile: empty input")
	}
	sorted := append([]int(nil), values...)
	sort.Ints(sorted)

	rank := int(math.Ceil(float64(percentile) / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1], nil
}
