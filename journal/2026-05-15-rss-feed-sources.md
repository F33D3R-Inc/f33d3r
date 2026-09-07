# Right Rail — RSS & API Feed Sources
**Date:** 2026-05-15
**Status:** Phase 2 — not yet built. Phase 1 (internal platform intelligence) ships first.

These are the approved feeds to integrate once the RSS ingestion pipeline exists.
Architecture: background poller in Nantar → Redis cache (5-min TTL) → right rail partial.
Do NOT pull RSS directly in templates.

---

## News
- Reuters: https://www.reutersagency.com/feed/
- BBC World: https://feeds.bbci.co.uk/news/world/rss.xml
- AP Top News: https://apnews.com/hub/ap-top-news (scrape or find RSS endpoint)

## Music
- Billboard: https://www.billboard.com/feed/
- Pitchfork: https://pitchfork.com/rss/news/
- Resident Advisor: https://ra.co/rss/news

## Stocks / Finance
- Bloomberg Markets: https://feeds.bloomberg.com/markets/news.rss
- CNBC Markets: https://www.cnbc.com/id/100003114/device/rss/rss.html
- MarketWatch: https://feeds.marketwatch.com/marketwatch/topstories/

## Cryptocurrency
- CoinDesk: https://www.coindesk.com/arc/outboundfeeds/rss/
- Cointelegraph: https://cointelegraph.com/rss
- Decrypt: https://decrypt.co/feed

## Sports
- ESPN: https://www.espn.com/espn/rss/news
- Yahoo Sports: https://sports.yahoo.com/rss/
- CBS Sports: https://www.cbssports.com/rss/headlines/

## Forex
- ForexLive: https://www.forexlive.com/feed/news
- DailyFX: https://www.dailyfx.com/feeds/market-news
- FXStreet: https://www.fxstreet.com/rss/news

## Weather (APIs, not RSS)
- OpenWeatherMap: https://openweathermap.org/api
- Tomorrow.io: https://www.tomorrow.io/weather-api/
- NOAA: https://www.weather.gov/documentation/services-web-api

---

## Notes
- Verify each feed URL is live before building the poller — some may have changed
- Bloomberg and AP may require API keys or have rate limits
- Weather needs user location signal — decide on IP geolocation vs. user-set location preference first
- Music feeds (Billboard, Pitchfork, RA) are the highest-value for F33D3R's core audience
- Crypto feeds are contextual — show on wallet/finance pages, not all pages
- All feeds normalize into RailItem struct before rendering
