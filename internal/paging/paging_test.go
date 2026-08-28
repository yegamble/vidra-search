package paging

import "testing"

func TestLimit(t *testing.T) {
	const (
		def = 20
		max = 200
	)
	cases := []struct {
		name string
		v    int
		want int
	}{
		// "Nothing usable" is one answer, not three: a negative limit is not a
		// request for one row, and it is not an error either — it is a caller who
		// did not choose, which is what the default is for.
		{"negative", -5, def},
		{"very negative", -1 << 30, def},
		{"zero", 0, def},
		{"one", 1, 1},
		{"under max", 50, 50},
		{"exactly max", max, max},
		{"over max", max + 1, max},
		{"absurd", 1 << 30, max},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Limit(tc.v, def, max); got != tc.want {
				t.Errorf("Limit(%d, %d, %d) = %d, want %d", tc.v, def, max, got, tc.want)
			}
		})
	}
}

func TestOffset(t *testing.T) {
	cases := []struct {
		name string
		v    int
		want int
	}{
		{"negative", -1, 0},
		{"very negative", -1 << 30, 0},
		{"zero", 0, 0},
		{"positive", 40, 40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Offset(tc.v); got != tc.want {
				t.Errorf("Offset(%d) = %d, want %d", tc.v, got, tc.want)
			}
		})
	}
}
