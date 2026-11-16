// Command import-subscriptions moves feed-engine's own plans and subscriptions
// into Themis, the commerce authority, once.
//
// It reads every creator_plans and subscriptions row whose people have
// identities and sends them to Themis's POST /v1/admin/import/subscriptions in
// batches, keeping their ids. Themis inserts what it does not already hold and
// announces each new row on commerce.events, which re-projects the same rows
// here. The import is idempotent: run it again and nothing changes.
//
// It verifies counts: Themis answers how many of the ids sent it now holds, and
// the command fails unless that is every one. The run and its counts are kept
// in commerce_import_runs.
//
//	import-subscriptions                # DATABASE_URL, THEMIS_URL, INTERNAL_API_KEY from the environment
//	import-subscriptions -dry-run       # read and count only
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
)

const batch = 500

type importPlan struct {
	ID             string    `json:"id"`
	CreatorAccount string    `json:"creator_account"`
	CreatorPIAL    string    `json:"creator_pial"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	PriceUnits     int64     `json:"price_units"`
	IsActive       bool      `json:"is_active"`
	CreatedAt      time.Time `json:"created_at"`
}

type importSubscription struct {
	ID                string     `json:"id"`
	SubscriberAccount string     `json:"subscriber_account"`
	SubscriberPIAL    string     `json:"subscriber_pial"`
	CreatorAccount    string     `json:"creator_account"`
	CreatorPIAL       string     `json:"creator_pial"`
	PlanID            *string    `json:"plan_id"`
	Status            string     `json:"status"`
	PeriodEnd         time.Time  `json:"period_end"`
	CancelledAt       *time.Time `json:"cancelled_at"`
	CreatedAt         time.Time  `json:"created_at"`
}

type importAnswer struct {
	PlansSent             int64 `json:"plans_sent"`
	PlansPresent          int64 `json:"plans_present"`
	SubscriptionsSent     int64 `json:"subscriptions_sent"`
	SubscriptionsPresent  int64 `json:"subscriptions_present"`
	PlansInserted         int64 `json:"plans_inserted"`
	SubscriptionsInserted int64 `json:"subscriptions_inserted"`
}

func main() {
	var (
		dsn      = flag.String("db", os.Getenv("DATABASE_URL"), "feed-engine database URL")
		themis   = flag.String("themis", os.Getenv("THEMIS_URL"), "Themis base URL")
		key      = flag.String("key", os.Getenv("INTERNAL_API_KEY"), "internal service key")
		dryRun   = flag.Bool("dry-run", false, "read and count without sending")
		deadline = flag.Duration("timeout", 10*time.Minute, "how long the whole import may take")
	)
	flag.Parse()
	if *dsn == "" || (!*dryRun && (*themis == "" || *key == "")) {
		flag.Usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *deadline)
	defer cancel()

	database, err := sql.Open("postgres", *dsn)
	if err != nil {
		log.Fatalf("opening the database: %v", err)
	}
	defer database.Close()

	plans, subs, skipped, err := dbpkg.LegacyCommerceRows(ctx, database)
	if err != nil {
		log.Fatalf("reading plans and subscriptions: %v", err)
	}
	log.Printf("read %d plan(s) and %d subscription(s); %d row(s) left out because an account has no identity",
		len(plans), len(subs), skipped)
	if *dryRun {
		return
	}

	founder, err := dbpkg.GetFounder(database)
	if err != nil || founder == nil || founder.PIALID == "" {
		log.Fatalf("the import is made in the founder's name, and no founder identity was found: %v", err)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	var total importAnswer
	send := func(ps []importPlan, ss []importSubscription) {
		body, err := json.Marshal(map[string]interface{}{"plans": ps, "subscriptions": ss})
		if err != nil {
			log.Fatalf("encoding a batch: %v", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, *themis+"/v1/admin/import/subscriptions", bytes.NewReader(body))
		if err != nil {
			log.Fatalf("building a request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Service-Key", *key)
		req.Header.Set("X-Internal-Key", *key)
		req.Header.Set("X-Pial-Identity", founder.PIALID)
		resp, err := client.Do(req)
		if err != nil {
			log.Fatalf("sending a batch to Themis: %v", err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			log.Fatalf("reading Themis's answer: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			log.Fatalf("Themis refused a batch (%d): %s", resp.StatusCode, raw)
		}
		var a importAnswer
		if err := json.Unmarshal(raw, &a); err != nil {
			log.Fatalf("Themis's answer is unreadable: %v", err)
		}
		total.PlansSent += a.PlansSent
		total.PlansPresent += a.PlansPresent
		total.SubscriptionsSent += a.SubscriptionsSent
		total.SubscriptionsPresent += a.SubscriptionsPresent
		total.PlansInserted += a.PlansInserted
		total.SubscriptionsInserted += a.SubscriptionsInserted
	}

	// Plans first: a subscription names its plan, and Themis links the two only
	// when the plan is already there.
	for i := 0; i < len(plans); i += batch {
		end := min(i+batch, len(plans))
		ps := make([]importPlan, 0, end-i)
		for _, p := range plans[i:end] {
			ps = append(ps, importPlan(p))
		}
		send(ps, nil)
	}
	for i := 0; i < len(subs); i += batch {
		end := min(i+batch, len(subs))
		ss := make([]importSubscription, 0, end-i)
		for _, s := range subs[i:end] {
			var planID *string
			// A plan id this brain held that is not a UUID names no plan.
			if _, err := uuid.Parse(s.PlanID); err == nil {
				id := s.PlanID
				planID = &id
			}
			ss = append(ss, importSubscription{
				ID: s.ID, SubscriberAccount: s.SubscriberAccount, SubscriberPIAL: s.SubscriberPIAL,
				CreatorAccount: s.CreatorAccount, CreatorPIAL: s.CreatorPIAL, PlanID: planID,
				Status: s.Status, PeriodEnd: s.PeriodEnd, CancelledAt: s.CancelledAt, CreatedAt: s.CreatedAt,
			})
		}
		send(nil, ss)
	}

	verified := total.PlansSent == int64(len(plans)) && total.PlansPresent == int64(len(plans)) &&
		total.SubscriptionsSent == int64(len(subs)) && total.SubscriptionsPresent == int64(len(subs))
	if err := dbpkg.RecordCommerceImport(ctx, database, total.PlansSent, total.PlansPresent,
		total.SubscriptionsSent, total.SubscriptionsPresent, verified); err != nil {
		log.Printf("recording the run: %v", err)
	}
	fmt.Printf("plans:         %d read, %d sent, %d held by Themis (%d new)\n",
		len(plans), total.PlansSent, total.PlansPresent, total.PlansInserted)
	fmt.Printf("subscriptions: %d read, %d sent, %d held by Themis (%d new)\n",
		len(subs), total.SubscriptionsSent, total.SubscriptionsPresent, total.SubscriptionsInserted)
	if !verified {
		log.Fatal("VERIFICATION FAILED: Themis does not hold every row sent — see the counts above")
	}
	fmt.Println("verified: Themis holds every plan and subscription; commerce.events re-projects them")
}
