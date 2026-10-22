#!/usr/bin/env python3
"""Recent-deal discovery with source validation and read-only production deduplication."""
from __future__ import annotations

import argparse
import json
import html as html_lib
from concurrent.futures import ThreadPoolExecutor, as_completed
import os
import re
import sys
import time
from datetime import date, datetime, timedelta, timezone
from email.utils import parsedate_to_datetime
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import parse_qsl, urlencode, urlparse, urlunparse

import httpx

USER_AGENT = "DealIntelligenceDiscoveryPilot/2.0"
API_DEFAULT = "https://deal-intelligence-api-henry-dibie-s-projects.vercel.app"
QUERY_TEMPLATES = [
    "Africa startup funding round investment raises venture capital",
    "African private equity acquisition buyout stake transaction",
    "Africa private capital fund first close final close fundraising",
    "Africa DFI investment financing loan facility company",
    "African company acquisition merger stake investment announced",
    "Afrique startup levée de fonds acquisition investissement capital",
    "África empresa financiamento investimento aquisição capital de risco",
    "African company receives investment from IFC BII Proparco AfDB",
]
MAX_RESULTS_PER_QUERY = 10
AFRICAN_ISO2 = {"DZ", "AO", "BJ", "BW", "BF", "BI", "CV", "CM", "CF", "TD", "KM", "CG", "CD", "CI", "DJ", "EG", "GQ", "ER", "SZ", "ET", "GA", "GM", "GH", "GN", "GW", "KE", "LS", "LR", "LY", "MG", "MW", "ML", "MR", "MU", "MA", "MZ", "NA", "NE", "NG", "RW", "ST", "SN", "SC", "SL", "SO", "ZA", "SS", "SD", "TZ", "TG", "TN", "UG", "ZM", "ZW", "EH"}
FRESHNESS_DAYS = 30
SEARCH_TIMEOUT = httpx.Timeout(15.0, connect=6.0)
PAGE_TIMEOUT = httpx.Timeout(12.0, connect=5.0)
API_TIMEOUT = httpx.Timeout(8.0, connect=4.0)


def normalize_url(url: str) -> str:
    """Remove tracking fragments while preserving meaningful query parameters."""
    try:
        p = urlparse(url.strip())
        if p.scheme not in {"http", "https"} or not p.hostname:
            return ""
        host = p.hostname.lower()
        if p.port and not ((p.scheme == "http" and p.port == 80) or (p.scheme == "https" and p.port == 443)):
            host += f":{p.port}"
        path = re.sub(r"/{2,}", "/", p.path or "/")
        query = urlencode([
            (k, v) for k, v in parse_qsl(p.query, keep_blank_values=True)
            if not k.lower().startswith("utm_") and k.lower() not in {"fbclid", "gclid", "mc_cid", "mc_eid"}
        ])
        return urlunparse((p.scheme.lower(), host, path.rstrip("/") or "/", "", query, ""))
    except (TypeError, ValueError):
        return ""


def parse_date(value) -> date | None:
    """Parse Tavily or publisher dates without guessing missing dates."""
    if not value:
        return None
    if isinstance(value, date) and not isinstance(value, datetime):
        return value
    if isinstance(value, (int, float)):
        try:
            return datetime.fromtimestamp(value, timezone.utc).date()
        except (OverflowError, OSError, ValueError):
            return None
    raw = str(value).strip()
    if not raw:
        return None
    try:
        return date.fromisoformat(raw[:10])
    except ValueError:
        pass
    try:
        dt = datetime.fromisoformat(raw.replace("Z", "+00:00"))
        return dt.date()
    except ValueError:
        pass
    try:
        return parsedate_to_datetime(raw).date()
    except (TypeError, ValueError, OverflowError):
        return None


