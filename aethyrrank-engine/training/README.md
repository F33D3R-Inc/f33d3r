# AethyrRank — Neural Model Training Pipeline

This directory contains the training pipeline for the neural engagement prediction model that powers Stage 2 of the AethyrRank 7-stage pipeline.

The current `src/pipeline/neural.rs` uses a high-quality heuristic stub that produces a realistic score distribution. Replace it with a trained ONNX model when sufficient behavioral data is available.

---

## Target model

**Architecture:** Wide & Deep with optional session Transformer  
**Output:** `P(meaningful_engagement | user, content, context)` ∈ [0, 1]

### Inputs

| Feature group | Dim | Notes |
|---|---|---|
| User embedding | 128 | Learned from interaction history |
| Content embedding | 128 | Learned from topic/tag/engagement |
| Session history | 20 × 128 | Last 20 interactions (Transformer input) |
| Context features | 8 | Time of day, device type, session depth, realm level |
| AESQ alignment | 1 | Pre-computed psychological alignment score |

### Architecture

```
Embedding Layer (user + content, dim=128)
    ↓
Feature Cross (wide — memorisation of frequent co-occurrence patterns)
    ↓
Deep MLP: [512, 256, 128] with ReLU + Dropout(0.3)
    ↓
Optional Transformer: 2 layers, 4 heads, causal attention on session history
    ↓
Sigmoid output → score ∈ [0, 1]
```

---

## Training data

Events are recorded to `feedback_events` table in `f33d3r_feed` via the AethyrRank `POST /feedback` endpoint.

Each training example is a `(user_state, content_item, context, reward)` tuple where reward is a weighted combination of engagement signals.

### Clean data construction

Raw events are filtered before training to remove bias:

**1. Exposure debiasing**
```
w_i = 1 / P(item | user, t)
```
Removes algorithm self-reinforcement. Items that appeared because AethyrRank showed them are downweighted proportionally to how likely they were to be shown.

**2. Volatility filter**
```
V(t) = |z(t) - z(t-1)|
```
High-volatility sessions (outrage spirals, novelty binges, doomscroll bursts) are downweighted. Removes reactions driven by session state rather than genuine preference.

**3. Intent stability filter**
```
I(t) = cosine(z(t), z(t-k))
```
Only train on sequences where user interest is stable across the last `k` steps. Exploration noise is excluded — LinUCB handles cold start, the neural model trains on stable signal.

**Clean dataset:**
```
D_clean = Σ w_i · 1[stable_intent] · e_i
```

---

## Training

Requirements: Python 3.10+, `torch >= 2.0`, `transformers`, `pandas`, `pyarrow`

```bash
# Install dependencies
pip install torch transformers pandas scikit-learn pyarrow

# Export training data from Postgres
python export_events.py \
  --db-url postgres://f33d3r:password@localhost:5432/f33d3r_feed \
  --start 2026-01-01 \
  --out data/events.parquet

# Train
python train.py \
  --data data/events.parquet \
  --output models/neural_ranker_v1 \
  --epochs 20 \
  --batch-size 2048

# Export to ONNX for production serving
python export_onnx.py \
  --model models/neural_ranker_v1 \
  --output models/neural_ranker_v1.onnx \
  --quantize int8
```

---

## Wiring the trained model into AethyrRank

Replace the heuristic block in `src/pipeline/neural.rs`:

```rust
// Replace the stub score_item() implementation with:
let input = build_inference_input(user, item, session);
let response = onnx_runtime.run(&input).await?;
let score = response.outputs[0].as_slice::<f32>()[0] as f64;
```

Recommended serving stack:
- **In-process:** `ort` crate (ONNX Runtime) — lowest latency, simplest deployment
- **GPU batch:** Triton Inference Server — for high-throughput multi-replica deployments

Quantise to int8 or fp16 before deployment to meet the 15ms latency budget for Stage 2.

---

## Evaluation metrics

| Metric | Target |
|--------|--------|
| NDCG@10 | > 0.82 |
| AUC-ROC | > 0.88 |
| P(engagement) calibration (ECE) | < 0.05 |
| p99 inference latency | < 15ms |

Retrain immediately if NDCG@10 drops > 5% on the held-out validation set.

---

## Retraining cadence

- **Online (current):** Incremental updates via LinUCB from the `POST /feedback` stream
- **Batch retrain:** Weekly on the last 30 days of clean filtered events
- **Trigger-based:** Retrain when NDCG@10 drops > 5% on validation
- **Cold start:** New users and new surfaces are handled entirely by LinUCB until sufficient data accumulates (roughly 1,000 events per user)

---

## Data schema reference

```json
{
  "user_id":           "uuid",
  "content_id":        "uuid",
  "surface":           "feed",
  "event_type":        "view_complete",
  "position":          3,
  "dwell_ms":          14200,
  "is_explore":        false,
  "session_id":        "uuid",
  "created_at":        "2026-04-23T14:01:10Z",
  "interest_vector":   [0.2, 0.8, 0.1, 0.4, 0.6, 0.3, 0.5, 0.7],
  "topic_vector":      [0.3, 0.6, 0.1, 0.5, 0.4, 0.2, 0.8, 0.3],
  "aesq_alignment":    0.91,
  "velocity_score":    0.82,
  "realm_level":       2
}
```
