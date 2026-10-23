package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseDiskThreshold_Valid(t *testing.T) {
	cases := []struct {
		in   string
		want DiskThreshold
	}{
		{"1%", DiskThreshold{Percent: 1, Set: true}},
		{"0.5%", DiskThreshold{Percent: 0.5, Set: true}},
		{"1.5 %", DiskThreshold{Percent: 1.5, Set: true}},
		{"  2%  ", DiskThreshold{Percent: 2, Set: true}},
		{"99.9%", DiskThreshold{Percent: 99.9, Set: true}},
		{"10GiB", DiskThreshold{Bytes: 10 << 30, Set: true}},
		{"10 GiB", DiskThreshold{Bytes: 10 << 30, Set: true}},
		{"500MB", DiskThreshold{Bytes: 500_000_000, Set: true}},
		{"10Gi", DiskThreshold{Bytes: 10 << 30, Set: true}},
		{"5G", DiskThreshold{Bytes: 5_000_000_000, Set: true}},
		{"2TiB", DiskThreshold{Bytes: 2 << 40, Set: true}},
		{"1.5Gi", DiskThreshold{Bytes: 3 << 29, Set: true}},
		{"5368709120", DiskThreshold{Bytes: 5368709120, Set: true}},
		{" 5368709120 ", DiskThreshold{Bytes: 5368709120, Set: true}},
		{"0", DiskThreshold{Set: true, Off: true}},
		{"off", DiskThreshold{Set: true, Off: true}},
		{"OFF", DiskThreshold{Set: true, Off: true}},
		{"disabled", DiskThreshold{Set: true, Off: true}},
		{" disabled ", DiskThreshold{Set: true, Off: true}},
		{"", DiskThreshold{}},
		{"   ", DiskThreshold{}},
	}
	for _, c := range cases {
		got, err := ParseDiskThreshold(c.in)
		if err != nil {
			t.Errorf("ParseDiskThreshold(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDiskThreshold(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestParseDiskThreshold_Invalid(t *testing.T) {
	for _, in := range []string{
		"-1", "0%", "100%", "101%", "abc", "1.2.3%", "10XB",
		"99999999999999999999", "99999999999999999999Gi", "-5%", "%", "nan%", "inf%",
		"5m", "1e3", "10 %%", "GiB", "10G B",
	} {
		if got, err := ParseDiskThreshold(in); err == nil {
			t.Errorf("ParseDiskThreshold(%q) = %+v, want error", in, got)
		}
	}
}

func TestDiskThreshold_Resolve(t *testing.T) {
	const total = uint64(1) << 40
	if got, want := (DiskThreshold{Percent: 1, Set: true}).Resolve(total), total/100; got != want {
		t.Errorf("1%% of 1TiB = %d, want %d", got, want)
	}
	if got := (DiskThreshold{Bytes: 123, Set: true}).Resolve(total); got != 123 {
		t.Errorf("absolute Resolve = %d, want 123", got)
	}
	if got := (DiskThreshold{Bytes: 123, Set: true}).Resolve(0); got != 123 {
		t.Errorf("absolute Resolve(0) = %d, want 123", got)
	}
	if got := (DiskThreshold{Set: true, Off: true}).Resolve(total); got != 0 {
		t.Errorf("Off Resolve = %d, want 0", got)
	}
	if got := (DiskThreshold{Percent: 1, Set: true}).Resolve(0); got != 0 {
		t.Errorf("percent Resolve(0) = %d, want 0", got)
	}
}

func TestDiskThreshold_Below(t *testing.T) {
	pct := DiskThreshold{Percent: 1, Set: true}
	abs := DiskThreshold{Bytes: 1000, Set: true}
	off := DiskThreshold{Set: true, Off: true}
	cases := []struct {
		name        string
		th          DiskThreshold
		free, total uint64
		want        bool
	}{
		{"pct below", pct, 9, 1000, true},
		{"pct exactly at threshold", pct, 10, 1000, false},
		{"pct above", pct, 11, 1000, false},
		{"pct total zero fails open", pct, 0, 0, false},
		{"abs below", abs, 999, 5000, true},
		{"abs exactly at threshold", abs, 1000, 5000, false},
		{"abs total zero still judged", abs, 999, 0, true},
		{"off never below", off, 0, 1000, false},
		{"unset never below", DiskThreshold{}, 0, 1000, false},
	}
	for _, c := range cases {
		if got := c.th.Below(c.free, c.total); got != c.want {
			t.Errorf("%s: Below(%d,%d) = %v, want %v", c.name, c.free, c.total, got, c.want)
		}
	}
}

func TestDiskThreshold_DefaultAndOrDefault(t *testing.T) {
	d := DefaultDiskThreshold()
	if d.Percent != 1 || d.Set || d.Off || d.Bytes != 0 {
		t.Errorf("Default = %+v, want {Percent:1}", d)
	}
	if got := (DiskThreshold{}).OrDefault(); got != d {
		t.Errorf("unset OrDefault = %+v, want %+v", got, d)
	}
	off := DiskThreshold{Set: true, Off: true}
	if got := off.OrDefault(); got != off {
		t.Errorf("explicit off OrDefault = %+v, want unchanged", got)
	}
	abs := DiskThreshold{Bytes: 7, Set: true}
	if got := abs.OrDefault(); got != abs {
		t.Errorf("explicit bytes OrDefault = %+v, want unchanged", got)
	}
}

func TestDiskThreshold_StringRoundTrip(t *testing.T) {
	for _, in := range []string{"1%", "0.5%", "1.5%", "10GiB", "500MB", "5368709120", "0", "off", "", "0.00001%", "0.000001%"} {
		th, err := ParseDiskThreshold(in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		back, err := ParseDiskThreshold(th.String())
		if err != nil {
			t.Fatalf("re-parse %q (from %q): %v", th.String(), in, err)
		}
		if back != th {
			t.Errorf("round trip %q -> %q -> %+v, want %+v", in, th.String(), back, th)
		}
	}
	// A tiny percentage must not render in exponent form ("1e-05%"): String()
	// feeds a flag default that main re-parses.
	if got := (DiskThreshold{Percent: 0.00001, Set: true}).String(); got != "0.00001%" {
		t.Errorf("tiny percent String = %q, want 0.00001%%", got)
	}
	if got := (DiskThreshold{Percent: 1}).String(); got != "1%" {
		t.Errorf("Default String = %q, want 1%%", got)
	}
	if got := (DiskThreshold{Set: true, Off: true}).String(); got != "off" {
		t.Errorf("off String = %q", got)
	}
}

type diskThresholdHolder struct {
	MinFreeDisk DiskThreshold `yaml:"minFreeDisk"`
}

func decodeStrict(doc string, out any) error {
	dec := yaml.NewDecoder(strings.NewReader(doc))
	dec.KnownFields(true)
	return dec.Decode(out)
}

func TestDiskThreshold_UnmarshalYAML(t *testing.T) {
	cases := []struct {
		doc  string
		want DiskThreshold
	}{
		{"minFreeDisk: 5368709120", DiskThreshold{Bytes: 5368709120, Set: true}},
		{`minFreeDisk: "5Gi"`, DiskThreshold{Bytes: 5 << 30, Set: true}},
		{`minFreeDisk: 5Gi`, DiskThreshold{Bytes: 5 << 30, Set: true}},
		{`minFreeDisk: "1%"`, DiskThreshold{Percent: 1, Set: true}},
		{`minFreeDisk: 1%`, DiskThreshold{Percent: 1, Set: true}},
		{`minFreeDisk: 0`, DiskThreshold{Set: true, Off: true}},
		{`minFreeDisk: "0"`, DiskThreshold{Set: true, Off: true}},
		{`minFreeDisk: off`, DiskThreshold{Set: true, Off: true}},
		{`minFreeDisk: disabled`, DiskThreshold{Set: true, Off: true}},
		{`other: 1`, DiskThreshold{}},
		{`{}`, DiskThreshold{}},
		{`minFreeDisk:`, DiskThreshold{}},
		{`minFreeDisk: ""`, DiskThreshold{}},
	}
	for _, c := range cases {
		var h diskThresholdHolder
		doc := c.doc
		var err error
		if strings.HasPrefix(doc, "other") {
			var o struct {
				MinFreeDisk DiskThreshold `yaml:"minFreeDisk"`
				Other       int           `yaml:"other"`
			}
			err = decodeStrict(doc, &o)
			h.MinFreeDisk = o.MinFreeDisk
		} else {
			err = decodeStrict(doc, &h)
		}
		if err != nil {
			t.Errorf("%q: %v", c.doc, err)
			continue
		}
		if h.MinFreeDisk != c.want {
			t.Errorf("%q: got %+v, want %+v", c.doc, h.MinFreeDisk, c.want)
		}
	}
}

func TestDiskThreshold_UnmarshalYAML_Errors(t *testing.T) {
	for _, doc := range []string{
		"minFreeDisk: {a: 1}",
		"minFreeDisk: [1, 2]",
		"minFreeDisk: -1",
		"minFreeDisk: 100%",
		"minFreeDisk: 1.5",
		"minFreeDisk: abc",
		"minFreeDisk: true",
	} {
		var h diskThresholdHolder
		if err := decodeStrict(doc, &h); err == nil {
			t.Errorf("%q: want error, got %+v", doc, h.MinFreeDisk)
		}
	}
}

func TestDiskThreshold_StrictModeStillRejectsUnknownFields(t *testing.T) {
	var h diskThresholdHolder
	err := decodeStrict("minFreeDisk: 1%\nbogus: 1\n", &h)
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("want unknown-field error mentioning bogus, got %v", err)
	}
}