def extract_event_date(text: str, reference_date=None) -> date | None:
    """Prefer a transaction milestone date explicitly described in the article over its publication date."""
    months = r"(?:Jan(?:uary)?|Feb(?:ruary)?|Mar(?:ch)?|Apr(?:il)?|May|Jun(?:e)?|Jul(?:y)?|Aug(?:ust)?|Sep(?:tember)?|Oct(?:ober)?|Nov(?:ember)?|Dec(?:ember)?)"
    date_pattern = re.compile(
        rf"\b(?:{months}\s+\d{{1,2}},?\s+\d{{4}}|\d{{4}}-\d{{2}}-\d{{2}})\b",
        re.IGNORECASE,
    )
    event_words = re.compile(
        r"\b(final[- ]close|first[- ]close|close|closing|announc\w*|rais\w*|funding|"
        r"acqui\w*|invest\w*|financ\w*|complet\w*|signed|transaction|deal|loan|facility|merger)\b",
        re.IGNORECASE,
    )
    found = []
    for match in date_pattern.finditer(text or ""):
        raw = match.group(0)
        parsed = parse_date(raw)
        if parsed is None:
            for fmt in ("%B %d, %Y", "%B %d %Y", "%b %d, %Y", "%b %d %Y"):
                try:
                    parsed = datetime.strptime(raw, fmt).date()
                    break
                except ValueError:
                    continue
        if parsed is None:
            continue
        context = text[max(0, match.start() - 100):min(len(text), match.end() + 100)]
        if not event_words.search(context):
            continue
        if reference_date and parsed > reference_date:
            continue
        found.append(parsed)
    return max(found) if found else None


def africa_relevant(deal: dict, title: str, lead: str, countries_in_text) -> bool:
    """Reject transactions with no African party/market evidence despite broad African search terms."""
    if str(deal.get("country") or "").upper() in AFRICAN_ISO2:
        return True
    if any(str(code).upper() in AFRICAN_ISO2 for code in (deal.get("countries") or [])):
        return True
    text = " ".join((title or "", lead or ""))
    if any(getattr(country, "iso2", "").upper() in AFRICAN_ISO2 for country in countries_in_text(text)):
        return True
    return bool(re.search(r"\b(africa|african|afrique|africaine|africain|áfrica|africano|africana)\b", title or "", re.IGNORECASE))


def _target_tokens(value: str) -> set[str]:
    tokens = set(re.findall(r"[a-z0-9]+", (value or "").casefold()))
    return tokens - {"the", "a", "an", "fund", "group", "holdings", "holding", "limited", "ltd", "plc", "inc"}


def related_fund_matches(candidate: dict, existing: list[dict]) -> list[dict]:
    """Flag same-manager/fund family records for review when closes may be separate milestones."""
    if candidate.get("deal_type") != "fund_raise" or not candidate.get("target"):
        return []
    tokens = _target_tokens(candidate.get("target", ""))
    try:
        candidate_day = date.fromisoformat(str(candidate.get("announced") or "")[:10])
    except ValueError:
        return []
    matches = []
    for row in existing:
        if row.get("deal_type") != "fund_raise" or not row.get("target"):
            continue
        other_tokens = _target_tokens(row.get("target", ""))
        if not tokens or not other_tokens or not (tokens <= other_tokens or other_tokens <= tokens):
            continue
        try:
            other_day = date.fromisoformat(str(row.get("announced") or "")[:10])
        except ValueError:
            continue
        if abs((candidate_day - other_day).days) <= 365:
            matches.append(row)
    return matches


def freshness_status(published, today: date | None = None, days: int = FRESHNESS_DAYS) -> tuple[str, date | None]:
    today = today or datetime.now(timezone.utc).date()
    published_day = parse_date(published)
    if published_day is None:
        return "undated", None
    if published_day > today:
        return "future_date", published_day
    if published_day < today - timedelta(days=days):
        return "stale", published_day
    return "recent", published_day


