// Package ecs implements the Dell ECS management-plane client used by
// ecs-phoenix-ext: login/token cache, namespace billing poll, JSON+XML parsing
// and binary unit conversion.
package ecs

import (
	"fmt"
	"math"
	"strings"
)

// ToBytes converts a magnitude in the given ECS unit to bytes using the
// binary table (Dell KB 000273649: API "GB" means GiB). Unknown units are
// an error — never guess decimal.
func ToBytes(n float64, unit string) (int64, error) {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("ecs: invalid magnitude %v", n)
	}
	var mul float64
	switch strings.ToUpper(strings.TrimSpace(unit)) {
	case "B", "BYTE", "BYTES":
		mul = 1
	case "KB", "KIB":
		mul = 1024
	case "MB", "MIB":
		mul = 1024 * 1024
	case "GB", "GIB":
		mul = 1024 * 1024 * 1024
	case "TB", "TIB":
		mul = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("ecs: unknown size unit %q", unit)
	}
	v := n * mul
	if v > math.MaxInt64 || v < math.MinInt64 {
		return 0, fmt.Errorf("ecs: magnitude %v %s overflows int64 bytes", n, unit)
	}
	return int64(math.Round(v)), nil
}
