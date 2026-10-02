package gh

import "testing"

func TestParseChecks(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		want string
	}{
		{"no checks", `{"statusCheckRollup":[]}`, "none"},
		{"all green", `{"statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"}]}`, "green"},
		{"neutral skipped count as green", `{"statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"COMPLETED","conclusion":"SKIPPED"}]}`, "green"},
		{"in progress", `{"statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"IN_PROGRESS"}]}`, "pending"},
		{"failure wins over pending", `{"statusCheckRollup":[{"status":"IN_PROGRESS"},{"status":"COMPLETED","conclusion":"FAILURE"}]}`, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseChecks([]byte(tc.json))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("parseChecks = %q, want %q", got, tc.want)
			}
		})
	}
}
