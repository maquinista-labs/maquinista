package relay

import (
	"reflect"
	"testing"
)

func TestParseMentions(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []Mention
	}{
		{
			name: "single",
			in:   "hello [@beta: please review this]",
			want: []Mention{{AgentID: "beta", Text: "please review this"}},
		},
		{
			name: "multiple",
			in:   "[@beta: do X] and [@gamma-42: do Y]",
			want: []Mention{
				{AgentID: "beta", Text: "do X"},
				{AgentID: "gamma-42", Text: "do Y"},
			},
		},
		{
			name: "nested brackets",
			in:   "[@beta: check [file.go:42] please]",
			want: []Mention{{AgentID: "beta", Text: "check [file.go:42] please"}},
		},
		{
			name: "escaped open",
			in:   `look at \[@beta: literal] here`,
			want: nil,
		},
		{
			name: "escaped-escape preserves parse",
			in:   `path is c:\\[@beta: still works]`,
			want: []Mention{{AgentID: "beta", Text: "still works"}},
		},
		{
			name: "no colon -> skipped",
			in:   "[@beta please look]",
			want: nil,
		},
		{
			name: "empty agent id -> skipped",
			in:   "[@: no id]",
			want: nil,
		},
		{
			name: "unterminated -> skipped",
			in:   "[@beta: never closed",
			want: nil,
		},
		{
			name: "adjacent",
			in:   "[@a: 1][@b: 2]",
			want: []Mention{{AgentID: "a", Text: "1"}, {AgentID: "b", Text: "2"}},
		},
		{
			name: "trailing space trimmed",
			in:   "[@a: value   ]",
			want: []Mention{{AgentID: "a", Text: "value"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseMentions(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseMentions(%q)\n got  %#v\n want %#v", tc.in, got, tc.want)
			}
		})
	}
}
