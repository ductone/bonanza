package analysis

import "testing"

func TestActionExecutionRequirementsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name         string
		tags         []string
		requirements map[string]string
		shellEnv     bool
		reject       bool
	}{
		{name: "ordinary image layer", tags: []string{"manual"}},
		{name: "image source archive", requirements: map[string]string{"local": "1", "no-remote": "1", "no-sandbox": "1"}, reject: true},
		{name: "uncached generator action", requirements: map[string]string{"no-cache": "1"}, reject: true},
		{name: "unsandboxed image action", requirements: map[string]string{"no-sandbox": "1"}, reject: true},
		{name: "tagged generator action", tags: []string{"exclusive", "external", "local", "no-cache", "no-sandbox"}, reject: true},
		{name: "noncacheable tagged generator", tags: []string{"no-cache"}, reject: true},
		{name: "inherited host environment", shellEnv: true, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateActionExecutionRequirements(tc.tags, tc.requirements, tc.shellEnv)
			if (err != nil) != tc.reject {
				t.Fatalf("action rejected = %t (error: %v), want %t", err != nil, err, tc.reject)
			}
		})
	}
}
