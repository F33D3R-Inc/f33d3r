"""
AethyrRank Neural Ranking Model — Training Script
PyTorch implementation of Wide & Deep with optional Transformer session encoder.

Usage:
    python train.py --data data/events.parquet --output models/neural_ranker_v1

Requirements:
    pip install torch transformers pandas scikit-learn pyarrow
"""

import argparse
import math
import os
from dataclasses import dataclass, field
from typing import List, Optional

import pandas as pd
import torch
import torch.nn as nn
import torch.nn.functional as F
from torch.utils.data import DataLoader, Dataset
from sklearn.model_selection import train_test_split


# ── Config ────────────────────────────────────────────────────────────────────

@dataclass
class TrainConfig:
    # Model
    user_emb_dim:     int   = 128
    content_emb_dim:  int   = 128
    mlp_layers:       List  = field(default_factory=lambda: [512, 256, 128])
    dropout:          float = 0.30
    use_transformer:  bool  = True
    session_len:      int   = 20
    n_heads:          int   = 4
    n_tf_layers:      int   = 2
    # Training
    batch_size:       int   = 2048
    epochs:           int   = 20
    lr:               float = 1e-3
    weight_decay:     float = 1e-5
    warmup_steps:     int   = 500
    # Data
    feature_dim:      int   = 7     # AESQ feature vector dim
    context_dim:      int   = 8     # time_of_day, device, session_depth, etc.
    # Clean data filters
    volatility_threshold:   float = 0.35
    intent_stability_min:   float = 0.60


# ── Dataset ───────────────────────────────────────────────────────────────────

class RankingDataset(Dataset):
    """
    Each row in the DataFrame represents one (user, content, context, reward) event.

    Expected columns:
        user_features:    list[float]  len=feature_dim  (AESQ features for user)
        content_features: list[float]  len=feature_dim
        context_features: list[float]  len=context_dim
        session_features: list[list[float]]  shape=(session_len, feature_dim)
        reward:           float  (from EventType reward mapping, position-discounted)
        sample_weight:    float  (exposure debiasing × intent stability)
    """

    def __init__(self, df: pd.DataFrame, cfg: TrainConfig):
        self.cfg = cfg
        # Apply clean data filters
        df = self._apply_volatility_filter(df)
        df = self._apply_intent_stability_filter(df)
        self.df = df.reset_index(drop=True)

    def _apply_volatility_filter(self, df: pd.DataFrame) -> pd.DataFrame:
        if "volatility" not in df.columns:
            return df
        mu_v = df["volatility"].mean()
        return df[df["volatility"] <= mu_v * (1.0 + self.cfg.volatility_threshold)]

    def _apply_intent_stability_filter(self, df: pd.DataFrame) -> pd.DataFrame:
        if "intent_stability" not in df.columns:
            return df
        return df[df["intent_stability"] >= self.cfg.intent_stability_min]

    def __len__(self):
        return len(self.df)

    def __getitem__(self, idx):
        row = self.df.iloc[idx]

        user_feat    = torch.tensor(row["user_features"],    dtype=torch.float32)
        content_feat = torch.tensor(row["content_features"], dtype=torch.float32)
        ctx_feat     = torch.tensor(row["context_features"], dtype=torch.float32)
        reward       = torch.tensor(float(row["reward"]),    dtype=torch.float32)
        weight       = torch.tensor(float(row.get("sample_weight", 1.0)), dtype=torch.float32)

        # Session history: pad or truncate to session_len
        raw_session  = row.get("session_features", [])
        session_feat = self._pad_session(raw_session)

        return {
            "user":    user_feat,
            "content": content_feat,
            "ctx":     ctx_feat,
            "session": session_feat,
            "reward":  reward,
            "weight":  weight,
        }

    def _pad_session(self, session: list) -> torch.Tensor:
        sl   = self.cfg.session_len
        fdim = self.cfg.feature_dim
        t    = torch.zeros(sl, fdim, dtype=torch.float32)
        if session:
            items = session[-sl:]  # take most recent
            for i, s in enumerate(items):
                t[i] = torch.tensor(s, dtype=torch.float32)
        return t


