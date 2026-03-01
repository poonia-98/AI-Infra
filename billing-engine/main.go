package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	natsgo "github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Config struct {
	DatabaseURL     string
	NatsURL         string
	StripeSecretKey string
	Port            string
	MeterInterval   time.Duration
	InvoiceDay      int // day of month to generate invoices
}

func loadConfig() Config {
	interval, _ := time.ParseDuration(getEnv("METER_INTERVAL", "60s"))
	return Config{
		DatabaseURL:     getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:         getEnv("NATS_URL", "nats://nats:4222"),
		StripeSecretKey: getEnv("STRIPE_SECRET_KEY", ""),
		Port:            getEnv("PORT", "8096"),
		MeterInterval:   interval,
		InvoiceDay:      1,
	}
}

var (
	usageRecorded = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_billing_usage_recorded_total",
	}, []string{"resource_type"})
	invoicesGenerated = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "platform_billing_invoices_generated_total",
	})
	mrrGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "platform_billing_mrr_cents",
		Help: "Monthly Recurring Revenue in cents",
	})
)

func init() {
	prometheus.MustRegister(usageRecorded, invoicesGenerated, mrrGauge)
}

// ── Domain ─────────────────────────────────────────────────────────────────

type UsageEvent struct {
	OrganisationID string  `json:"organisation_id"`
	ResourceType   string  `json:"resource_type"`
	Quantity       float64 `json:"quantity"`
	Unit           string  `json:"unit"`
	AgentID        string  `json:"agent_id,omitempty"`
	ExecutionID    string  `json:"execution_id,omitempty"`
	WorkflowExecID string  `json:"workflow_exec_id,omitempty"`
	IdempotencyKey string  `json:"idempotency_key,omitempty"`
	Metadata       map[string]interface{} `json:"metadata,omitempty"`
}

type Plan struct {
	ID                         string
	Slug                       string
	BasePriceCents             int64
	PricePerAgentHourMC        int64
	PricePerExecutionMC        int64
	PricePerCPUSecondMC        int64
	PricePerMemoryGBHourMC     int64
	PricePerAPICallMC          int64
	PricePerWorkflowExecMC     int64
	IncludedAgents             int
	IncludedExecutions         int
	IncludedCPUHours           float64
}

type LineItem struct {
	Description string  `json:"description"`
	Quantity    float64 `json:"quantity"`
	Unit        string  `json:"unit"`
	UnitPriceMC int64   `json:"unit_price_mc"`
	AmountMC    int64   `json:"amount_mc"`
	AmountCents int64   `json:"amount_cents"`
}

// ── Billing Engine ─────────────────────────────────────────────────────────

type BillingEngine struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	http   *http.Client
	logger *slog.Logger
}