def candidate_matches_database(candidate: dict, existing: list[dict], same_transaction) -> list[dict]:
    """Use production transaction identity logic, then exact canonical IDs as a fallback."""
    candidate_id = str(candidate.get("id") or "")
    matches = []
    for row in existing:
        if candidate_id and candidate_id == str(row.get("id") or ""):
            matches.append(row)
            continue
        try:
            if same_transaction(candidate, row):
                matches.append(row)
        except (KeyError, TypeError, ValueError):
            continue
    return matches


def classify_candidates(candidates: list[dict], existing: list[dict], same_transaction) -> list[dict]:
    out = []
    for candidate in candidates:
        matches = candidate_matches_database(candidate["deal"], existing, same_transaction)
        if matches:
            status = "already_in_database"
        elif candidate.get("freshness_status") == "recent" and candidate.get("source", {}).get("page_fetch_status") == "fetched":
            if candidate.get("source", {}).get("date_basis") in {"publisher_event", "publisher_article"}:
                status = "potentially_new_needs_human_review"
            elif candidate.get("source", {}).get("date_basis") == "search_result":
                status = "needs_date_confirmation"
            else:
                status = "not_eligible_as_new"
        else:
            status = "not_eligible_as_new"
        candidate["database_match"] = [
            {"id": row.get("id"), "announced": row.get("announced"), "headline": row.get("headline"),
             "deal_type": row.get("deal_type"), "target": row.get("target")}
            for row in matches[:5]
        ]
        candidate["review_status"] = status
        out.append(candidate)
    return out


class ArticleParser(HTMLParser):
    """Extract article-focused paragraphs and publication metadata, not site-wide boilerplate."""
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.skip_depth = 0
        self.article_depth = 0
        self.main_depth = 0
        self.in_paragraph = False
        self.paragraph_parts = []
        self.paragraphs = []
        self.article_paragraphs = []
        self.main_paragraphs = []
        self.title_depth = 0
        self.title_parts = []
        self.meta = {}
        self._article_seen = False
        self._main_seen = False

    def handle_starttag(self, tag, attrs):
        a = {str(k).lower(): str(v) for k, v in attrs if v is not None}
        if tag in {"script", "style", "noscript", "svg", "nav", "footer", "header", "aside"}:
            self.skip_depth += 1
        if tag == "article":
            self.article_depth += 1
            self._article_seen = True
        if tag == "main":
            self.main_depth += 1
            self._main_seen = True
        if tag == "title":
            self.title_depth += 1
        if tag == "p":
            self.in_paragraph = True
            self.paragraph_parts = []
        if tag == "meta":
            key = (a.get("property") or a.get("name") or a.get("itemprop") or "").lower()
            val = a.get("content", "").strip()
            if key and val and key in {
                "article:published_time", "article:modified_time", "date", "datepublished",
                "pubdate", "publish-date", "parsely-pub-date", "og:title",
            }:
                self.meta.setdefault(key, val)

    def handle_endtag(self, tag):
        if tag in {"script", "style", "noscript", "svg", "nav", "footer", "header", "aside"} and self.skip_depth:
            self.skip_depth -= 1
        if tag == "article" and self.article_depth:
            self.article_depth -= 1
        if tag == "main" and self.main_depth:
            self.main_depth -= 1
        if tag == "title" and self.title_depth:
            self.title_depth -= 1
        if tag == "p" and self.in_paragraph:
            value = re.sub(r"\s+", " ", " ".join(self.paragraph_parts)).strip()
            if value:
                self.paragraphs.append(value)
                if self.article_depth:
                    self.article_paragraphs.append(value)
                if self.main_depth:
                    self.main_paragraphs.append(value)
            self.paragraph_parts = []
            self.in_paragraph = False

    def handle_data(self, data):
        if self.skip_depth:
            return
        value = re.sub(r"\s+", " ", data).strip()
        if not value:
            return
        if self.title_depth:
            self.title_parts.append(value)
        if self.in_paragraph:
            self.paragraph_parts.append(value)

    def result(self):
        # Prefer the semantic article body, then main content, then paragraph-only fallback.
        paragraphs = self.article_paragraphs or self.main_paragraphs or self.paragraphs
        title = re.sub(r"\s+", " ", " ".join(self.title_parts)).strip()
        if not title:
            title = self.meta.get("og:title", "")
        return title[:300], paragraphs[:120], self.meta