# ── Model ─────────────────────────────────────────────────────────────────────

class AethyrRankModel(nn.Module):
    """
    Wide & Deep neural ranking model with optional Transformer session encoder.

    Output: scalar P(engagement | u, i, ctx) ∈ [0, 1]
    """

    def __init__(self, cfg: TrainConfig):
        super().__init__()
        self.cfg = cfg

        input_dim = cfg.user_emb_dim + cfg.content_emb_dim + cfg.context_dim

        # Wide component: feature crosses for memorisation
        self.wide = nn.Linear(input_dim, 1, bias=True)

        # Deep component: MLP for generalisation
        layers = []
        in_d = input_dim
        for out_d in cfg.mlp_layers:
            layers += [
                nn.Linear(in_d, out_d),
                nn.LayerNorm(out_d),
                nn.ReLU(),
                nn.Dropout(cfg.dropout),
            ]
            in_d = out_d
        self.deep = nn.Sequential(*layers)

        # Session Transformer encoder
        if cfg.use_transformer:
            encoder_layer = nn.TransformerEncoderLayer(
                d_model    = cfg.feature_dim,
                nhead      = cfg.n_heads,
                dim_feedforward = cfg.feature_dim * 4,
                dropout    = cfg.dropout,
                batch_first = True,
            )
            self.transformer = nn.TransformerEncoder(encoder_layer, num_layers=cfg.n_tf_layers)
            self.session_proj = nn.Linear(cfg.feature_dim, cfg.user_emb_dim)
        else:
            self.transformer  = None
            self.session_proj = None

        # User and content projections (input features → embedding dims)
        self.user_proj    = nn.Linear(cfg.feature_dim, cfg.user_emb_dim)
        self.content_proj = nn.Linear(cfg.feature_dim, cfg.content_emb_dim)

        # Final combination: wide + deep → sigmoid
        self.output = nn.Linear(cfg.mlp_layers[-1] + 1, 1)

    def forward(
        self,
        user:    torch.Tensor,   # (B, feature_dim)
        content: torch.Tensor,   # (B, feature_dim)
        ctx:     torch.Tensor,   # (B, context_dim)
        session: torch.Tensor,   # (B, session_len, feature_dim)
    ) -> torch.Tensor:           # (B,) ∈ [0, 1]

        u = self.user_proj(user)      # (B, user_emb_dim)
        c = self.content_proj(content) # (B, content_emb_dim)

        # Session encoding via Transformer
        if self.transformer is not None:
            # Causal mask: each position only attends to previous positions
            sl   = session.size(1)
            mask = torch.triu(torch.ones(sl, sl, device=session.device), diagonal=1).bool()
            s    = self.transformer(session, mask=mask)     # (B, sl, feature_dim)
            s    = s[:, -1, :]                              # take last token
            u    = u + self.session_proj(s)                 # residual add to user repr

        combined = torch.cat([u, c, ctx], dim=-1)  # (B, input_dim)

        wide_out = self.wide(combined)              # (B, 1)
        deep_out = self.deep(combined)              # (B, mlp_layers[-1])

        logit = self.output(torch.cat([deep_out, wide_out], dim=-1))  # (B, 1)
        return torch.sigmoid(logit).squeeze(-1)     # (B,)


# ── Training loop ─────────────────────────────────────────────────────────────

