package main

import "testing"

// TestPublicationBaseline retains approved migration ports without fabricating forwarding rules.
func TestPublicationBaseline(t *testing.T) {
	cfg := Config{Mappings: []Mapping{{Enabled: true, ListenHost: "0.0.0.0", ListenPort: 23306}}}
	ports, err := publicationPorts(cfg, []int{3306, 13306})
	if err != nil || len(ports) != 3 || ports[0] != 3306 || ports[1] != 13306 || ports[2] != 23306 {
		t.Fatal(ports, err)
	}
	if len(cfg.Mappings) != 1 {
		t.Fatal("migration invented mapping")
	}
	ports, err = publicationPorts(cfg, nil)
	if err != nil || len(ports) != 1 || ports[0] != 23306 {
		t.Fatal("post-migration removal prevented", ports, err)
	}
	if _, err = publicationPorts(cfg, []int{6379}); err == nil {
		t.Fatal("foreign baseline accepted")
	}
}