def _jsonld_article(markup: str) -> dict:
    """Read publisher-declared Article JSON-LD when available."""
    found = []
    pattern = re.compile(
        r"<script\b[^>]*type\s*=\s*['\"]application/ld\+json['\"][^>]*>(.*?)</script\s*>",
        re.IGNORECASE | re.DOTALL,
    )

    def walk(value):
        if isinstance(value, list):
            for item in value:
                walk(item)
        elif isinstance(value, dict):
            types = value.get("@type", [])
            if isinstance(types, str):
                types = [types]
            if any(str(t).lower() in {"article", "newsarticle", "report", "blogposting"} for t in types):
                found.append(value)
            for key, child in value.items():
                if key in {"@graph", "mainEntity", "mainEntityOfPage"}:
                    walk(child)

    for raw in pattern.findall(markup[:2_000_000]):
        try:
            walk(json.loads(html_lib.unescape(raw).strip()))
        except (ValueError, TypeError):
            continue
    if not found:
        return {}
    found.sort(key=lambda obj: (bool(obj.get("articleBody")), bool(obj.get("datePublished"))), reverse=True)
    article = found[0]
    body = article.get("articleBody")
    if isinstance(body, str):
        body = html_lib.unescape(re.sub(r"<[^>]+>", " ", body))
        body = re.sub(r"\s+", " ", body).strip()
    else:
        body = ""
    return {
        "title": str(article.get("headline") or article.get("name") or "")[:300],
        "body": body[:12000],
        "published_date": article.get("datePublished"),
    }


def parse_article(markup: str) -> dict:
    parser = ArticleParser()
    try:
        parser.feed(markup[:2_000_000])
    except Exception:
        pass
    title, paragraphs, meta = parser.result()
    structured = _jsonld_article(markup)
    if structured.get("title"):
        # Structured publisher headline is generally more article-specific than a generic page title.
        title = structured["title"]
    if not paragraphs and structured.get("body"):
        paragraphs = [p.strip() for p in re.split(r"(?<=[.!?])\s+", structured["body"]) if p.strip()]
    deal_terms = re.compile(
        r"\b(raises?|raised|funding|investment|invests?|acquires?|acquisition|merger|merges?|"
        r"private equity|venture capital|fund close|first close|final close|loan|facility|financing|"
        r"levée|lève|rachat|acquiert|investissement|financement|aquisição|financiamento|investimento)\b",
        re.IGNORECASE,
    )
    relevant = [p for p in paragraphs if deal_terms.search(p)]
    # Use a small lead + transaction-specific paragraphs. Never feed the entire page to the extractor.
    chosen = list(dict.fromkeys((paragraphs[:2] + relevant[:5])))
    lead = re.sub(r"\s+", " ", " ".join(chosen)).strip()[:3500]
    pub_date = next((meta[k] for k in (
        "article:published_time", "datepublished", "parsely-pub-date", "publish-date", "pubdate", "date"
    ) if meta.get(k)), None) or structured.get("published_date")
    return {"title": title, "lead": lead, "published_date": pub_date, "paragraph_count": len(paragraphs)}


