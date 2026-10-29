package config

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

// DiskThreshold is the minimum free-disk headroom an agent requires before it
// claims work. It is either a percentage of the total size of the filesystem
// being checked, or an absolute number of bytes, or switched off.
//
// The zero value means "unset": the field was not configured, and callers
// should use OrDefault to obtain the effective value. An explicit 0 / "off" /
// "disabled" is Set with Off true, so it can be told apart from unset and
// override a non-zero default.
type DiskThreshold struct {
	// Percent is the threshold as a percentage of the filesystem total
	// (0 < Percent < 100). Used when Bytes is 0 and the threshold is not Off.
	Percent float64
	// Bytes is an absolute threshold. When non-zero it takes precedence over Percent.
	Bytes uint64
	// Set reports that the value was configured explicitly.
	Set bool
	// Off reports that the check is explicitly disabled.
	Off bool
}

// DefaultDiskThreshold returns the default threshold: 1% of the filesystem
// total. It is not Set; use OrDefault to apply it to a possibly-unset value.
func DefaultDiskThreshold() DiskThreshold {
	return DiskThreshold{Percent: 1, Set: false}
}

// OrDefault returns t if it was explicitly configured (including "off"), and
// the default threshold otherwise.
func (t DiskThreshold) OrDefault() DiskThreshold {
	if t.Set {
		return t
	}
	return DefaultDiskThreshold()
}

var (
	diskDigitsRE  = regexp.MustCompile(`^[0-9]+$`)
	diskDecimalRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	diskSizeRE    = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([A-Za-z]*)$`)
)

// diskQuantitySuffixes are the suffixes accepted by resource.ParseQuantity
// that denote byte multiples (IEC and SI). Milli ("m") and exponent forms are
// deliberately not accepted.
var diskQuantitySuffixes = map[string]bool{
	"K": true, "k": true, "M": true, "G": true, "T": true, "P": true, "E": true,
	"Ki": true, "Mi": true, "Gi": true, "Ti": true, "Pi": true, "Ei": true,
}

// ParseDiskThreshold parses a minFreeDisk value. Accepted forms: "1%", "0.5%",
// "1.5 %" (percent, 0 < p < 100); "10GiB", "500MB", "10Gi", "5G" (absolute,
// IEC and SI); "5368709120" (absolute bytes); "0", "off", "disabled" (check
// disabled); and "" (unset).
func ParseDiskThreshold(s string) (DiskThreshold, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "":
		return DiskThreshold{}, nil
	case "off", "disabled":
		return DiskThreshold{Set: true, Off: true}, nil
	}

	if num, ok := strings.CutSuffix(s, "%"); ok {
		num = strings.TrimSpace(num)
		if !diskDecimalRE.MatchString(num) {
			return DiskThreshold{}, fmt.Errorf("invalid minFreeDisk percentage %q", s)
		}
		p, err := strconv.ParseFloat(num, 64)
		if err != nil || math.IsNaN(p) || math.IsInf(p, 0) {
			return DiskThreshold{}, fmt.Errorf("invalid minFreeDisk percentage %q", s)
		}
		if p <= 0 || p >= 100 {
			return DiskThreshold{}, fmt.Errorf("minFreeDisk percentage %q must be greater than 0%% and less than 100%% (use \"off\" to disable the check)", s)
		}
		return DiskThreshold{Percent: p, Set: true}, nil
	}

	m := diskSizeRE.FindStringSubmatch(s)
	if m == nil {
		return DiskThreshold{}, fmt.Errorf("invalid minFreeDisk %q: want a percentage (\"1%%\"), a size (\"10GiB\"), a byte count, or \"off\"", s)
	}
	num, suffix := m[1], m[2]

	var n uint64
	switch {
	case suffix == "" || suffix == "B":
		if !diskDigitsRE.MatchString(num) {
			return DiskThreshold{}, fmt.Errorf("invalid minFreeDisk %q: a byte count must be a whole number", s)
		}
		v, err := strconv.ParseUint(num, 10, 64)
		if err != nil {
			return DiskThreshold{}, fmt.Errorf("invalid minFreeDisk %q: %w", s, err)
		}
		n = v
	default:
		unit := suffix
		if base, ok := strings.CutSuffix(suffix, "B"); ok && diskQuantitySuffixes[base] {
			unit = base
		}
		if !diskQuantitySuffixes[unit] {
			return DiskThreshold{}, fmt.Errorf("invalid minFreeDisk %q: unknown size suffix %q", s, suffix)
		}
		q, err := resource.ParseQuantity(num + unit)
		if err != nil {
			return DiskThreshold{}, fmt.Errorf("invalid minFreeDisk %q: %w", s, err)
		}
		if q.Sign() < 0 || q.Cmp(*resource.NewQuantity(math.MaxInt64, resource.DecimalSI)) >= 0 {
			return DiskThreshold{}, fmt.Errorf("invalid minFreeDisk %q: out of range", s)
		}
		v := q.Value() // rounds a fractional byte count up
		n = uint64(v)
	}

	if n == 0 {
		return DiskThreshold{Set: true, Off: true}, nil
	}
	return DiskThreshold{Bytes: n, Set: true}, nil
}

// Resolve returns the threshold in bytes for a filesystem of the given total
// size. An absolute threshold ignores total. A percentage is rounded down. Off
// and unset thresholds resolve to 0, as does a percentage when total is 0.
func (t DiskThreshold) Resolve(total uint64) uint64 {
	if t.Off {
		return 0
	}
	if t.Bytes != 0 {
		return t.Bytes
	}
	if t.Percent <= 0 {
		return 0
	}
	return uint64(float64(total) * t.Percent / 100)
}

// Below reports whether free bytes are strictly below the threshold, matching
// the existing workspace gate (free < min). An Off or unset threshold is never
// below, and a percentage threshold with total == 0 is never below because the
// agent cannot judge it and fails open.
func (t DiskThreshold) Below(free, total uint64) bool {
	if t.Off || t.Bytes == 0 && t.Percent <= 0 {
		return false
	}
	if t.Bytes == 0 && total == 0 {
		return false
	}
	return free < t.Resolve(total)
}

// String renders the threshold in a form ParseDiskThreshold accepts. An unset
// threshold renders as the empty string.
func (t DiskThreshold) String() string {
	switch {
	case t.Off:
		return "off"
	case t.Bytes != 0:
		return strconv.FormatUint(t.Bytes, 10)
	case t.Percent > 0:
		return strconv.FormatFloat(t.Percent, 'f', -1, 64) + "%"
	default:
		return ""
	}
}

// UnmarshalYAML accepts an integer or string scalar. Mappings and sequences
// are rejected. An absent field never reaches this method, so it stays unset.
func (t *DiskThreshold) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: minFreeDisk must be a scalar (a percentage, a size, a byte count or \"off\")", node.Line)
	}
	switch node.Tag {
	case "!!null":
		*t = DiskThreshold{}
		return nil
	case "!!int", "!!str":
	default:
		return fmt.Errorf("line %d: minFreeDisk must be a string or an integer, got %s %q", node.Line, node.Tag, node.Value)
	}
	v, err := ParseDiskThreshold(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", node.Line, err)
	}
	*t = v
	return nil
}
