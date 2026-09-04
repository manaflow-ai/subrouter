package main

import "testing"

func TestPreferredQwenChromeBundleUsesStableThenBetaThenCanary(t *testing.T) {
	for _, test := range []struct {
		name      string
		available map[string]bool
		want      string
	}{
		{"stable", map[string]bool{"com.google.Chrome": true, "com.google.Chrome.beta": true}, "com.google.Chrome"},
		{"beta", map[string]bool{"com.google.Chrome.beta": true, "com.google.Chrome.canary": true}, "com.google.Chrome.beta"},
		{"canary", map[string]bool{"com.google.Chrome.canary": true}, "com.google.Chrome.canary"},
		{"missing", map[string]bool{}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := preferredQwenChromeBundle(func(bundle string) bool { return test.available[bundle] })
			if got != test.want {
				t.Fatalf("preferredQwenChromeBundle() = %q, want %q", got, test.want)
			}
		})
	}
}
