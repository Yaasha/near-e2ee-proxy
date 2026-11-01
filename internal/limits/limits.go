// Package limits shares the SDK body limit between HTTP validation, SSE parsing
// and exact-wire receipt retention. Generation policy belongs to the backend.
package limits

import "math"

// DefaultBodyBytes matches the pinned SDK's default MaxBodyBytes. Operators may
// increase it; local parsers must not impose a smaller independent ceiling.
const DefaultBodyBytes = 64 << 20

func BodyBytes(configured int64) int64 {
	if configured == 0 {
		return DefaultBodyBytes
	}
	return configured
}

// Each of four active operations can retain a request and response. Reserve
// enough receipt capacity for all four pairs without overflowing the SDK budget.
func ValidBodyBytes(value int64) bool {
	return value >= 0 && value <= math.MaxInt64/8
}
