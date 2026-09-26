package main

import "testing"

func TestParseGoldenLocalDaemonListenAddressRequiresOneOwnedLoopbackSocket(t *testing.T) {
	address, err := parseGoldenLocalDaemonListenAddress([]byte("p123\nf7\nn127.0.0.1:43123\nTST=LISTEN\n"))
	if err != nil || address != "127.0.0.1:43123" {
		t.Fatalf("address = %q, error = %v", address, err)
	}
	for _, test := range []struct {
		name   string
		output string
	}{
		{name: "public listener", output: "n0.0.0.0:43123\n"},
		{name: "zero port", output: "n127.0.0.1:0\n"},
		{name: "multiple listeners", output: "n127.0.0.1:43123\nn127.0.0.1:43124\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseGoldenLocalDaemonListenAddress([]byte(test.output)); err == nil {
				t.Fatal("accepted an ambiguous or unowned daemon listener")
			}
		})
	}
}
