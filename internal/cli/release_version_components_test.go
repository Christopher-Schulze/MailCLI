package cli

import (
	"strings"
	"testing"
)

func TestReleaseVersionRejectsNonnumericComponents(t *testing.T) {
	for _, value := range []string{
		"+1.2.3", "1.+2.3", "1.2.+3", "v+1.2.3",
		"+0.0.0", "0.+0.0", "0.0.+0", "-0.0.0", "0.-0.0", "0.0.-0",
		"-1.2.3", "1.-2.3", "1.2.-3", "01.2.3", "1.02.3", "1.2.03",
		"1..3", ".2.3", "1.2.", "1.2", "1.2.3.4", "v", "",
		"1 .2.3", "1. 2.3", "1.2. 3", "1.\t2.3", "1.2.\n3",
		"١.2.3", "1.２.3", "1.2.3-beta", strings.Repeat("9", 64) + ".2.3",
	} {
		t.Run(value, func(t *testing.T) {
			parts, normalized, err := parseReleaseVersion(value)
			if err == nil || parts != [3]int{} || normalized != "" {
				t.Errorf("invalid version accepted: parts=%v normalized=%q error=%v", parts, normalized, err)
			}
			for _, pair := range [][2]string{{value, "1.2.3"}, {"1.2.3", value}} {
				_, _, err := compareReleaseVersions(pair[0], pair[1])
				if err == nil || errorCode(err) != "update_package_invalid" {
					t.Errorf("comparison accepted invalid component: pair=%q error=%v", pair, err)
				}
			}
		})
	}
}

func TestReleaseVersionPreservesValidNormalization(t *testing.T) {
	for _, test := range []struct {
		value, normalized string
		parts             [3]int
	}{
		{"0.0.0", "0.0.0", [3]int{0, 0, 0}},
		{"1.2.3", "1.2.3", [3]int{1, 2, 3}},
		{"v1.2.3", "1.2.3", [3]int{1, 2, 3}},
		{"10.20.30", "10.20.30", [3]int{10, 20, 30}},
		{"  v1.2.3\n", "1.2.3", [3]int{1, 2, 3}},
	} {
		t.Run(test.value, func(t *testing.T) {
			parts, normalized, err := parseReleaseVersion(test.value)
			if err != nil || parts != test.parts || normalized != test.normalized {
				t.Fatalf("valid version changed: parts=%v normalized=%q error=%v", parts, normalized, err)
			}
		})
	}
}
