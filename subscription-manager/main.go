package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
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
	DatabaseURL    string
	NatsURL        string
	BillingURL     string
	Port           string
	RenewalLeadDay int
}

func loadConfig() Config {
	return Config{
		DatabaseURL:    getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:        getEnv("NATS_URL", "nats://nats:4222"),
		BillingURL:     getEnv("BILLING_ENGINE_URL", "http://billing-engine:8101"),
		Port:           getEnv("PORT", "8102"),
		RenewalLeadDay: 3,
	}
}

var (
	subscriptionsCreated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_subscriptions_created_total",
	}, []string{"plan"})
	subscriptionsCancelled = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "platform_subscriptions_cancelled_total",
	})
	subscriptionsUpgraded = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "platform_subscriptions_upgraded_total",
	})
	trialExpirations = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "platform_trial_expirations_total",
	})
)

func init() {
	prometheus.MustRegister(subscriptionsCreated, subscriptionsCancelled, subscriptionsUpgraded, trialExpirations)
}

type SubscriptionManager struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	http   *http.Client
	logger *slog.Logger
}

func New(cfg Config, db *sql.DB, nc *natsgo.Conn) *SubscriptionManager {
	return &SubscriptionManager{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		http:   &http.Client{Timeout: 15 * time.Second},
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// ── Core operations ────────────────────────────────────────────────────────────

func (m *SubscriptionManager) CreateSubscription(ctx context.Context, orgID, planName, billingCycle, pmID, provider string) (string, error) {
	// Get plan
	var planID string
	var basePrice int64
	err := m.db.QueryRowContext(ctx, `
		SELECT id::text, base_price_cents FROM billing_plans WHERE name=$1 AND is_active=TRUE`, planName).
		Scan(&planID, &basePrice)
	if err != nil {
		return "", fmt.Errorf("plan not found: %s", planName)
	}

	// Determine period end based on billing cycle
	periodEnd := "NOW() + INTERVAL '1 month'"
	if billingCycle == "annual" {
		periodEnd = "NOW() + INTERVAL '1 year'"
	}

	// Trial for free/starter plans — 14 days
	trialClause := "NULL"
	if planName == "starter" || planName == "growth" {
		trialClause = "NOW() + INTERVAL '14 days'"
	}

	var subID string
	err = m.db.QueryRowContext(ctx, fmt.Sprintf(`
		INSERT INTO subscriptions
		  (organisation_id, plan_id, status, billing_cycle,
		   current_period_start, current_period_end,
		   trial_ends_at, payment_method_id, payment_provider)
		VALUES ($1::uuid, $2::uuid,
		        CASE WHEN %s != 'NULL' THEN 'trialing' ELSE 'active' END,
		        $3, NOW(), %s, %s, $4, $5)
		ON CONFLICT (organisation_id) DO UPDATE
		  SET plan_id=$2::uuid, status='active', billing_cycle=$3,
		      current_period_start=NOW(), current_period_end=%s,
		      payment_method_id=$4, payment_provider=$5, updated_at=NOW()
		RETURNING id::text`, trialClause, periodEnd, trialClause, periodEnd),
		orgID, planID, billingCycle, pmID, provider).Scan(&subID)
	if err != nil {
		return "", fmt.Errorf("create subscription: %w", err)
	}

	subscriptionsCreated.With(prometheus.Labels{"plan": planName}).Inc()
	m.publishEvent("subscription.created", map[string]interface{}{
		"organisation_id": orgID, "plan": planName, "subscription_id": subID,
	})
	m.logger.Info("subscription created", "org_id", orgID, "plan", planName, "sub_id", subID)
	return subID, nil
}

func (m *SubscriptionManager) UpgradePlan(ctx context.Context, orgID, newPlanName string) error {
	var planID string
	err := m.db.QueryRowContext(ctx, `SELECT id::text FROM billing_plans WHERE name=$1`, newPlanName).Scan(&planID)
	if err != nil {
		return fmt.Errorf("plan not found: %s", newPlanName)
	}

	_, err = m.db.ExecContext(ctx, `
		UPDATE subscriptions SET plan_id=$1::uuid, status='active', updated_at=NOW()
		WHERE organisation_id=$2::uuid`, planID, orgID)
	if err != nil {
		return err
	}

	// Update org quotas to match new plan
	m.updateOrgQuotas(ctx, orgID, newPlanName)

	subscriptionsUpgraded.Inc()
	m.publishEvent("subscription.upgraded", map[string]interface{}{
		"organisation_id": orgID, "new_plan": newPlanName,
	})
	return nil
}

func (m *SubscriptionManager) CancelSubscription(ctx context.Context, orgID, reason string) error {
	_, err := m.db.ExecContext(ctx, `
		UPDATE subscriptions
		SET status='cancelled', cancelled_at=NOW(), cancel_reason=$1, updated_at=NOW()
		WHERE organisation_id=$2::uuid AND status NOT IN ('cancelled','expired')`,
		reason, orgID)
	if err != nil {
		return err
	}
	subscriptionsCancelled.Inc()
	m.publishEvent("subscription.cancelled", map[string]interface{}{
		"organisation_id": orgID, "reason": reason,
	})
	return nil
}

func (m *SubscriptionManager) updateOrgQuotas(ctx context.Context, orgID, planName string) {
	quotaMap := map[string]map[string]interface{}{
		"free":       {"max_agents": 3, "max_executions_day": 100, "max_api_rpm": 60},
		"starter":    {"max_agents": 25, "max_executions_day": 1000, "max_api_rpm": 600},
		"growth":     {"max_agents": 100, "max_executions_day": 10000, "max_api_rpm": 2000},
		"enterprise": {"max_agents": 10000, "max_executions_day": 1000000, "max_api_rpm": 10000},
	}
	quotas, ok := quotaMap[planName]
	if !ok {
		return
	}
	m.db.ExecContext(ctx, `
		INSERT INTO organisation_quotas (organisation_id, max_agents, max_executions_day, max_api_rpm)
		VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (organisation_id) DO UPDATE
		  SET max_agents=$2, max_executions_day=$3, max_api_rpm=$4`,
		orgID, quotas["max_agents"], quotas["max_executions_day"], quotas["max_api_rpm"])
}

// ── Background jobs ────────────────────────────────────────────────────────────

func (m *SubscriptionManager) RunRenewalChecker(ctx context.Context) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	m.checkRenewals(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkRenewals(ctx)
		}
	}
}

