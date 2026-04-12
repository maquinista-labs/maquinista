package bot

import "testing"

func TestParseAgentArg(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"@alpha", "alpha"},
		{"alpha", "alpha"},
		{"  @alpha  ", "alpha"},
		{"@alpha some trailing junk", "alpha"},
		{"@impl-42 review", "impl-42"},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := parseAgentArg(c.in); got != c.want {
			t.Errorf("parseAgentArg(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
