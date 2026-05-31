package pypi

// Internal unit tests for upstream.go helpers that aren't exported.
// External tests (upstream_test.go, package pypi_test) cover the
// HTTP-level integration.

import "testing"

func TestEarliestUpload(t *testing.T) {
	mk := func(times ...string) warehouseURLs {
		w := warehouseURLs{}
		for _, ts := range times {
			w.URLs = append(w.URLs, struct {
				UploadTimeISO8601 string `json:"upload_time_iso_8601"`
			}{UploadTimeISO8601: ts})
		}
		return w
	}

	cases := []struct {
		name string
		in   warehouseURLs
		want int64
	}{
		{name: "empty", in: warehouseURLs{}, want: 0},
		{
			name: "one entry RFC3339Nano",
			in:   mk("2024-05-01T00:00:00.123456Z"),
			want: 1714521600,
		},
		{
			name: "picks min of three",
			in: mk(
				"2024-05-05T12:00:00.000000Z",
				"2024-05-01T00:00:00.000000Z",
				"2024-05-03T08:00:00.000000Z",
			),
			want: 1714521600,
		},
		{
			name: "skips garbage entries",
			in: mk(
				"not a date",
				"",
				"2024-05-01T00:00:00.000000Z",
			),
			want: 1714521600,
		},
		{
			name: "all garbage -> 0",
			in:   mk("not a date", ""),
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := earliestUpload(tc.in)
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}