def fetch_database(api_base: str, client: httpx.Client, page_size: int = 100) -> list[dict]:
    """Fetch every current visible deal in parallel; fail closed on partial or changing snapshots."""
    def get_page(offset: int):
        response = client.get(
            f"{api_base.rstrip('/')}/api/deals",
            params={"limit": page_size, "offset": offset},
            headers={"Accept": "application/json", "User-Agent": USER_AGENT},
        )
        response.raise_for_status()
        payload = response.json()
        rows = payload.get("deals")
        total = payload.get("total_in_database")
        if not isinstance(rows, list) or not isinstance(total, int) or total < 0:
            raise RuntimeError("Production deals API returned an unexpected schema; refusing to continue.")
        return offset, total, rows

    first_offset, expected_total, first_rows = get_page(0)
    if expected_total == 0:
        raise RuntimeError("Production deals API returned zero records; refusing to treat an empty snapshot as complete.")
    if not first_rows and expected_total:
        raise RuntimeError("Production deals API returned an empty first page.")
    pages = [(first_offset, expected_total, first_rows)]
    offsets = list(range(page_size, expected_total, page_size))
    if offsets:
        with ThreadPoolExecutor(max_workers=6) as pool:
            futures = [pool.submit(get_page, offset) for offset in offsets]
            for future in as_completed(futures):
                pages.append(future.result())
    pages.sort(key=lambda page: page[0])
    all_rows = []
    for offset, total, rows in pages:
        if total != expected_total:
            raise RuntimeError("Production deal count changed during pagination; rerun to avoid partial deduplication.")
        expected_page_size = min(page_size, expected_total - offset)
        if len(rows) != expected_page_size:
            raise RuntimeError(
                f"Production deals API returned {len(rows)} records at offset {offset}; "
                f"expected {expected_page_size}. Refusing partial deduplication."
            )
        all_rows.extend(rows)
    if len(all_rows) != expected_total:
        raise RuntimeError(f"Loaded {len(all_rows)} of {expected_total} production deals; refusing partial deduplication.")
    ids = [str(row.get("id") or "") for row in all_rows]
    if any(not value for value in ids):
        raise RuntimeError("Production API returned a deal without a canonical id; refusing unsafe deduplication.")
    if len(set(ids)) != len(ids):
        raise RuntimeError("Production API pagination returned duplicate deal ids; refusing a potentially incomplete snapshot.")
    return all_rows


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--deal-intel-root", required=True)
    ap.add_argument("--output", default="deal-intelligence-discovery.json")
    ap.add_argument("--max-pages", type=int, choices=(5, 10, 15), default=10)
    ap.add_argument("--freshness-days", type=int, choices=(7, 14, 30), default=30)
    ap.add_argument("--api-base-url", default=os.environ.get("DEAL_INTEL_API_BASE_URL", API_DEFAULT))
    args = ap.parse_args()
    key = os.environ.get("TAVILY_API_KEY", "").strip()
    if not key:
        raise SystemExit("TAVILY_API_KEY Actions secret is missing.")
    api_base = args.api_base_url.rstrip("/")
    parsed_api = urlparse(api_base)
    if parsed_api.scheme != "https" or not parsed_api.hostname:
        raise SystemExit("DEAL_INTEL_API_BASE_URL must be a valid HTTPS base URL.")

    sys.path.insert(0, str(Path(args.deal_intel_root).resolve()))
    try:
        from src.engines.deals import extract_deal, same_transaction, countries_in_text, classify_sector
    except Exception as exc:
        raise SystemExit(f"Cannot import Deal Intelligence extractor: {type(exc).__name__}") from exc

    proxy_url = os.environ.get("HARVESTER_PROXY_URL", "").strip()
    start = time.monotonic()
    today = datetime.now(timezone.utc).date()
    since = today - timedelta(days=args.freshness_days)
    queries = [f"{q} {today.year}" for q in QUERY_TEMPLATES]
    by_url, query_stats, fetch_errors = {}, [], {}

    # Validate the production endpoint before spending Tavily requests. Search and API reads
    # are direct; only publisher-page requests use the configured proxy.
    try:
        with httpx.Client(timeout=API_TIMEOUT, follow_redirects=True) as api_client:
            database = fetch_database(api_base, api_client)
    except Exception as exc:
        raise SystemExit(f"Could not load the complete production deal database ({type(exc).__name__}); refusing to discover or label candidates as new.") from exc

    with httpx.Client(timeout=SEARCH_TIMEOUT, follow_redirects=True) as client:
        for query in queries:
            try:
                response = client.post(
                    "https://api.tavily.com/search",
                    headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"},
                    json={"query": query, "topic": "news", "time_range": "month", "days": args.freshness_days,
                          "search_depth": "advanced", "max_results": MAX_RESULTS_PER_QUERY,
                          "include_answer": False, "include_raw_content": False},
                )
                response.raise_for_status()
                rows = response.json().get("results") or []
                query_stats.append({"query": query, "status": "ok", "results": len(rows)})
                for row in rows:
                    url = normalize_url(str(row.get("url") or ""))
                    if not url:
                        continue
                    if url not in by_url:
                        by_url[url] = {
                            "title": str(row.get("title") or "")[:500], "url": url,
                            "publisher": (urlparse(url).hostname or "").lower(),
                            "published_date": row.get("published_date"),
                            "search_snippet": str(row.get("content") or "")[:1500],
                            "matched_queries": [query],
                        }
                    elif query not in by_url[url]["matched_queries"]:
                        by_url[url]["matched_queries"].append(query)
            except Exception as exc:
                query_stats.append({"query": query, "status": "error", "error_type": type(exc).__name__})

        if not any(q["status"] == "ok" for q in query_stats):
            raise SystemExit("All Tavily searches failed; no discovery results were generated.")

    results = list(by_url.values())
    for item in results:
        status, parsed = freshness_status(item.get("published_date"), today=today, days=args.freshness_days)
        item["freshness_status"] = status
        item["published_day"] = parsed.isoformat() if parsed else None
        item["freshness_cutoff"] = since.isoformat()
        item["page_fetch_status"] = "not_selected"

    # Process newest dated results first; do not let old/undated hits consume the fetch budget.
    recent_results = [r for r in results if r["freshness_status"] == "recent"]
    recent_results.sort(key=lambda r: (r["published_day"] or "", r["url"]), reverse=True)
    fetch_opts = {"timeout": PAGE_TIMEOUT, "follow_redirects": True}
    if proxy_url:
        fetch_opts["proxy"] = proxy_url
    candidates = []
    with httpx.Client(**fetch_opts, headers={"User-Agent": USER_AGENT, "Accept": "text/html,application/xhtml+xml"}) as client:
        for item in recent_results[:args.max_pages]:
            item["page_fetch_status"] = "fetch_failed"
            try:
                response = client.get(item["url"])
                item["http_status"] = response.status_code
                content_type = response.headers.get("content-type", "").lower()
                if not response.is_success or "text/html" not in content_type:
                    fetch_errors[f"http_{response.status_code}"] = fetch_errors.get(f"http_{response.status_code}", 0) + 1
                    continue
                article = parse_article(response.text)
                if article["title"]:
                    item["page_title"] = article["title"]
                item["page_fetch_status"] = "fetched"
                item["article_paragraph_count"] = article["paragraph_count"]
                fetched_day = parse_date(article.get("published_date"))
                if fetched_day:
                    item["publisher_published_day"] = fetched_day.isoformat()
                title = item.get("page_title") or item["title"]
                lead = article.get("lead") or ""
                event_day = extract_event_date("\n".join((title, lead)), reference_date=fetched_day or parse_date(item.get("published_date")))
                effective_day = event_day or fetched_day
                if effective_day:
                    item["published_day"] = effective_day.isoformat()
                    item["date_basis"] = "publisher_event" if event_day else "publisher_article"
                    item["freshness_status"] = freshness_status(effective_day, today=today, days=args.freshness_days)[0]
                else:
                    item["date_basis"] = "search_result" if parse_date(item.get("published_date")) else "unknown"
                # Search snippets alone must never create a deal from an unrelated publisher page.
                evidence = "\n".join(part for part in (title, lead) if part)
                item["page_evidence_excerpt"] = evidence[:1600]
                if item["freshness_status"] != "recent":
                    item["candidate_status"] = "skipped_not_recent_after_event_date_check"
                    continue
                if len(lead) < 80:
                    item["candidate_status"] = "insufficient_article_body_evidence"
                    continue
                deal_evidence = "\n".join(part for part in (title, lead[:2200]) if part)
                if not re.search(r"\b(raise|raises|raised|raising|funding|fundraise|fundraising|investment|invests|invested|acquires|acquired|acquisition|merger|financing|financed|loan|facility|levée|rachat|acquiert|investissement|financement|aquisição|financiamento|investimento)\b", deal_evidence, re.IGNORECASE):
                    item["candidate_status"] = "no_transaction_language_in_publisher_body"
                    continue
                deal = extract_deal(
                    title, deal_evidence,
                    published=item.get("published_day") or item.get("publisher_published_day"), publisher=item["publisher"], url=item["url"], africa_source=True,
                )
                if not deal:
                    item["candidate_status"] = "no_deal_extracted"
                    continue
                if not africa_relevant(deal, title, lead, countries_in_text):
                    item["candidate_status"] = "rejected_no_african_party_or_market_evidence"
                    continue
                # Recover fund series names and final-close values from explicit headline wording.
                if deal.get("deal_type") == "fund_raise":
                    fund_name = re.search(r"(?i)\b((?:[A-Z][\w&.-]*\s+){0,5}Fund\s+(?:II|III|IV|V|VI|VII|VIII|IX|X|\d+))\b", title)
                    if fund_name:
                        deal["target"] = re.sub(r"\s+", " ", fund_name.group(1)).strip()
                    final_close = re.search(
                        r"(?i)(?P<amount>(?:US\$|USD\s?|\$|€|£|₦)\s?[\d,.]+\s?(?:million|billion|m|bn|b)?)\s+(?:at\s+the\s+)?final[- ]close\b",
                        title,
                    )
                    if final_close:
                        amount_text = final_close.group("amount").replace(" ", "")
                        currency_match = re.match(r"(US\$|USD|\$|€|£|₦)([\d,.]+)(million|billion|m|bn|b)?", amount_text, re.IGNORECASE)
                        if currency_match:
                            symbol, number, scale = currency_match.groups()
                            multiplier = {"million": 1_000_000, "m": 1_000_000, "billion": 1_000_000_000, "bn": 1_000_000_000, "b": 1_000_000_000}.get((scale or "").lower(), 1)
                            amount = float(number.replace(",", "")) * multiplier
                            currency = "USD" if symbol.upper() in {"US$", "USD", "$"} else {"€": "EUR", "£": "GBP", "₦": "NGN"}.get(symbol, None)
                            deal["amount"] = amount
                            deal["currency"] = currency
                            deal["amount_usd"] = amount if currency == "USD" else None
                deal["announced"] = item.get("published_day") or deal.get("announced")
                # Fund labels are often broad; don't infer a fund's sector from LP examples in the article.
                focused_sector = classify_sector(title if deal.get("deal_type") == "fund_raise" else " ".join((title, lead[:400])))
                deal["sector"] = focused_sector
                candidates.append({
                    "deal": deal, "freshness_status": item["freshness_status"],
                    "source": {"title": title[:500], "url": item["url"], "publisher": item["publisher"],
                               "published_date": item.get("publisher_published_day") or item.get("published_day"),
                               "date_basis": item.get("date_basis", "unknown"),
                               "matched_queries": item["matched_queries"],
                               "page_fetch_status": item["page_fetch_status"],
                               "evidence_excerpt": item.get("page_evidence_excerpt", "")[:1200]},
                    "review_status": "candidate_pending_database_deduplication",
                })
            except Exception as exc:
                kind = type(exc).__name__
                fetch_errors[kind] = fetch_errors.get(kind, 0) + 1
                item["page_fetch_status"] = f"error_{kind}"

    candidates = classify_candidates(candidates, database, same_transaction)
    for candidate in candidates:
        if candidate["review_status"] == "potentially_new_needs_human_review":
            related = related_fund_matches(candidate["deal"], database)
            if related:
                candidate["review_status"] = "possible_related_fund_record_needs_human_review"
                candidate["related_existing_records"] = [
                    {"id": row.get("id"), "announced": row.get("announced"), "headline": row.get("headline"),
                     "target": row.get("target"), "amount_usd": row.get("amount_usd")}
                    for row in related[:5]
                ]
    summary = {
        "queries_succeeded": sum(q["status"] == "ok" for q in query_stats),
        "queries_failed": sum(q["status"] != "ok" for q in query_stats),
        "unique_urls_discovered": len(results),
        "recent_dated_urls": len(recent_results),
        "source_pages_attempted": min(len(recent_results), args.max_pages),
        "source_pages_fetched": sum(r["page_fetch_status"] == "fetched" for r in recent_results[:args.max_pages]),
        "fetch_errors_by_type": fetch_errors,
        "production_database_records_loaded": len(database),
        "deal_candidates_extracted": len(candidates),
        "matched_existing_database": sum(c["review_status"] == "already_in_database" for c in candidates),
        "potentially_new_for_human_review": sum(c["review_status"] == "potentially_new_needs_human_review" for c in candidates),
        "possible_related_fund_records": sum(c["review_status"] == "possible_related_fund_record_needs_human_review" for c in candidates),
        "needs_date_confirmation": sum(c["review_status"] == "needs_date_confirmation" for c in candidates),
        "not_eligible_as_new": sum(c["review_status"] == "not_eligible_as_new" for c in candidates),
        "elapsed_seconds": round(time.monotonic() - start, 2),
    }
    # Only source pages fetched successfully and confirmed recent are eligible for candidate output.
    output = {
        "schema_version": 2, "run_at_utc": datetime.now(timezone.utc).isoformat(),
        "mode": "review_only_no_production_writes",
        "freshness_policy": {"days": args.freshness_days, "cutoff": since.isoformat(),
                             "only_recent_publisher_pages_can_be_labelled_potentially_new": True},
        "database_comparison": {"api_base_url": api_base, "records_loaded": len(database),
                                "dataset_scope": "complete visible canonical product deals from /api/deals",
                                "complete_visible_snapshot_required": True, "writes_performed": False},
        "limits": {"queries": len(queries), "max_results_per_query": MAX_RESULTS_PER_QUERY,
                   "max_source_pages_fetched": args.max_pages, "proxy_configured": bool(proxy_url),
                   "proxy_credentials_written": False, "raw_html_written": False},
        "summary": summary, "queries": query_stats, "results": results, "deal_candidates": candidates,
        "limitations": [
            "A potentially-new label means no match was found in the complete visible canonical product deal set; it is not analyst-verified.",
            "The public API does not expose hidden or merged audit-only rows; their canonical visible records should be present in the product set.",
            "The pilot does not write to production. Human review is required before ingestion.",
            "Recent publisher article dates do not override older transaction milestone dates explicitly stated in the article.",
            "Fund-close milestones are compared against same-fund records within a one-year window and may require analyst review.",
            "Only candidates with African party or market evidence are eligible; broad Africa-focused searches can return unrelated foreign deals.",
            "Only recent candidates with publisher-declared dates can be labelled potentially new; search-index dates require confirmation.",
            "Search snippets alone are never sufficient evidence to create a deal candidate.",
            "This bounded discovery run is not comprehensive Bloomberg-equivalent coverage.",
        ],
    }
    out = Path(args.output)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(output, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"summary": summary, "proxy_configured": bool(proxy_url), "output": str(out)}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
