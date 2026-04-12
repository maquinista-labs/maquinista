package bot

import "testing"

func TestSplitQuoted(t *testing.T) {
	cases := []struct {
		in     string
		want   []string
		errOK  bool
	}{
		{`a b c`, []string{"a", "b", "c"}, false},
		{`name "0 8 * * *" @agent "/run"`, []string{"name", "0 8 * * *", "@agent", "/run"}, false},
		{`one "two three"`, []string{"one", "two three"}, false},
		{`   trim   spaces  `, []string{"trim", "spaces"}, false},
		{`"unterminated`, nil, true},
	}
	for _, c := range cases {
		got, err := splitQuoted(c.in)
		if (err != nil) != c.errOK {
			t.Errorf("splitQuoted(%q) errOK=%v got err=%v", c.in, c.errOK, err)
			continue
		}
		if !c.errOK && !stringSliceEq(got, c.want) {
			t.Errorf("splitQuoted(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func stringSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
