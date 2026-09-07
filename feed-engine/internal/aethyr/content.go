// Package aethyr is the client side of the AethyrRank contract: the HTTP
// client (client.go) and the translation from a works-lane candidate into
// the ContentItem the engine scores (this file).
//
// Nothing in this package invents a signal. Every field on the wire item is
// either read off the work or off a batched query the handler ran over the
// candidate window; where the platform has no measurement for a field yet
// the value written is the honest zero, not a plausible-looking constant.
package aethyr

import (
	"math"
	"strings"
	"time"

	"github.com/f33d3r/feed-engine/internal/model"
)

// WorkSignals are the per-candidate measurements that do not live on the
// work row itself and are batched over the window by the handler.
type WorkSignals struct {
	// ViewTimeSeconds is the mean dwell recorded in feedback_events.
	ViewTimeSeconds float64
	// RecentReactions is the count of reactions inside the velocity window.
	RecentReactions int
	// PostsLast24h is how many works the author published in the last day.
	PostsLast24h int
	// SelfReply is true when the author replied to their own work within
	// thirty minutes of publishing it.
	SelfReply bool
	// CreatorExposure is the author's total recent views across their works.
	CreatorExposure uint64
	// InNetwork is true when the viewer follows the author.
	InNetwork bool
}

// velocityWindow is the trailing period RecentReactions is counted over.
// Exported so the handler counts over exactly the window this file assumes.
const VelocityWindow = 6 * time.Hour

// WorkToContent translates a candidate work into the engine's content item.
// The work must already carry its PsychVector; the handler ensures that
// before building the request.
func WorkToContent(w *model.Work, s WorkSignals) model.AethyrContent {
	likes := float64(w.LikeCount)
	reposts := float64(w.RepostCount)
	replies := float64(w.ReplyCount)
	saves := float64(w.BookmarkCount)

	// A reaction implies an impression. The view counter only moves when a
	// view_time event lands, so a work can carry reactions and zero recorded
	// views; the impression floor keeps the engagement rate finite and keeps
	// a brand-new work (no views, no reactions) from being dropped by the
	// pre-ranker as zero-quality — one impression with no reactions is the
	// neutral rate, not a failing one.
	reactions := likes + reposts + replies + saves
	impressions := math.Max(float64(w.ViewCount), reactions)
	if impressions < 1 {
		impressions = 1
	}

	// Velocity: reactions in the trailing window, saturating so five
	// reactions in six hours reads as clearly moving and fifty as viral.
	velocity := 1.0 - math.Exp(-float64(s.RecentReactions)/5.0)

	selfReply := 0.0
	if s.SelfReply {
		selfReply = 1.0
	}

	// Revenue priors are declared as priors: a subscriber-only work is the
	// only conversion surface the works lane has, and only a creator account
	// can be paid.
	conversion := 0.0
	if w.SubscriberOnly {
		conversion = 0.30
	}
	creatorRate, ltv := 0.0, 0.0
	if w.AuthorIsCreator {
		creatorRate = 0.20
		ltv = 0.10
	}

	adult := 0.0
	if w.IsNSFW || w.IsGore || w.AuthorIsAdultCreator {
		adult = 1.0
	}

	creatorExposure := s.CreatorExposure
	if creatorExposure < uint64(impressions) {
		creatorExposure = uint64(impressions)
	}

	return model.AethyrContent{
		ContentID:       w.ID,
		CreatorID:       w.AuthorID,
		TopicVector:     w.PsychVector,
		PublishedAt:     w.CreatedAt,
		ExposureCount:   uint64(w.ViewCount),
		CreatorExposure: creatorExposure,
		Tags:            w.Tags,
		ContentType:     ContentTypeOf(w),
		VelocityScore:   velocity,
		// No retention or completion measurement exists for works yet. Zero
		// is uniform across the pool, so it moves no work relative to another.
		EarlyRetention:        0,
		CompletionRate:        0,
		ConversionProbability: conversion,
		CreatorRevenueRate:    creatorRate,
		LtvEstimate:           ltv,
		AdultProbability:      adult,
		PostsLast24h:          uint64(s.PostsLast24h),
		SelfReplyCadence:      selfReply,
		CharCount:             uint64(len([]rune(w.Body))),
		IsInNetwork:           s.InNetwork,
		Engagement: model.AethyrEngagement{
			Likes:           likes,
			Shares:          reposts,
			Comments:        replies,
			Saves:           saves,
			ViewTimeSeconds: s.ViewTimeSeconds,
			Impressions:     impressions,
		},
	}
}

// ContentTypeOf maps a work's kind and media onto the engine's content_type
// vocabulary (text | image | video | audio).
func ContentTypeOf(w *model.Work) string {
	switch strings.ToLower(w.Kind) {
	case "video", "react_video":
		return "video"
	case "voice":
		return "audio"
	}
	if w.VideoMasterURL != "" {
		return "video"
	}
	if w.VoiceURL != "" {
		return "audio"
	}
	if len(w.MediaURLs) > 0 || strings.EqualFold(w.ContentType, "image") {
		return "image"
	}
	return "text"
}
