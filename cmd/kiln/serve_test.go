package main

import "testing"

func TestLoopbackAddr(t *testing.T) {
	loopback := []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "LOCALHOST:80"}
	for _, a := range loopback {
		if !loopbackAddr(a) {
			t.Errorf("loopbackAddr(%q) = false, want true", a)
		}
	}
	// An empty host binds every interface; with auth disabled that hands the
	// instance to the network, so it must not count as loopback.
	reachable := []string{":8080", "0.0.0.0:8080", "192.168.1.5:8080", "[::]:8080", "example.com:8080", "garbage"}
	for _, a := range reachable {
		if loopbackAddr(a) {
			t.Errorf("loopbackAddr(%q) = true, want false", a)
		}
	}
}
