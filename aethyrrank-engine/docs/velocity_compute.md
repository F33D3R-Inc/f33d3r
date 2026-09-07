# Content Velocity Compute Service

AethyrRank consumes `velocity_score`, `early_retention`, and `completion_rate`
as pre-computed fields on `ContentItem`. This document describes how the
content-service should compute them before calling `/rank`.

## velocity_score — d(engagement)/dt

Computed over a **sliding 1-hour window**, normalised to [0, 1].

```
raw_velocity = (Σ weighted_events in [t-1h, t]) / (Σ weighted_events in [t-25h, t-24h])

velocity_score = sigmoid((raw_velocity - 1.0) * 2.0)
```

- `raw_velocity > 1.0` → accelerating (score > 0.5)
- `raw_velocity = 1.0` → stable (score ≈ 0.5)
- `raw_velocity < 1.0` → decelerating (score < 0.5)

Event weights (use same taxonomy as reward_from_event in linucb.rs):
```
save:    4.0    share:   3.0    comment: 2.5
like:    1.5    click:   1.0    impression: 0.2
skip:   -1.0    negative_feedback: -3.0
```

### Implementation note

This should run as a background job (e.g. every 5 minutes) updating a
Redis key `velocity:{content_id}` with the current score. The content-service
reads from Redis before building the `/rank` payload.

## early_retention

Fraction of users who watched the first 3 seconds (video) or scrolled past
the first fold (article) before moving on.

```
early_retention = views_past_3s / total_impressions
```

Clip to [0, 1]. Use a 24-hour rolling window.

## completion_rate

Fraction of users who reached the end of the content.

```
completion_rate = completions / total_starts
```

"Completion" for video = watched ≥ 90% of duration.
"Completion" for article = scrolled to end (estimated via scroll depth).

## Lifecycle phases (used in explanation strings)

| Phase | Condition |
|-------|-----------|
| `TestCohort` | `exposure_count < 500` |
| `Expanding` | `composite_velocity ≥ 0.70` |
| `Viral` | `composite_velocity ≥ 0.90` |
| `Stable` | `0.20 < composite_velocity < 0.70` |
| `Decaying` | `composite_velocity ≤ 0.20` |

`composite_velocity = 0.50 × velocity_score + 0.25 × early_retention + 0.25 × completion_rate`

## Promotion logic (content-service responsibility)

When `velocity_score > 0.70`:
1. Expand test cohort → wider audience segment
2. Increase candidate pool inclusion probability in retrieval stage
3. Flag as `Expanding` in content metadata

When `velocity_score > 0.90`:
1. Push to exploration pools across all surfaces
2. Flag as `Viral`

When `velocity_score < 0.20` for 3 consecutive windows:
1. Reduce inclusion probability in retrieval
2. Flag as `Decaying`
3. After 7 days of `Decaying`, archive from active pool
