package cmd

import "testing"

func TestResolveVersion(t *testing.T) {
	cases := []struct {
		name, stamped, module, want string
	}{
		{name: "stamped", stamped: "0.1.0", want: "0.1.0"},
		{name: "stamped with v", stamped: "v0.1.0", want: "0.1.0"},
		{name: "stamped wins over module", stamped: "0.2.0", module: "v0.1.0", want: "0.2.0"},
		{name: "go install", module: "v0.1.0", want: "0.1.0"},
		{name: "pseudo-version", module: "v0.1.1-0.20261108000000-abcdef123456", want: "0.1.1-0.20261108000000-abcdef123456"},
		{name: "devel", module: "(devel)", want: "dev"},
		{name: "nothing", want: "dev"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveVersion(tc.stamped, tc.module); got != tc.want {
				t.Errorf("resolveVersion(%q, %q) = %q, want %q", tc.stamped, tc.module, got, tc.want)
			}
		})
	}
}
