package lbengine

import (
	"os"
	"testing"
)

func TestUsableBackends(t *testing.T) {
	csv := "# pxname,svname,qcur,status,weight\n" +
		"fe,FRONTEND,,OPEN,\n" +
		"api,api-1,0,UP,100\n" +
		"api,api-2,0,DOWN,100\n" +
		"api,BACKEND,0,UP,200\n" +
		"db,db-1,0,DOWN,100\n" +
		"db,db-2,0,MAINT,100\n" +
		"db,BACKEND,0,DOWN,0\n" +
		"cache,cache-1,0,UP 1/3,100\n" +
		"static,static-1,0,no check,100\n" +
		"drained,d-1,0,DRAIN,0\n" +
		"stats,FRONTEND,,OPEN,\n"
	got := UsableBackends(csv)
	want := map[string]bool{"api": true, "db": false, "cache": true, "static": true, "drained": false}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
	// The real HAProxy fixture parses too.
	if b, err := os.ReadFile("../testdata/showstat.csv"); err == nil {
		if len(UsableBackends(string(b))) == 0 {
			t.Error("no backends parsed from the HAProxy fixture")
		}
	}
}
