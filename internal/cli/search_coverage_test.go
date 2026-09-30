package cli

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestSearchSourceCompletenessProjectionAndContract(t *testing.T) {
	manifest, _ := fixtureOutputContracts(t)
	definition, exists := manifest.OutputDefinitions["search_coverage"]
	if !exists {
		t.Fatal("search coverage definition missing")
	}
	found := false
	for _, field := range definition.Fields {
		if field.Name == "sources_complete" {
			found = true
			if field.Type != "boolean" || !field.AlwaysPresent || field.Nullable || !strings.Contains(field.Description, "every page") {
				t.Fatalf("sources_complete contract = %+v", field)
			}
		}
	}
	if !found {
		t.Fatal("sources_complete contract missing")
	}
	for _, command := range []string{"filter", "search"} {
		for _, fields := range []string{"", "sender,snippet", "all"} {
			for _, complete := range []bool{false, true} {
				t.Run(command+"/"+fields+"/"+strconv.FormatBool(complete), func(t *testing.T) {
					gateway := newPageProjectionGateway()
					gateway.searchPage.Coverage.SourcesComplete = complete
					code, output, stderr := runPageProjectionCommand(gateway, pageProjectionArgs(command, fields))
					var page struct {
						Coverage struct {
							SourcesComplete *bool `json:"sources_complete"`
						} `json:"coverage"`
					}
					if err := json.Unmarshal(pageFromEnvelope(t, output), &page); err != nil {
						t.Fatal(err)
					}
					if code != 0 || stderr != "" || page.Coverage.SourcesComplete == nil || *page.Coverage.SourcesComplete != complete {
						t.Fatalf("code=%d stderr=%q output=%s", code, stderr, output)
					}
					var human bytes.Buffer
					writeSearchResults(&human, gateway.searchPage)
					if !strings.Contains(human.String(), "sources_complete="+strconv.FormatBool(complete)) {
						t.Fatalf("human output loses source evidence: %s", human.String())
					}
				})
			}
		}
	}
}
