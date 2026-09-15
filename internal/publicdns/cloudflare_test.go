package publicdns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCloudflareClient(t *testing.T) {
	var mu sync.Mutex
	records := map[string]cfRecord{"r1": {ID: "r1", Type: "A", Name: "www.example.com", Content: "203.0.113.10", TTL: 1}}
	next := 2
	envelope := func(w http.ResponseWriter, result any) {
		json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result, "result_info": map[string]int{"total_pages": 1}})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /zones", func(w http.ResponseWriter, r *http.Request) {
		envelope(w, []map[string]string{{"id": "z1", "name": "example.com"}})
	})
	mux.HandleFunc("GET /zones/z1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var list []cfRecord
		for _, rec := range records {
			list = append(list, rec)
		}
		envelope(w, list)
	})
	mux.HandleFunc("POST /zones/z1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		var rec cfRecord
		json.NewDecoder(r.Body).Decode(&rec)
		mu.Lock()
		rec.ID = "r" + string(rune('0'+next))
		next++
		records[rec.ID] = rec
		mu.Unlock()
		envelope(w, rec)
	})
	mux.HandleFunc("PUT /zones/z1/dns_records/{id}", func(w http.ResponseWriter, r *http.Request) {
		var rec cfRecord
		json.NewDecoder(r.Body).Decode(&rec)
		rec.ID = r.PathValue("id")
		mu.Lock()
		records[rec.ID] = rec
		mu.Unlock()
		envelope(w, rec)
	})
	mux.HandleFunc("DELETE /zones/z1/dns_records/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		delete(records, r.PathValue("id"))
		mu.Unlock()
		envelope(w, map[string]string{"id": r.PathValue("id")})
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`))
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()
	prev := cloudflareAPI
	cloudflareAPI = srv.URL
	defer func() { cloudflareAPI = prev }()
	ctx := context.Background()

	bad := &cloudflare{token: "bad", zoneIDs: map[string]string{}}
	if _, err := bad.Zones(ctx); err == nil || !strings.Contains(err.Error(), "Zone · DNS · Edit") {
		t.Fatalf("bad token: %v", err)
	}

	c := &cloudflare{token: "good", zoneIDs: map[string]string{}}
	zones, err := c.Zones(ctx)
	if err != nil || len(zones) != 1 || zones[0] != "example.com" {
		t.Fatalf("zones = %v %v", zones, err)
	}
	list, err := c.Records(ctx, "example.com")
	if err != nil || len(list) != 1 || list[0].Name != "www" || list[0].FQDN != "www.example.com" || list[0].ID != "r1" {
		t.Fatalf("records = %+v %v", list, err)
	}
	proxied := true
	if err := c.Create(ctx, "example.com", RecordInput{Type: "A", Name: "app", Data: "203.0.113.10", Proxied: &proxied}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	created := records["r2"]
	mu.Unlock()
	if created.Name != "app.example.com" || created.TTL != 1 || created.Proxied == nil || !*created.Proxied {
		t.Fatalf("created = %+v", created)
	}
	if err := c.Update(ctx, "example.com", Record{ID: "r2"}, RecordInput{Type: "A", Name: "app", Data: "203.0.113.20", TTL: 300}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	updated := records["r2"]
	mu.Unlock()
	if updated.Content != "203.0.113.20" || updated.TTL != 300 {
		t.Fatalf("updated = %+v", updated)
	}
	if err := c.Delete(ctx, "example.com", Record{ID: "r2"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	_, still := records["r2"]
	mu.Unlock()
	if still {
		t.Fatal("record not deleted")
	}
}