func (m *SubscriptionManager) checkRenewals(ctx context.Context) {
	// Find subscriptions expiring within lead days
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.id::text, s.organisation_id::text, bp.name, s.billing_cycle,
		       s.current_period_end, s.payment_method_id, s.payment_provider
		FROM subscriptions s JOIN billing_plans bp ON bp.id=s.plan_id
		WHERE s.status='active'
		  AND s.current_period_end <= NOW() + INTERVAL '1 day' * $1
		  AND s.current_period_end > NOW()`, m.cfg.RenewalLeadDay)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var subID, orgID, planName, cycle, pmID, provider string
		var periodEnd time.Time
		rows.Scan(&subID, &orgID, &planName, &cycle, &periodEnd, &pmID, &provider)
		m.publishEvent("subscription.renewal_due", map[string]interface{}{
			"subscription_id": subID, "organisation_id": orgID,
			"plan": planName, "period_end": periodEnd.Format(time.RFC3339),
		})
	}

	// Expire trials that ended
	expiredRows, err := m.db.QueryContext(ctx, `
		UPDATE subscriptions
		SET status='active', trial_ends_at=NULL, updated_at=NOW()
		WHERE status='trialing' AND trial_ends_at <= NOW()
		RETURNING id::text, organisation_id::text`)
	if err == nil {
		defer expiredRows.Close()
		for expiredRows.Next() {
			var subID, orgID string
			expiredRows.Scan(&subID, &orgID)
			trialExpirations.Inc()
			m.publishEvent("subscription.trial_ended", map[string]interface{}{
				"subscription_id": subID, "organisation_id": orgID,
			})
		}
	}

	// Auto-renew active subscriptions past period end
	m.db.ExecContext(ctx, `
		UPDATE subscriptions SET
		  current_period_start=current_period_end,
		  current_period_end=CASE billing_cycle
		    WHEN 'annual' THEN current_period_end + INTERVAL '1 year'
		    ELSE current_period_end + INTERVAL '1 month'
		  END,
		  updated_at=NOW()
		WHERE status='active' AND current_period_end <= NOW()`)
}

func (m *SubscriptionManager) publishEvent(subject string, data map[string]interface{}) {
	data["timestamp"] = time.Now().UTC().Format(time.RFC3339)
	payload, _ := json.Marshal(data)
	_ = m.nc.Publish("billing.events", payload)
	_ = m.nc.Publish(subject, payload)
}

// ── HTTP API ──────────────────────────────────────────────────────────────────

func (m *SubscriptionManager) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "subscription-manager"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := m.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	r.POST("/subscriptions", func(c *gin.Context) {
		var body struct {
			OrgID        string `json:"organisation_id" binding:"required"`
			PlanName     string `json:"plan_name" binding:"required"`
			BillingCycle string `json:"billing_cycle"`
			PaymentMethodID string `json:"payment_method_id"`
			Provider     string `json:"provider"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if body.BillingCycle == "" {
			body.BillingCycle = "monthly"
		}
		if body.Provider == "" {
			body.Provider = "stripe"
		}
		subID, err := m.CreateSubscription(c.Request.Context(),
			body.OrgID, body.PlanName, body.BillingCycle, body.PaymentMethodID, body.Provider)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"subscription_id": subID, "plan": body.PlanName, "status": "active"})
	})

	r.GET("/subscriptions/:org_id", func(c *gin.Context) {
		row := m.db.QueryRowContext(c.Request.Context(), `
			SELECT s.id::text, bp.name, bp.display_name, s.status,
			       s.billing_cycle, s.current_period_start, s.current_period_end,
			       s.trial_ends_at, s.cancelled_at, s.external_id
			FROM subscriptions s JOIN billing_plans bp ON bp.id=s.plan_id
			WHERE s.organisation_id=$1::uuid`, c.Param("org_id"))
		var id, planName, displayName, status, cycle string
		var start, end time.Time
		var trialEnd, cancelledAt *time.Time
		var extID *string
		if err := row.Scan(&id, &planName, &displayName, &status, &cycle,
			&start, &end, &trialEnd, &cancelledAt, &extID); err != nil {
			c.JSON(200, gin.H{"plan_name": "free", "status": "active"})
			return
		}
		c.JSON(200, gin.H{
			"id": id, "plan_name": planName, "display_name": displayName,
			"status": status, "billing_cycle": cycle,
			"current_period_start": start, "current_period_end": end,
			"trial_ends_at": trialEnd, "cancelled_at": cancelledAt,
			"external_id": extID,
		})
	})

	r.PATCH("/subscriptions/:org_id/upgrade", func(c *gin.Context) {
		var body struct {
			PlanName string `json:"plan_name" binding:"required"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if err := m.UpgradePlan(c.Request.Context(), c.Param("org_id"), body.PlanName); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"status": "upgraded", "plan": body.PlanName})
	})

	r.DELETE("/subscriptions/:org_id", func(c *gin.Context) {
		reason := c.Query("reason")
		if err := m.CancelSubscription(c.Request.Context(), c.Param("org_id"), reason); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"status": "cancelled"})
	})

	// Trigger invoice generation via billing-engine
	r.POST("/subscriptions/:org_id/invoice", func(c *gin.Context) {
		payload, _ := json.Marshal(map[string]interface{}{"organisation_id": c.Param("org_id")})
		req, err := http.NewRequestWithContext(c.Request.Context(), "POST",
			m.cfg.BillingURL+"/invoices/generate", bytes.NewReader(payload))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := m.http.Do(req)
		if err != nil {
			c.JSON(503, gin.H{"error": "billing engine unavailable"})
			return
		}
		defer resp.Body.Close()
		var result map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&result)
		c.JSON(resp.StatusCode, result)
	})

	return r
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting subscription-manager", "port", cfg.Port)

	var db *sql.DB
	var err error
	for i := 0; i < 30; i++ {
		db, err = sql.Open("postgres", cfg.DatabaseURL)
		if err == nil && db.PingContext(context.Background()) == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		logger.Error("postgres unavailable", "error", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(10)
	defer db.Close()

	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(cfg.NatsURL, natsgo.MaxReconnects(-1),
			natsgo.Name("subscription-manager"))
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

	mgr := New(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.RunRenewalChecker(ctx)

	router := mgr.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http error", "error", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}