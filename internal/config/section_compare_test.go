package config

import "testing"

func TestSameRoutingConfigComparesTheNormalizedForm(t *testing.T) {
	t.Parallel()

	stored := NormalizeRoutingConfig(RoutingConfig{
		Strategy:            "fill-first",
		IncludeDefaultGroup: true,
		ChannelGroups: []RoutingChannelGroup{{
			Name:              "team-a",
			Match:             ChannelGroupMatch{Prefixes: []string{"team-a"}},
			ChannelPriorities: map[string]int{"Codex1": 2, "Codex2": 1},
		}},
		PathRoutes: []RoutingPathRoute{{Path: "/team-a", Group: "team-a"}},
	})
	spelledDifferently := RoutingConfig{
		Strategy:            " FF ",
		IncludeDefaultGroup: true,
		ChannelGroups: []RoutingChannelGroup{{
			Name:              "Team-A/",
			Match:             ChannelGroupMatch{Prefixes: []string{"Team-A"}, Channels: []string{}},
			ChannelPriorities: map[string]int{"Codex2": 1, "Codex1": 2},
		}},
		PathRoutes: []RoutingPathRoute{{Path: "team-a/", Group: "TEAM-A", Fallback: "none"}},
	}
	if !SameRoutingConfig(spelledDifferently, stored) {
		t.Fatalf("a respelling of the stored routing should compare equal:\n%+v\n%+v", NormalizeRoutingConfig(spelledDifferently), stored)
	}
	// The kind of section typed into an editor that never shows the stored
	// value: storing it would drop the group and the path route.
	if SameRoutingConfig(RoutingConfig{Strategy: "fill-first", IncludeDefaultGroup: true}, stored) {
		t.Fatal("routing without the stored group and path route compared equal")
	}
	without := stored
	without.IncludeDefaultGroup = false
	if SameRoutingConfig(without, stored) {
		t.Fatal("include-default-group differs but compared equal")
	}
	if !SameRoutingConfig(RoutingConfig{}, RoutingConfig{ChannelGroups: []RoutingChannelGroup{}, PathRoutes: []RoutingPathRoute{}}) {
		t.Fatal("absent and empty lists should compare equal")
	}
}

func TestSameProxyPoolIgnoresOrderAndSpelling(t *testing.T) {
	t.Parallel()

	stored := []ProxyPoolEntry{
		{ID: "hk-1", Name: "HK 1", URL: "http://hk-1.example:8080", Enabled: true},
		{ID: "sg-1", Name: "SG 1", URL: "socks5://sg-1.example:1080", Enabled: false},
	}
	written := []ProxyPoolEntry{
		{ID: "SG 1", Name: " SG 1 ", URL: " socks5://sg-1.example:1080 ", Enabled: false},
		{ID: "hk-1", Name: "HK 1", URL: "http://hk-1.example:8080", Enabled: true},
		{ID: "broken", URL: "ftp://never-stored.example:21", Enabled: true},
	}
	if !SameProxyPool(written, stored) {
		t.Fatal("the stored pool in another order and spelling should compare equal")
	}
	if !SameProxyPool(nil, []ProxyPoolEntry{}) {
		t.Fatal("absent and empty pools should compare equal")
	}
	disabled := append([]ProxyPoolEntry(nil), stored...)
	disabled[0].Enabled = false
	if SameProxyPool(disabled, stored) {
		t.Fatal("a disabled proxy compared equal to an enabled one")
	}
	if SameProxyPool(stored[:1], stored) {
		t.Fatal("a pool missing a proxy compared equal")
	}
	if stored[0].ID != "hk-1" || written[0].ID != "SG 1" {
		t.Fatal("SameProxyPool reordered or rewrote its arguments")
	}
}

func TestYAMLRootKeysListsTheKeysTheDocumentSpellsOut(t *testing.T) {
	t.Parallel()

	keys := YAMLRootKeys([]byte("# notes\nport: 8318\nrouting:\nproxy-pool: []\nnested:\n  api-keys: [x]\n"))
	if len(keys) != 4 || !keys["port"] || !keys["routing"] || !keys["proxy-pool"] || !keys["nested"] {
		t.Fatalf("root keys = %v, want port, routing, proxy-pool and nested", keys)
	}
	for _, doc := range []string{"", "- a list\n", "just a scalar\n", "port: [unclosed\n"} {
		if keys := YAMLRootKeys([]byte(doc)); keys != nil {
			t.Fatalf("YAMLRootKeys(%q) = %v, want nil", doc, keys)
		}
	}
}
