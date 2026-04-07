package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseDiskPaths(t *testing.T) {
	got := parseDiskPaths(" /, /tmp, var, /tmp, ")
	want := []string{"/", "/tmp", "/var"}

	if len(got) != len(want) {
		t.Fatalf("len mismatch got=%d want=%d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("value mismatch at %d: got=%q want=%q", i, got[i], want[i])
		}
	}
}

func TestDiskMetricKey(t *testing.T) {
	tests := map[string]string{
		"/":    "disk:/",
		"/tmp": "disk:/tmp",
		"/var": "disk:/var",
	}
	for in, want := range tests {
		if got := diskMetricKey(in); got != want {
			t.Fatalf("diskMetricKey(%q)=%q want=%q", in, got, want)
		}
	}
}

func TestInodeMetricKey(t *testing.T) {
	tests := map[string]string{
		"/":    "inode:/",
		"/tmp": "inode:/tmp",
	}
	for in, want := range tests {
		if got := inodeMetricKey(in); got != want {
			t.Fatalf("inodeMetricKey(%q)=%q want=%q", in, got, want)
		}
	}
}

func TestDiskIOKey(t *testing.T) {
	tests := map[string][3]string{
		"/":    {"iops:/", "throughput:/:rx", "throughput:/:tx"},
		"/tmp": {"iops:/tmp", "throughput:/tmp:rx", "throughput:/tmp:tx"},
	}
	for in, want := range tests {
		if got := iopsMetricKey(in); got != want[0] {
			t.Fatalf("iopsMetricKey(%q)=%q want=%q", in, got, want[0])
		}
		if got := throughputMetricKey(in, "rx"); got != want[1] {
			t.Fatalf("throughputMetricKey(%q,\"rx\")=%q want=%q", in, got, want[1])
		}
		if got := throughputMetricKey(in, "tx"); got != want[2] {
			t.Fatalf("throughputMetricKey(%q,\"tx\")=%q want=%q", in, got, want[2])
		}
	}
}

func TestLVMMetricKey(t *testing.T) {
	if got := lvmThinDataMetricKey("vg0", "thinpool"); got != "lvm:data:vg0/thinpool" {
		t.Fatalf("unexpected lvm data key: %s", got)
	}
	if got := lvmThinMetaMetricKey("vg0", "thinpool"); got != "lvm:meta:vg0/thinpool" {
		t.Fatalf("unexpected lvm meta key: %s", got)
	}
}

func TestNetMetricKeys(t *testing.T) {
	if got := netBytesMetricKey("eth0", "rx"); got != "net:eth0:bytes:rx" {
		t.Fatalf("unexpected bytes key: %s", got)
	}
	if got := netBytesMetricKey("eth0", "tx"); got != "net:eth0:bytes:tx" {
		t.Fatalf("unexpected bytes key: %s", got)
	}
	if got := netPacketsMetricKey("eth0", "rx"); got != "net:eth0:packets:rx" {
		t.Fatalf("unexpected packets key: %s", got)
	}
	if got := netPacketsMetricKey("eth0", "tx"); got != "net:eth0:packets:tx" {
		t.Fatalf("unexpected packets key: %s", got)
	}
}

func TestNormalizePushURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		base    string
		want    string
		wantErr bool
	}{
		{
			name: "uuid-expands-default-style",
			raw:  "a1c6859d-ec5f-4fc1-b2ef-cf2cd2bced08",
			base: "https://push.netquirk.com",
			want: "https://push.netquirk.com/a1c6859d-ec5f-4fc1-b2ef-cf2cd2bced08",
		},
		{
			name: "uuid-expands-custom-base",
			raw:  "a1c6859d-ec5f-4fc1-b2ef-cf2cd2bced08",
			base: "https://push.example.com/",
			want: "https://push.example.com/a1c6859d-ec5f-4fc1-b2ef-cf2cd2bced08",
		},
		{
			name:    "uuid-with-slash-rejected",
			raw:     "foo/bar",
			base:    "https://push.netquirk.com",
			wantErr: true,
		},
		{
			name:    "full-url-rejected",
			raw:     "https://push.netquirk.com/a1c6859d-ec5f-4fc1-b2ef-cf2cd2bced08",
			base:    "https://push.netquirk.com",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizePushURL(tt.raw, tt.base)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got none (got=%q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("normalizePushURL(%q,%q)=%q want=%q", tt.raw, tt.base, got, tt.want)
			}
		})
	}
}

func TestPercentToUint64(t *testing.T) {
	tests := []struct {
		in   float64
		want uint64
	}{
		{in: -1, want: 0},
		{in: 0, want: 0},
		{in: 0.49, want: 0},
		{in: 0.5, want: 1},
		{in: 49.4, want: 49},
		{in: 49.5, want: 50},
		{in: 100, want: 100},
		{in: 100.1, want: 100},
	}

	for _, tt := range tests {
		got := percentToUint64(tt.in)
		if got != tt.want {
			t.Fatalf("percentToUint64(%v)=%d want=%d", tt.in, got, tt.want)
		}
	}
}

func TestPushPayloadMetricsMarshalAsIntegers(t *testing.T) {
	payload := pushPayload{
		AgentVersion: 1,
		Timestamp:    1775340000,
		Metrics: map[string]uint64{
			"cpu:user": 12,
			"inode:/":  4930021,
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	s := string(b)
	if strings.Contains(s, ".") {
		t.Fatalf("expected integer-only JSON values, got %s", s)
	}
}

func TestParseLVMPercentToken(t *testing.T) {
	tests := []struct {
		in     string
		want   uint64
		wantOK bool
	}{
		{in: "45.2", want: 45, wantOK: true},
		{in: "45.6", want: 46, wantOK: true},
		{in: "100.0", want: 100, wantOK: true},
		{in: "-", want: 0, wantOK: false},
		{in: "", want: 0, wantOK: false},
		{in: "x", want: 0, wantOK: false},
	}
	for _, tt := range tests {
		got, ok := parseLVMPercentToken(tt.in)
		if ok != tt.wantOK {
			t.Fatalf("parseLVMPercentToken(%q) ok=%v want=%v", tt.in, ok, tt.wantOK)
		}
		if got != tt.want {
			t.Fatalf("parseLVMPercentToken(%q)=%d want=%d", tt.in, got, tt.want)
		}
	}
}

func TestSkipTemplateLVName(t *testing.T) {
	if !strings.HasPrefix("tpl_pool0", "tpl_") {
		t.Fatalf("expected tpl_ prefix detection to be true")
	}
	if strings.HasPrefix("thinpool", "tpl_") {
		t.Fatalf("expected non-template LV name to not match tpl_ prefix")
	}
}