def train(cfg: TrainConfig, data_path: str, output_path: str):
    device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
    print(f"Training on {device}")

    # Load and split data
    df             = pd.read_parquet(data_path)
    train_df, val_df = train_test_split(df, test_size=0.10, random_state=42)

    train_ds = RankingDataset(train_df, cfg)
    val_ds   = RankingDataset(val_df,   cfg)

    train_loader = DataLoader(train_ds, batch_size=cfg.batch_size, shuffle=True,  num_workers=4)
    val_loader   = DataLoader(val_ds,   batch_size=cfg.batch_size, shuffle=False, num_workers=4)

    model     = AethyrRankModel(cfg).to(device)
    optimizer = torch.optim.AdamW(model.parameters(), lr=cfg.lr, weight_decay=cfg.weight_decay)

    total_steps = len(train_loader) * cfg.epochs
    scheduler   = torch.optim.lr_scheduler.OneCycleLR(
        optimizer, max_lr=cfg.lr, total_steps=total_steps, pct_start=0.05,
    )

    best_val_loss = float("inf")

    for epoch in range(1, cfg.epochs + 1):
        # ── Train ──────────────────────────────────────────────────────────────
        model.train()
        train_loss = 0.0
        for batch in train_loader:
            user    = batch["user"].to(device)
            content = batch["content"].to(device)
            ctx     = batch["ctx"].to(device)
            session = batch["session"].to(device)
            reward  = batch["reward"].to(device)
            weight  = batch["weight"].to(device)

            pred = model(user, content, ctx, session)

            # Weighted BCE loss (sample weights from exposure debiasing)
            loss = F.binary_cross_entropy(pred, reward.clamp(0, 1), weight=weight)

            optimizer.zero_grad()
            loss.backward()
            nn.utils.clip_grad_norm_(model.parameters(), max_norm=1.0)
            optimizer.step()
            scheduler.step()

            train_loss += loss.item()

        # ── Validate ───────────────────────────────────────────────────────────
        model.eval()
        val_loss = 0.0
        with torch.no_grad():
            for batch in val_loader:
                pred = model(
                    batch["user"].to(device),
                    batch["content"].to(device),
                    batch["ctx"].to(device),
                    batch["session"].to(device),
                )
                reward = batch["reward"].to(device)
                weight = batch["weight"].to(device)
                val_loss += F.binary_cross_entropy(pred, reward.clamp(0, 1), weight=weight).item()

        avg_train = train_loss / len(train_loader)
        avg_val   = val_loss   / len(val_loader)
        print(f"Epoch {epoch:3d}/{cfg.epochs}  train={avg_train:.4f}  val={avg_val:.4f}")

        if avg_val < best_val_loss:
            best_val_loss = avg_val
            os.makedirs(output_path, exist_ok=True)
            torch.save(model.state_dict(), os.path.join(output_path, "model_best.pt"))
            print(f"  ✓ checkpoint saved (val_loss={best_val_loss:.4f})")

    print(f"\nTraining complete. Best val loss: {best_val_loss:.4f}")
    print(f"Checkpoint: {output_path}/model_best.pt")


# ── ONNX export ───────────────────────────────────────────────────────────────

def export_onnx(cfg: TrainConfig, model_path: str, output_path: str):
    """Export trained model to ONNX for production serving."""
    import torch.onnx

    model = AethyrRankModel(cfg)
    model.load_state_dict(torch.load(model_path, map_location="cpu"))
    model.eval()

    # Dummy inputs for tracing
    B = 1
    dummy = {
        "user":    torch.zeros(B, cfg.feature_dim),
        "content": torch.zeros(B, cfg.feature_dim),
        "ctx":     torch.zeros(B, cfg.context_dim),
        "session": torch.zeros(B, cfg.session_len, cfg.feature_dim),
    }

    torch.onnx.export(
        model,
        (dummy["user"], dummy["content"], dummy["ctx"], dummy["session"]),
        output_path,
        input_names  = ["user", "content", "ctx", "session"],
        output_names = ["score"],
        dynamic_axes = {
            "user":    {0: "batch"},
            "content": {0: "batch"},
            "ctx":     {0: "batch"},
            "session": {0: "batch"},
            "score":   {0: "batch"},
        },
        opset_version = 17,
    )
    print(f"ONNX model exported to {output_path}")


# ── Entry point ───────────────────────────────────────────────────────────────

if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--data",   required=True,  help="Path to events.parquet")
    parser.add_argument("--output", required=True,  help="Output directory for checkpoint")
    parser.add_argument("--onnx",   default=None,   help="If set, also export ONNX to this path")
    parser.add_argument("--no-transformer", action="store_true")
    args = parser.parse_args()

    cfg = TrainConfig(use_transformer=not args.no_transformer)
    train(cfg, args.data, args.output)

    if args.onnx:
        export_onnx(
            cfg,
            model_path  = os.path.join(args.output, "model_best.pt"),
            output_path = args.onnx,
        )
