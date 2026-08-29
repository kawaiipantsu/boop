package config

import (
	"testing"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// Every category a tool can report must be configurable, or the rule table
// silently cannot express what the user wants.
func TestEveryPermissionCategoryIsConfigurable(t *testing.T) {
	rules := Default().Permissions.Rules()
	for cat := range permissions.DefaultRules() {
		rule, ok := rules[cat]
		if !ok {
			t.Errorf("category %q has no config key; it cannot be set", cat)
			continue
		}
		if rule == "" {
			t.Errorf("category %q maps to an empty rule", cat)
		}
	}
}

func TestNetworkFetchAndSearchAreDistinct(t *testing.T) {
	rules := Default().Permissions.Rules()
	if rules[permissions.CatNetworkSearch] != permissions.RuleAllow {
		t.Errorf("search = %q, want allow", rules[permissions.CatNetworkSearch])
	}
	if rules[permissions.CatNetworkFetch] != permissions.RuleConfirm {
		t.Errorf("fetch = %q, want confirm", rules[permissions.CatNetworkFetch])
	}
}
