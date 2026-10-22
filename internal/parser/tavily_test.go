package parser

import "testing"

func TestTavilyParser_Parse(t *testing.T) {
	body := []byte(`{"query":"q","answer":"Short answer","results":[{"title":"First","url":"https://example.com/1","content":"Snippet one","score":0.9},{"title":"Second","url":"https://example.com/2","content":"Snippet two","score":0.8}]}`)
	got, err := NewTavily().Parse("original query", body)
	if err != nil { t.Fatalf("Parse returned error: %v", err) }
	if got.Query != "original query" || len(got.Organic) != 2 { t.Fatalf("unexpected parsed result: %+v", got) }
	if got.Organic[0].Position != 1 || got.Organic[0].Title != "First" || got.Organic[0].URL != "https://example.com/1" || got.Organic[0].Snippet != "Snippet one" { t.Errorf("unexpected first result: %+v", got.Organic[0]) }
	if got.FeaturedSnippet == nil || got.FeaturedSnippet.Text != "Short answer" { t.Errorf("expected Tavily answer mapping, got %+v", got.FeaturedSnippet) }
	if got.Calibration != nil { t.Errorf("did not expect calibration note: %+v", got.Calibration) }
}

func TestTavilyParser_MissingResultsIsCalibrated(t *testing.T) {
	got, err := NewTavily().Parse("q", []byte(`{"results":[]}`))
	if err != nil { t.Fatalf("Parse returned error: %v", err) }
	if got.Calibration == nil { t.Fatal("expected missing-results calibration") }
}
