package firebase

import (
	"encoding/json"
	"testing"
)

// The Realtime Database hands back a node whose keys are all integers as an
// array with nulls in the gaps, so CoinCounts must decode both shapes. These
// cover the shapes a real `owned` / share snapshot can take.
func TestCoinCountsUnmarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]int
	}{
		{
			name: "object with mixed keys",
			raw:  `{"0":2,"7":1,"148fc368db6d8fe7":4}`,
			want: map[string]int{"0": 2, "7": 1, "148fc368db6d8fe7": 4},
		},
		{
			name: "dense integer keys coerced to an array",
			raw:  `[null,3,1,5]`,
			want: map[string]int{"1": 3, "2": 1, "3": 5},
		},
		{
			name: "array starting at zero keeps a real zero",
			raw:  `[7,0,2]`,
			want: map[string]int{"0": 7, "1": 0, "2": 2},
		},
		{
			name: "empty array",
			raw:  `[]`,
			want: map[string]int{},
		},
		{
			name: "null",
			raw:  `null`,
			want: nil,
		},
		{
			name: "empty object",
			raw:  `{}`,
			want: map[string]int{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var counts CoinCounts
			if err := json.Unmarshal([]byte(tc.raw), &counts); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.raw, err)
			}
			if len(counts) != len(tc.want) {
				t.Fatalf("got %v, want %v", counts, tc.want)
			}
			for coinID, want := range tc.want {
				if counts[coinID] != want {
					t.Errorf("counts[%q] = %d, want %d", coinID, counts[coinID], want)
				}
			}
		})
	}
}

// A zero count is still a count: it must survive decoding rather than be dropped
// as if it were an array gap.
func TestCoinCountsKeepsZeroFromObject(t *testing.T) {
	var counts CoinCounts
	if err := json.Unmarshal([]byte(`{"5":0}`), &counts); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if value, present := counts["5"]; !present || value != 0 {
		t.Fatalf("counts = %v, want {5:0}", counts)
	}
}
