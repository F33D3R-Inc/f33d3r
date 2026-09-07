"""
Event export script — pulls raw feedback events and constructs clean
training examples for the neural ranking model.

Usage:
    python export_events.py --start 2025-01-01 --out data/events.parquet

Reads from Postgres (configure via DB_URL env var).
Applies exposure debiasing and intent stability filtering.
Outputs a Parquet file compatible with train.py.
"""

import argparse
import os
import math
import json
import pandas as pd
import numpy as np
from datetime import datetime


# ── Reward taxonomy (mirrors src/bandit/linucb.rs) ────────────────────────────

REWARD_MAP = {
    "save":              1.0,
    "share":             0.9,
    "comment":           0.8,
    "like":              0.6,
    "view_complete":     0.5,
    "click":             0.4,
    "impression":        0.1,
    "skip":             -0.2,
    "negative_feedback":-1.0,
    "purchase":          1.0,
}

def position_discount(position: int) -> float:
    """Mirrors position_discount in handlers.rs."""
    return 1.0 / (1.0 + math.log(max(position, 1)))

def reward_signal(event_type: str, position: int) -> float:
    r = REWARD_MAP.get(event_type, 0.0)
    return r * position_discount(position)


# ── Exposure debiasing ────────────────────────────────────────────────────────

def compute_sample_weights(df: pd.DataFrame) -> pd.Series:
    """
    Inverse propensity weighting: w_i = 1 / P(item | user, t)
    Approximated as: w_i = 1 / (item_exposure_count / total_impressions)

    Clips to [0.1, 10.0] to prevent extreme weights destabilising training.
    """
    total = df["exposure_count"].sum()
    if total == 0:
        return pd.Series(1.0, index=df.index)
    p_item = df["exposure_count"] / total
    w = 1.0 / p_item.clip(lower=1e-4)
    return w.clip(lower=0.1, upper=10.0)


# ── Volatility filter ─────────────────────────────────────────────────────────

def compute_volatility(df: pd.DataFrame) -> pd.Series:
    """
    V(t) = |z(t) - z(t-1)| for the user's interest vector over time.
    Approximated here using the standard deviation of reward within a session.
    Replace with proper latent-state diff once user embeddings are available.
    """
    session_std = df.groupby("session_id")["reward"].transform("std").fillna(0.0)
    return session_std


# ── Intent stability filter ───────────────────────────────────────────────────

def compute_intent_stability(df: pd.DataFrame) -> pd.Series:
    """
    I(t) = cosine(z(t), z(t-k))
    Approximated as consistency of engagement polarity within a session window.
    Replace with cosine similarity of user embedding snapshots when available.
    """
    def session_stability(group):
        r = group["reward"].values
        if len(r) < 2:
            return pd.Series(1.0, index=group.index)
        # Fraction of events with same sign as session mean
        mean_sign = np.sign(r.mean())
        stability = np.mean(np.sign(r) == mean_sign)
        return pd.Series(stability, index=group.index)

    return df.groupby("session_id", group_keys=False).apply(session_stability)


# ── Main export ───────────────────────────────────────────────────────────────

def export(start_date: str, output_path: str, db_url: str):
    """
    In production: replace the mock DataFrame below with a SQLAlchemy query
    against your feedback event store.

    Schema expected from DB:
        user_id, session_id, content_id, event_type, position_at_display,
        timestamp, user_features (json array), content_features (json array),
        context_features (json array), session_features (json array of arrays),
        exposure_count
    """
    print(f"Exporting events since {start_date}...")

    # ── Production query (replace mock below) ────────────────────────────────
    # import sqlalchemy as sa
    # engine = sa.create_engine(db_url)
    # df = pd.read_sql("""
    #     SELECT * FROM feedback_events
    #     WHERE timestamp >= %(start)s
    # """, engine, params={"start": start_date})

    # ── Mock data for local testing ───────────────────────────────────────────
    rng = np.random.default_rng(42)
    n   = 10_000
    event_types = list(REWARD_MAP.keys())
    df  = pd.DataFrame({
        "user_id":          [f"u{i%500}"       for i in range(n)],
        "session_id":       [f"s{i//20}"       for i in range(n)],
        "content_id":       [f"c{i%2000}"      for i in range(n)],
        "event_type":       rng.choice(event_types, n),
        "position_at_display": rng.integers(1, 51, n),
        "exposure_count":   rng.integers(100, 100_000, n),
        "user_features":    [rng.random(7).tolist() for _ in range(n)],
        "content_features": [rng.random(7).tolist() for _ in range(n)],
        "context_features": [rng.random(8).tolist() for _ in range(n)],
        "session_features": [
            [rng.random(7).tolist() for _ in range(rng.integers(1, 21))]
            for _ in range(n)
        ],
    })
    # ── End mock ──────────────────────────────────────────────────────────────

    # Compute reward signal
    df["reward"] = df.apply(
        lambda r: reward_signal(r["event_type"], r["position_at_display"]), axis=1
    )

    # Compute clean-data filters
    df["volatility"]        = compute_volatility(df)
    df["intent_stability"]  = compute_intent_stability(df)
    df["sample_weight"]     = compute_sample_weights(df)

    # Normalise sample weights
    df["sample_weight"] = df["sample_weight"] / df["sample_weight"].mean()

    os.makedirs(os.path.dirname(output_path) or ".", exist_ok=True)
    df.to_parquet(output_path, index=False)
    print(f"Exported {len(df)} events → {output_path}")
    print(f"  reward range:  [{df['reward'].min():.2f}, {df['reward'].max():.2f}]")
    print(f"  mean weight:   {df['sample_weight'].mean():.2f}")
    print(f"  volatility p95:{df['volatility'].quantile(0.95):.2f}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--start", required=True,  help="Start date YYYY-MM-DD")
    parser.add_argument("--out",   required=True,  help="Output parquet path")
    parser.add_argument("--db",    default=os.getenv("DB_URL", ""), help="DB connection URL")
    args   = parser.parse_args()
    export(args.start, args.out, args.db)
