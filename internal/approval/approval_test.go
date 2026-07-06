package approval

import "testing"

func TestResolved(t *testing.T) {
	cases := []struct {
		d    Decision
		want bool
	}{
		{Pending, false},
		{"", false}, // zero value: not yet registered, still not final
		{Approved, true},
		{Rejected, true},
		{Timeout, true},
	}
	for _, c := range cases {
		if got := c.d.Resolved(); got != c.want {
			t.Errorf("Decision(%q).Resolved() = %v, want %v", c.d, got, c.want)
		}
	}
}