func NewBillingEngine(cfg Config, db *sql.DB, nc *natsgo.Conn) *BillingEngine {
	return &BillingEngine{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		http:   &http.Client{Timeout: 30 * time.Second},
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// ── Usage Recording ────────────────────────────────────────────────────────

func (b *BillingEngine) RecordUsage(ctx context.Context, evt UsageEvent) error {
	// Look up subscription and plan pricing
	plan, subID, err := b.getOrgPlan(ctx, evt.OrganisationID)
	if err != nil {
		return fmt.Errorf("get org plan: %w", err)
	}

	amountMC := b.calculateCost(plan, evt.ResourceType, evt.Quantity)
	metaJSON, _ := json.Marshal(evt.Metadata)

	_, err = b.db.ExecContext(ctx, `
		INSERT INTO usage_records
		  (organisation_id, subscription_id, resource_type, quantity, unit,
		   amount_mc, agent_id, execution_id, workflow_exec_id,
		   idempotency_key, metadata, billing_period)
		VALUES
		  ($1::uuid, $2::uuid, $3, $4, $5, $6,
		   NULLIF($7,'')::uuid, NULLIF($8,'')::uuid, NULLIF($9,'')::uuid,
		   NULLIF($10,''), $11::jsonb, CURRENT_DATE)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		evt.OrganisationID, subID, evt.ResourceType, evt.Quantity, evt.Unit,
		amountMC, evt.AgentID, evt.ExecutionID, evt.WorkflowExecID,
		evt.IdempotencyKey, string(metaJSON))

	if err != nil {
		return fmt.Errorf("insert usage record: %w", err)
	}

	usageRecorded.With(prometheus.Labels{"resource_type": evt.ResourceType}).Inc()
	return nil
}

func (b *BillingEngine) calculateCost(plan *Plan, resourceType string, quantity float64) int64 {
	if plan == nil {
		return 0
	}
	var unitPriceMC int64
	switch resourceType {
	case "agent_hour":
		unitPriceMC = plan.PricePerAgentHourMC
	case "execution":
		unitPriceMC = plan.PricePerExecutionMC
	case "cpu_second":
		unitPriceMC = plan.PricePerCPUSecondMC
	case "memory_gb_hour":
		unitPriceMC = plan.PricePerMemoryGBHourMC
	case "api_call":
		unitPriceMC = plan.PricePerAPICallMC
	case "workflow_execution":
		unitPriceMC = plan.PricePerWorkflowExecMC
	}
	return int64(math.Round(quantity * float64(unitPriceMC)))
}

// ── Usage Aggregation ──────────────────────────────────────────────────────

func (b *BillingEngine) AggregatePeriodUsage(ctx context.Context, orgID string, periodStart, periodEnd time.Time) (map[string]float64, int64, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT resource_type, SUM(quantity), SUM(amount_mc)
		FROM usage_records
		WHERE organisation_id=$1::uuid
		  AND recorded_at >= $2 AND recorded_at < $3
		GROUP BY resource_type`,
		orgID, periodStart, periodEnd)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	usage := map[string]float64{}
	var totalMC int64
	for rows.Next() {
		var resType string
		var qty float64
		var amtMC int64
		rows.Scan(&resType, &qty, &amtMC)
		usage[resType] = qty
		totalMC += amtMC
	}
	return usage, totalMC, nil
}

// ── Invoice Generation ─────────────────────────────────────────────────────

func (b *BillingEngine) GenerateInvoice(ctx context.Context, orgID string, periodStart, periodEnd time.Time) (string, error) {
	plan, subID, err := b.getOrgPlan(ctx, orgID)
	if err != nil {
		return "", err
	}

	usage, totalUsageMC, err := b.AggregatePeriodUsage(ctx, orgID, periodStart, periodEnd)
	if err != nil {
		return "", err
	}

	var lineItems []LineItem
	var subtotalCents int64

	// Base plan fee
	if plan != nil && plan.BasePriceCents > 0 {
		lineItems = append(lineItems, LineItem{
			Description: "Base plan fee",
			Quantity:    1,
			Unit:        "month",
			UnitPriceMC: plan.BasePriceCents * 1000000,
			AmountCents: plan.BasePriceCents,
		})
		subtotalCents += plan.BasePriceCents
	}

	// Usage line items
	usageLabels := map[string]string{
		"agent_hour":         "Agent compute hours",
		"execution":          "Agent executions",
		"cpu_second":         "CPU seconds",
		"memory_gb_hour":     "Memory (GB·hours)",
		"api_call":           "API calls",
		"workflow_execution": "Workflow executions",
	}
	for resType, qty := range usage {
		if qty == 0 {
			continue
		}
		label, ok := usageLabels[resType]
		if !ok {
			label = resType
		}
		amountMC := b.calculateCost(plan, resType, qty)
		amountCents := amountMC / 1000000
		if amountCents == 0 && amountMC > 0 {
			amountCents = 1
		}
		lineItems = append(lineItems, LineItem{
			Description: label,
			Quantity:    qty,
			Unit:        resType,
			AmountMC:    amountMC,
			AmountCents: amountCents,
		})
		subtotalCents += amountCents
	}

	lineItemsJSON, _ := json.Marshal(lineItems)

	invoiceNumber := fmt.Sprintf("INV-%s-%s",
		periodStart.Format("200601"),
		orgID[:8])

	var invoiceID string
	err = b.db.QueryRowContext(ctx, `
		INSERT INTO invoices
		  (organisation_id, subscription_id, invoice_number, status,
		   billing_period_start, billing_period_end,
		   subtotal_cents, total_cents, line_items)
		VALUES
		  ($1::uuid, $2::uuid, $3, 'open', $4, $5, $6, $6, $7::jsonb)
		ON CONFLICT (invoice_number) DO UPDATE SET
		  subtotal_cents=EXCLUDED.subtotal_cents,
		  total_cents=EXCLUDED.total_cents,
		  line_items=EXCLUDED.line_items,
		  updated_at=NOW()
		RETURNING id::text`,
		orgID, subID, invoiceNumber, periodStart, periodEnd,
		subtotalCents, string(lineItemsJSON)).Scan(&invoiceID)
	if err != nil {
		return "", fmt.Errorf("insert invoice: %w", err)
	}

	invoicesGenerated.Inc()
	b.logger.Info("invoice generated", "org_id", orgID, "invoice_id", invoiceID,
		"total_cents", subtotalCents, "usage_mc", totalUsageMC)

	b.publishEvent("billing.invoice.generated", map[string]interface{}{
		"organisation_id": orgID,
		"invoice_id":      invoiceID,
		"total_cents":     subtotalCents,
		"period_start":    periodStart,
		"period_end":      periodEnd,
	})

	return invoiceID, nil
}

// ── Subscription Management ────────────────────────────────────────────────

func (b *BillingEngine) UpdateMRR(ctx context.Context) {
	var mrr int64
	b.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(bp.base_price_cents), 0)
		FROM subscriptions s
		JOIN billing_plans bp ON bp.id=s.plan_id
		WHERE s.status='active'`).Scan(&mrr)
	mrrGauge.Set(float64(mrr))
}

func (b *BillingEngine) getOrgPlan(ctx context.Context, orgID string) (*Plan, string, error) {
	row := b.db.QueryRowContext(ctx, `
		SELECT s.id::text, bp.id::text, bp.slug,
		       bp.base_price_cents, bp.price_per_agent_hour_mc,
		       bp.price_per_execution_mc, bp.price_per_cpu_second_mc,
		       bp.price_per_memory_gb_hour_mc, bp.price_per_api_call_mc,
		       bp.price_per_workflow_exec_mc, bp.included_agents,
		       bp.included_executions, bp.included_cpu_hours
		FROM subscriptions s
		JOIN billing_plans bp ON bp.id=s.plan_id
		WHERE s.organisation_id=$1::uuid AND s.status IN ('active','trialing')
		LIMIT 1`, orgID)

	var subID, planID, slug string
	var plan Plan
	err := row.Scan(&subID, &planID, &slug,
		&plan.BasePriceCents, &plan.PricePerAgentHourMC,
		&plan.PricePerExecutionMC, &plan.PricePerCPUSecondMC,
		&plan.PricePerMemoryGBHourMC, &plan.PricePerAPICallMC,
		&plan.PricePerWorkflowExecMC, &plan.IncludedAgents,
		&plan.IncludedExecutions, &plan.IncludedCPUHours)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, "", nil
		}
		return nil, "", err
	}
	plan.ID = planID
	plan.Slug = slug
	return &plan, subID, nil
}

// ── NATS subscription for usage events ────────────────────────────────────

func (b *BillingEngine) SubscribeToUsageEvents(ctx context.Context) {
	// Agent execution completed → record execution + CPU + memory usage
	b.nc.Subscribe("platform.billing.usage", func(msg *natsgo.Msg) {
		var evt UsageEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			return
		}
		if err := b.RecordUsage(ctx, evt); err != nil {
			b.logger.Error("failed to record usage", "error", err, "org", evt.OrganisationID)
		}
	})

	// Agent execution completed → auto-meter
	b.nc.Subscribe("agents.events", func(msg *natsgo.Msg) {
		var event struct {
			EventType    string  `json:"event_type"`
			AgentID      string  `json:"agent_id"`
			OrgID        string  `json:"organisation_id"`
			DurationMs   int64   `json:"duration_ms"`
			CPUSeconds   float64 `json:"cpu_seconds"`
			MemoryGBHour float64 `json:"memory_gb_hour"`
		}
		if err := json.Unmarshal(msg.Data, &event); err != nil {
			return
		}
		if event.EventType != "execution.completed" && event.EventType != "agent.completed" {
			return
		}
		if event.OrgID == "" {
			return
		}

		if event.DurationMs > 0 {
			b.RecordUsage(ctx, UsageEvent{
				OrganisationID: event.OrgID,
				ResourceType:   "execution",
				Quantity:       1,
				Unit:           "count",
				AgentID:        event.AgentID,
				IdempotencyKey: fmt.Sprintf("exec-%s-%s", event.AgentID, time.Now().Format("20060102150405")),
			})
		}
		if event.CPUSeconds > 0 {
			b.RecordUsage(ctx, UsageEvent{
				OrganisationID: event.OrgID,
				ResourceType:   "cpu_second",
				Quantity:       event.CPUSeconds,
				Unit:           "second",
				AgentID:        event.AgentID,
			})
		}
		if event.MemoryGBHour > 0 {
			b.RecordUsage(ctx, UsageEvent{
				OrganisationID: event.OrgID,
				ResourceType:   "memory_gb_hour",
				Quantity:       event.MemoryGBHour,
				Unit:           "gb_hour",
				AgentID:        event.AgentID,
			})
		}
	})

	b.logger.Info("billing engine subscribed to NATS events")
}

// ── Periodic tasks ─────────────────────────────────────────────────────────

func (b *BillingEngine) StartPeriodicTasks(ctx context.Context) {
	// Update MRR every 5 minutes
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		b.UpdateMRR(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.UpdateMRR(ctx)
			}
		}
	}()

	// Check if monthly invoices need to be generated (run hourly)
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now().UTC()
				if now.Day() == b.cfg.InvoiceDay && now.Hour() == 2 {
					b.generateMonthlyInvoices(ctx)
				}
			}
		}
	}()
}

func (b *BillingEngine) generateMonthlyInvoices(ctx context.Context) {
	now := time.Now().UTC()
	periodEnd := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	periodStart := periodEnd.AddDate(0, -1, 0)

	rows, err := b.db.QueryContext(ctx, `
		SELECT organisation_id::text FROM subscriptions
		WHERE status IN ('active','trialing')`)
	if err != nil {
		b.logger.Error("failed to list subscriptions", "error", err)
		return
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		var orgID string
		rows.Scan(&orgID)
		if _, err := b.GenerateInvoice(ctx, orgID, periodStart, periodEnd); err != nil {
			b.logger.Error("invoice generation failed", "org_id", orgID, "error", err)
		}
		count++
	}
	b.logger.Info("monthly invoices generated", "count", count)
}

func (b *BillingEngine) publishEvent(subject string, data map[string]interface{}) {
	if b.nc == nil {
		return
	}
	data["timestamp"] = time.Now().UTC().Format(time.RFC3339)
	payload, _ := json.Marshal(data)
	_ = b.nc.Publish(subject, payload)
}

// ── HTTP API ──────────────────────────────────────────────────────────────

func (b *BillingEngine) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "billing-engine"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := b.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Record usage manually (for services that can't publish to NATS)
	r.POST("/usage", func(c *gin.Context) {
		var evt UsageEvent
		if err := c.ShouldBindJSON(&evt); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if err := b.RecordUsage(c.Request.Context(), evt); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"status": "recorded"})
	})

	// Get usage summary for an org
	r.GET("/orgs/:org_id/usage", func(c *gin.Context) {
		since := time.Now().UTC().AddDate(0, 0, -30)
		if s := c.Query("since"); s != "" {
			since, _ = time.Parse(time.RFC3339, s)
		}
		usage, totalMC, err := b.AggregatePeriodUsage(
			c.Request.Context(), c.Param("org_id"), since, time.Now().UTC())
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{
			"organisation_id": c.Param("org_id"),
			"since":           since,
			"usage":           usage,
			"total_mc":        totalMC,
			"total_cents":     totalMC / 1000000,
		})
	})

	// List invoices for an org
	r.GET("/orgs/:org_id/invoices", func(c *gin.Context) {
		rows, err := b.db.QueryContext(c.Request.Context(), `
			SELECT id::text, invoice_number, status, billing_period_start,
			       billing_period_end, total_cents, currency, paid_at, created_at
			FROM invoices WHERE organisation_id=$1::uuid
			ORDER BY billing_period_start DESC LIMIT 24`, c.Param("org_id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var invoices []map[string]interface{}
		for rows.Next() {
			var id, num, status, currency string
			var totalCents int64
			var start, end, created time.Time
			var paidAt *time.Time
			rows.Scan(&id, &num, &status, &start, &end, &totalCents, &currency, &paidAt, &created)
			invoices = append(invoices, map[string]interface{}{
				"id": id, "invoice_number": num, "status": status,
				"billing_period_start": start, "billing_period_end": end,
				"total_cents": totalCents, "currency": currency,
				"paid_at": paidAt, "created_at": created,
			})
		}
		if invoices == nil {
			invoices = []map[string]interface{}{}
		}
		c.JSON(200, invoices)
	})

	// Generate invoice on demand
	r.POST("/orgs/:org_id/invoices/generate", func(c *gin.Context) {
		var body struct {
			PeriodStart string `json:"period_start"`
			PeriodEnd   string `json:"period_end"`
		}
		c.ShouldBindJSON(&body)
		start := time.Now().UTC().AddDate(0, -1, 0)
		end := time.Now().UTC()
		if body.PeriodStart != "" {
			start, _ = time.Parse(time.RFC3339, body.PeriodStart)
		}
		if body.PeriodEnd != "" {
			end, _ = time.Parse(time.RFC3339, body.PeriodEnd)
		}
		invoiceID, err := b.GenerateInvoice(c.Request.Context(), c.Param("org_id"), start, end)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"invoice_id": invoiceID})
	})

	// Get subscription for org
	r.GET("/orgs/:org_id/subscription", func(c *gin.Context) {
		row := b.db.QueryRowContext(c.Request.Context(), `
			SELECT s.id::text, s.status, s.trial_ends_at,
			       s.current_period_start, s.current_period_end,
			       bp.name, bp.slug, bp.base_price_cents, bp.features
			FROM subscriptions s JOIN billing_plans bp ON bp.id=s.plan_id
			WHERE s.organisation_id=$1::uuid`, c.Param("org_id"))
		var id, status, planName, planSlug string
		var baseCents int64
		var trialEnd, periodStart, periodEnd *time.Time
		var features []byte
		if err := row.Scan(&id, &status, &trialEnd, &periodStart, &periodEnd,
			&planName, &planSlug, &baseCents, &features); err != nil {
			c.JSON(404, gin.H{"error": "no subscription found"})
			return
		}
		var featList []string
		json.Unmarshal(features, &featList)
		c.JSON(200, gin.H{
			"id": id, "status": status, "trial_ends_at": trialEnd,
			"current_period_start": periodStart, "current_period_end": periodEnd,
			"plan": gin.H{
				"name": planName, "slug": planSlug,
				"base_price_cents": baseCents, "features": featList,
			},
		})
	})

	// List all plans
	r.GET("/plans", func(c *gin.Context) {
		rows, err := b.db.QueryContext(c.Request.Context(), `
			SELECT id::text, name, slug, description, base_price_cents,
			       included_agents, included_executions, max_agents, features
			FROM billing_plans WHERE is_active=TRUE AND is_public=TRUE
			ORDER BY sort_order`)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var plans []map[string]interface{}
		for rows.Next() {
			var id, name, slug, desc string
			var baseCents int64
			var inclAgents, inclExecs, maxAgents int
			var features []byte
			rows.Scan(&id, &name, &slug, &desc, &baseCents, &inclAgents, &inclExecs, &maxAgents, &features)
			var featList []string
			json.Unmarshal(features, &featList)
			plans = append(plans, map[string]interface{}{
				"id": id, "name": name, "slug": slug, "description": desc,
				"base_price_cents": baseCents, "included_agents": inclAgents,
				"included_executions": inclExecs, "max_agents": maxAgents,
				"features": featList,
			})
		}
		c.JSON(200, plans)
	})

	// Create subscription
	r.POST("/orgs/:org_id/subscribe", func(c *gin.Context) {
		var body struct {
			PlanSlug string `json:"plan_slug" binding:"required"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		var subID string
		err := b.db.QueryRowContext(c.Request.Context(), `
			INSERT INTO subscriptions (organisation_id, plan_id, status, trial_ends_at)
			SELECT $1::uuid, bp.id, 'trialing', NOW() + INTERVAL '14 days'
			FROM billing_plans bp WHERE bp.slug=$2
			ON CONFLICT (organisation_id) DO UPDATE
			  SET plan_id=EXCLUDED.plan_id, status='active', updated_at=NOW()
			RETURNING id::text`,
			c.Param("org_id"), body.PlanSlug).Scan(&subID)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"subscription_id": subID})
	})

	return r
}

// ── Main ──────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting billing-engine", "port", cfg.Port)

	var db *sql.DB
	var err error
	for i := 0; i < 30; i++ {
		db, err = sql.Open("postgres", cfg.DatabaseURL)
		if err == nil && db.PingContext(context.Background()) == nil {
			break
		}
		logger.Info("waiting for postgres", "attempt", i+1)
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		logger.Error("postgres unavailable", "error", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	defer db.Close()

	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(cfg.NatsURL, natsgo.MaxReconnects(-1),
			natsgo.ReconnectWait(2*time.Second), natsgo.Name("billing-engine"))
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		logger.Error("nats unavailable", "error", err)
		os.Exit(1)
	}
	defer nc.Close()

	engine := NewBillingEngine(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine.SubscribeToUsageEvents(ctx)
	engine.StartPeriodicTasks(ctx)

	router := engine.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}
	go func() {
		logger.Info("billing-engine listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http error", "error", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	logger.Info("shutting down billing-engine")
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var _ = bytes.NewReader // suppress import warning