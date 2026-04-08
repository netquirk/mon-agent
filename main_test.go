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

func TestDiskIOVecKey(t *testing.T) {
	tests := map[string]string{
		"/":    "vec_disk_/",
		"/tmp": "vec_disk_/tmp",
	}
	for in, want := range tests {
		if got := diskVecMetricKey(in); got != want {
			t.Fatalf("diskVecMetricKey(%q)=%q want=%q", in, got, want)
		}
	}
}

func TestLVMMetricKey(t *testing.T) {
	if got := lvmPackedMetricKey("vg0", "thinpool"); got != "pack2_lvm_vg0/thinpool" {
		t.Fatalf("unexpected lvm packed key: %s", got)
	}
}

func TestDiskInodePackedMetricKey(t *testing.T) {
	if got := diskInodePackedMetricKey("/"); got != "pack2_disk_/" {
		t.Fatalf("unexpected disk/inode pack2 key: %s", got)
	}
	if got := diskInodePackedMetricKey("/tmp"); got != "pack2_disk_/tmp" {
		t.Fatalf("unexpected disk/inode pack2 key: %s", got)
	}
}

func TestNetMetricKeys(t *testing.T) {
	if got := netVecMetricKey("eth0"); got != "vec_net_eth0" {
		t.Fatalf("unexpected vec net key: %s", got)
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

func TestPercentToScaled100Uint64(t *testing.T) {
	tests := []struct {
		in   float64
		want uint64
	}{
		{in: -1, want: 0},
		{in: 0, want: 0},
		{in: 0.001, want: 0},
		{in: 0.01, want: 1},
		{in: 1.23, want: 123},
		{in: 49.995, want: 5000},
		{in: 100, want: 10000},
		{in: 101.5, want: 10150},
		{in: 250.75, want: 25075},
	}
	for _, tt := range tests {
		got := percentToScaled100Uint64(tt.in)
		if got != tt.want {
			t.Fatalf("percentToScaled100Uint64(%v)=%d want=%d", tt.in, got, tt.want)
		}
	}
}

func TestPackU16x4(t *testing.T) {
	got := packU16x4(100, 200, 300, 400)
	want := uint64(100) | (uint64(200) << 16) | (uint64(300) << 32) | (uint64(400) << 48)
	if got != want {
		t.Fatalf("packU16x4 mismatch got=%d want=%d", got, want)
	}
}

func TestPackU16x4Clamps(t *testing.T) {
	got := packU16x4(70000, 1, 2, 3)
	want := uint64(0xffff) | (uint64(1) << 16) | (uint64(2) << 32) | (uint64(3) << 48)
	if got != want {
		t.Fatalf("packU16x4 clamp mismatch got=%d want=%d", got, want)
	}
}

func TestPackU32x2(t *testing.T) {
	got := packU32x2(1000, 2000)
	want := uint64(1000) | (uint64(2000) << 32)
	if got != want {
		t.Fatalf("packU32x2 mismatch got=%d want=%d", got, want)
	}
}

func TestPackU32x2Clamps(t *testing.T) {
	got := packU32x2(^uint64(0), 1)
	want := uint64(0xffff_ffff) | (uint64(1) << 32)
	if got != want {
		t.Fatalf("packU32x2 clamp mismatch got=%d want=%d", got, want)
	}
}

func TestPushPayloadMetricsMarshalAsIntegers(t *testing.T) {
	payload := pushPayload{
		AgentVersion: (1 << 42) | 1,
		Timestamp:    1775340000,
		Metrics:      map[string]json.RawMessage{},
	}
	addUint64Metric(payload.Metrics, "cpu:user", 12)
	addUint64Metric(payload.Metrics, "inode:/", 4930021)
	addUint64ArrayMetric(payload.Metrics, "vec_net_eth0", []uint64{100, 200, 10, 20})
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	s := string(b)
	if strings.Contains(s, ".") {
		t.Fatalf("expected integer-only JSON values, got %s", s)
	}
	if !strings.Contains(s, "\"vec_net_eth0\":[100,200,10,20]") {
		t.Fatalf("expected 4-lane net metric in payload, got %s", s)
	}
}

func TestEncodeSemverVersion(t *testing.T) {
	got, err := encodeSemverVersion("1.2.3")
	if err != nil {
		t.Fatalf("encodeSemverVersion failed: %v", err)
	}
	want := (uint64(1) << 42) | (uint64(2) << 21) | uint64(3)
	if got != want {
		t.Fatalf("unexpected encoding: got=%d want=%d", got, want)
	}

	got, err = encodeSemverVersion("v10.20.30")
	if err != nil {
		t.Fatalf("encodeSemverVersion with v-prefix failed: %v", err)
	}
	want = (uint64(10) << 42) | (uint64(20) << 21) | uint64(30)
	if got != want {
		t.Fatalf("unexpected v-prefix encoding: got=%d want=%d", got, want)
	}
}

func TestEncodeSemverVersionRejectsInvalid(t *testing.T) {
	invalid := []string{"", "1", "1.2", "dev", "1.2.x", "0.1.2", "2097152.1.1"}
	for _, input := range invalid {
		if _, err := encodeSemverVersion(input); err == nil {
			t.Fatalf("expected error for %q", input)
		}
	}
}

func TestParseLVMPercentToken(t *testing.T) {
	tests := []struct {
		in     string
		want   uint64
		wantOK bool
	}{
		{in: "45.2", want: 4520, wantOK: true},
		{in: "45.6", want: 4560, wantOK: true},
		{in: "45.67", want: 4567, wantOK: true},
		{in: "100.0", want: 10000, wantOK: true},
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
