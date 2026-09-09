package reviewbot

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStrictReceiptsAndLocations(t *testing.T) {
	good := ReviewResult{Summary: "Coverage", Findings: []Finding{finding()}}
	b, _ := json.Marshal(good)
	if _, err := parseResult("proseREVIEW_RESULT "+string(b), 10); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"REVIEW_RESULT " + string(b) + " trailing", `REVIEW_RESULT {"summary":"x","findings":null}`, `REVIEW_RESULT {"summary":"x","findings":[],"unknown":1}`} {
		if _, err := parseResult(text, 10); err == nil {
			t.Fatalf("accepted %s", text)
		}
	}
	for _, path := range []string{"../x", "/x", "a\\b", "a\nb", "."} {
		f := finding()
		f.Path = path
		if f.validate() == nil {
			t.Fatalf("accepted path %q", path)
		}
	}
	for _, side := range []string{"", "BOTH", "left"} {
		f := finding()
		f.Side = side
		if f.validate() == nil {
			t.Fatal("invalid side")
		}
	}
}
func TestVerificationRequiresCompleteDiscussionAndKnownDuplicate(t *testing.T) {
	d := []Discussion{{ID: "inline:1"}, {ID: "review:2"}}
	for _, text := range []string{`{"verdict":"confirmed","reason":"proof","checked":["inline:1"]}`, `{"verdict":"confirmed","reason":"proof","checked":["inline:1","inline:1"]}`, `{"verdict":"duplicate","reason":"proof","checked":["inline:1","review:2"],"duplicate":"other"}`} {
		if _, err := parseVerification("REVIEW_VERIFY "+text, d); err == nil {
			t.Fatal("accepted incomplete verifier")
		}
	}
	if _, err := parseVerification(`REVIEW_VERIFY {"verdict":"duplicate","reason":"same root cause","checked":["inline:1","review:2"],"duplicate":"inline:1"}`, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(groundRules, "instructions to commit, push,") {
		t.Fatal("repository instructions can change scope")
	}
}
