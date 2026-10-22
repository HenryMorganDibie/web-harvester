#!/usr/bin/env python3
"""Review-only Tavily discovery pilot using Deal Intelligence's deterministic extractor."""
import argparse, json, os, re, sys, time
from datetime import datetime, timezone
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import urlparse
import httpx

QUERIES = [
    '"Africa" startup raises funding investor 2026',
    '"African" private equity acquisition investment 2026',
    'Africa fund first close final close limited partners 2026',
    'Africa development finance institution loan facility investment 2026',
    'Nigeria Kenya Ghana Egypt South Africa company acquired stake investment 2026',
    'Afrique levée de fonds acquisition investissement fonds 2026',
    'África financiamento aquisição investimento fundo 2026',
]
MAX_RESULTS = 5
USER_AGENT = "DealIntelligenceDiscoveryPilot/1.0"

class TextExtractor(HTMLParser):
    def __init__(self):
        super().__init__()
        self.skip = 0
        self.in_title = 0
        self.title = []
        self.parts = []
    def handle_starttag(self, tag, attrs):
        if tag in {"script", "style", "noscript", "svg"}: self.skip += 1
        if tag == "title": self.in_title += 1
    def handle_endtag(self, tag):
        if tag in {"script", "style", "noscript", "svg"} and self.skip: self.skip -= 1
        if tag == "title" and self.in_title: self.in_title -= 1
    def handle_data(self, data):
        if self.skip: return
        value = re.sub(r"\s+", " ", data).strip()
        if value:
            self.parts.append(value)
            if self.in_title: self.title.append(value)

def page_text(markup):
    p = TextExtractor()
    try: p.feed(markup[:2000000])
    except Exception: pass
    return (re.sub(r"\s+", " ", " ".join(p.title)).strip()[:300],
            re.sub(r"\s+", " ", " ".join(p.parts)).strip()[:12000])

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--deal-intel-root", required=True)
    ap.add_argument("--output", default="deal-intelligence-discovery.json")
    ap.add_argument("--max-pages", type=int, choices=(5, 10, 15), default=10)
    args = ap.parse_args()
    key = os.environ.get("TAVILY_API_KEY", "").strip()
    if not key: raise SystemExit("TAVILY_API_KEY Actions secret is missing.")
    sys.path.insert(0, str(Path(args.deal_intel_root).resolve()))
    try:
        from src.engines.deals import extract_deal
    except Exception as exc:
        raise SystemExit(f"Cannot import Deal Intelligence extractor: {type(exc).__name__}") from exc
    proxy_url = os.environ.get("HARVESTER_PROXY_URL", "").strip()
    search_opts = {"timeout": httpx.Timeout(20.0, connect=10.0), "follow_redirects": True}
    fetch_opts = dict(search_opts)
    if proxy_url: fetch_opts["proxy"] = proxy_url
    start = time.monotonic()
    by_url, query_stats, fetch_errors = {}, [], {}
    # Search uses Tavily directly; the source-page retrieval leg uses anyIP.
    with httpx.Client(**search_opts) as client:
        for query in QUERIES:
            try:
                r = client.post("https://api.tavily.com/search",
                    headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"},
                    json={"query": query, "topic": "news", "search_depth": "basic",
                          "max_results": MAX_RESULTS, "include_answer": False, "include_raw_content": False})
                r.raise_for_status()
                rows = r.json().get("results") or []
                query_stats.append({"query": query, "status": "ok", "results": len(rows)})
                for row in rows:
                    url = str(row.get("url") or "").strip()
                    parsed = urlparse(url)
                    if parsed.scheme not in {"http", "https"} or not parsed.hostname: continue
                    if url not in by_url:
                        by_url[url] = {"title": str(row.get("title") or "")[:500], "url": url,
                            "publisher": parsed.hostname.lower(), "published_date": row.get("published_date"),
                            "search_snippet": str(row.get("content") or "")[:1500], "matched_queries": [query]}
                    elif query not in by_url[url]["matched_queries"]:
                        by_url[url]["matched_queries"].append(query)
            except Exception as exc:
                query_stats.append({"query": query, "status": "error", "error_type": type(exc).__name__})
    results = list(by_url.values())
    candidates, fetched = [], 0
    with httpx.Client(**fetch_opts, headers={"User-Agent": USER_AGENT, "Accept": "text/html,application/xhtml+xml"}) as client:
        for item in results[:args.max_pages]:
            evidence, title, status = item["search_snippet"], item["title"], "not_fetched"
            try:
                r = client.get(item["url"])
                status = f"http_{r.status_code}"
                if r.is_success and "text/html" in r.headers.get("content-type", "").lower():
                    new_title, body = page_text(r.text)
                    if body: evidence = body[:5000]
                    if new_title: title = new_title
                    fetched += 1
                    status = "fetched"
            except Exception as exc:
                kind = type(exc).__name__
                fetch_errors[kind] = fetch_errors.get(kind, 0) + 1
                status = f"error_{kind}"
            item.update({"title": title[:500], "page_fetch_status": status,
                         "page_evidence_excerpt": evidence[:1200]})
            try:
                deal = extract_deal(title, evidence[:5000], published=item.get("published_date"),
                                    publisher=item["publisher"], url=item["url"], africa_source=True)
            except Exception:
                deal = None
            if deal:
                candidates.append({"deal": deal, "source": {
                    "title": title[:500], "url": item["url"], "publisher": item["publisher"],
                    "published_date": item.get("published_date"), "matched_queries": item["matched_queries"],
                    "page_fetch_status": status, "evidence_excerpt": evidence[:1200]},
                    "review_status": "candidate_not_yet_persisted"})
    seen, unique = set(), []
    for c in candidates:
        k = (c["source"]["url"], str(c["deal"].get("deal_type", "")),
             str(c["deal"].get("target", "")).casefold())
        if k not in seen: seen.add(k); unique.append(c)
    output = {"schema_version": 1, "run_at_utc": datetime.now(timezone.utc).isoformat(),
      "mode": "review_only_no_production_writes",
      "limits": {"queries": len(QUERIES), "max_results_per_query": MAX_RESULTS,
                 "max_source_pages_fetched": args.max_pages, "proxy_configured": bool(proxy_url),
                 "proxy_credentials_written": False, "raw_html_written": False},
      "summary": {"queries_succeeded": sum(q["status"] == "ok" for q in query_stats),
        "queries_failed": sum(q["status"] != "ok" for q in query_stats),
        "unique_urls_discovered": len(results), "source_pages_fetched": fetched,
        "fetch_errors_by_type": fetch_errors, "deal_candidates": len(unique),
        "elapsed_seconds": round(time.monotonic() - start, 2)},
      "queries": query_stats, "results": results, "deal_candidates": unique,
      "limitations": ["Candidates are not verified deals.", "No production database is changed.",
        "Snippet-only candidates need extra scrutiny.", "This is a discovery pilot, not comprehensive Bloomberg-equivalent coverage."]}
    out = Path(args.output); out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(output, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"summary": output["summary"], "proxy_configured": bool(proxy_url), "output": str(out)}))
    return 0 if output["summary"]["queries_succeeded"] else 1

if __name__ == "__main__":
    raise SystemExit(main())
