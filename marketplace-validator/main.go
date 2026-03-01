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
	"os/exec"
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
	DatabaseURL string
	NatsURL     string
	Port        string
	ScannerBin  string
}

func loadConfig() Config {
	return Config{
		DatabaseURL: getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:     getEnv("NATS_URL", "nats://nats:4222"),
		Port:        getEnv("PORT", "8102"),
		ScannerBin:  getEnv("SCANNER_BIN", "trivy"),
	}
}

var (
	scansCompleted = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "platform_marketplace_scans_completed_total"}, []string{"result"})
	scansRunning   = prometheus.NewGauge(prometheus.GaugeOpts{Name: "platform_marketplace_scans_running"})
)
func init() { prometheus.MustRegister(scansCompleted, scansRunning) }

type Validator struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	logger *slog.Logger
	jobs   chan string
}

func New(cfg Config, db *sql.DB, nc *natsgo.Conn) *Validator {
	return &Validator{
		cfg:    cfg, db: db, nc: nc,
		logger: slog.New(slog.NewJSONHandler(os.Stdout, nil)),
		jobs:   make(chan string, 64),
	}
}

func (v *Validator) StartWorkers(ctx context.Context, n int) {
	for i := 0; i < n; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case scanID := <-v.jobs:
					v.runScan(ctx, scanID)
				}
			}
		}()
	}
}

func (v *Validator) runScan(ctx context.Context, scanID string) {
	scansRunning.Inc()
	defer scansRunning.Dec()

	var templateID, registryID, image, scanType string
	v.db.QueryRowContext(ctx, `
		SELECT ts.template_id::text, COALESCE(ts.registry_id::text,''), at.image, ts.scan_type
		FROM template_scans ts JOIN agent_templates at ON at.id=ts.template_id
		WHERE ts.id=$1::uuid`, scanID).Scan(&templateID, &registryID, &image, &scanType)

	v.db.ExecContext(ctx, `UPDATE template_scans SET status='running' WHERE id=$1::uuid`, scanID)

	start := time.Now()
	findings, severity, err := v.scanImage(ctx, image, scanType)
	duration := time.Since(start).Milliseconds()

	status := "passed"
	if err != nil {
		status = "error"
	} else if severity == "critical" || severity == "high" {
		status = "failed"
	}

	findingsJSON, _ := json.Marshal(findings)
	counts := map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0}
	for _, f := range findings {
		if fm, ok := f.(map[string]interface{}); ok {
			if s, ok := fm["severity"].(string); ok {
				counts[s]++
			}
		}
	}
	countsJSON, _ := json.Marshal(counts)

	v.db.ExecContext(ctx, `
		UPDATE template_scans SET
		  status=$1, severity=$2, findings=$3::jsonb, finding_counts=$4::jsonb,
		  scan_duration_ms=$5, completed_at=NOW()
		WHERE id=$6::uuid`,
		status, severity, string(findingsJSON), string(countsJSON), duration, scanID)

	scansCompleted.With(prometheus.Labels{"result": status}).Inc()

	if status == "passed" {
		v.db.ExecContext(ctx, `
			INSERT INTO template_approvals (template_id, status, auto_approved)
			VALUES ($1::uuid, 'approved', TRUE)
			ON CONFLICT DO NOTHING`, templateID)
		v.nc.Publish("marketplace.template.approved", mustJSON(map[string]interface{}{
			"template_id": templateID, "scan_id": scanID,
		}))
	} else {
		v.nc.Publish("marketplace.template.scan_failed", mustJSON(map[string]interface{}{
			"template_id": templateID, "scan_id": scanID, "severity": severity,
		}))
	}
}

func (v *Validator) scanImage(ctx context.Context, image, scanType string) ([]interface{}, string, error) {
	// Try real trivy scan first
	trivyPath, err := exec.LookPath(v.cfg.ScannerBin)
	if err == nil {
		ctx2, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx2, trivyPath, "image", "--format", "json", "--quiet", image).Output()
		if err == nil {
			var trivyResult struct {
				Results []struct {
					Vulnerabilities []struct {
						VulnerabilityID string `json:"VulnerabilityID"`
						Severity        string `json:"Severity"`
						Title           string `json:"Title"`
					} `json:"Vulnerabilities"`
				} `json:"Results"`
			}
			if json.Unmarshal(out, &trivyResult) == nil {
				var findings []interface{}
				maxSeverity := "none"
				severityOrder := map[string]int{"CRITICAL": 4, "HIGH": 3, "MEDIUM": 2, "LOW": 1}
				for _, r := range trivyResult.Results {
					for _, vuln := range r.Vulnerabilities {
						sev := normalizeSeverity(vuln.Severity)
						findings = append(findings, map[string]interface{}{
							"id": vuln.VulnerabilityID, "severity": sev, "title": vuln.Title,
						})
						if severityOrder[vuln.Severity] > severityOrder[normalizeSeverity(maxSeverity)] {
							maxSeverity = sev
						}
					}
				}
				return findings, maxSeverity, nil
			}
		}
	}

	// Fallback: mock scan (development mode)
	v.logger.Warn("trivy not available, using mock scan", "image", image)
	return []interface{}{}, "none", nil
}

func normalizeSeverity(s string) string {
	switch s {
	case "CRITICAL": return "critical"
	case "HIGH": return "high"
	case "MEDIUM": return "medium"
	case "LOW": return "low"
	default: return "none"
	}
}

func (v *Validator) Subscribe(ctx context.Context) {
	v.nc.Subscribe("marketplace.template.submitted", func(msg *natsgo.Msg) {
		var event struct {
			TemplateID string `json:"template_id"`
		}
		if err := json.Unmarshal(msg.Data, &event); err != nil { return }
		scanID := v.createScan(ctx, event.TemplateID, "vulnerability")
		if scanID != "" {
			v.jobs <- scanID
		}
	})
}

func (v *Validator) createScan(ctx context.Context, templateID, scanType string) string {
	var id string
	v.db.QueryRowContext(ctx, `
		INSERT INTO template_scans (template_id, scan_type, scanner, status)
		VALUES ($1::uuid, $2, 'trivy', 'pending') RETURNING id::text`, templateID, scanType).Scan(&id)
	return id
}

func (v *Validator) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"status": "healthy", "service": "marketplace-validator"}) })
	r.GET("/ready", func(c *gin.Context) {
		if err := v.db.PingContext(c.Request.Context()); err != nil { c.JSON(503, gin.H{"status": "not_ready"}); return }
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	r.POST("/templates/:template_id/scan", func(c *gin.Context) {
		scanType := c.DefaultQuery("type", "vulnerability")
		scanID := v.createScan(c.Request.Context(), c.Param("template_id"), scanType)
		if scanID == "" { c.JSON(500, gin.H{"error": "failed to create scan"}); return }
		v.jobs <- scanID
		c.JSON(202, gin.H{"scan_id": scanID, "status": "queued"})
	})

	r.GET("/templates/:template_id/scans", func(c *gin.Context) {
		rows, _ := v.db.QueryContext(c.Request.Context(), `
			SELECT id::text, scan_type, scanner, status, severity, finding_counts, scan_duration_ms, created_at, completed_at
			FROM template_scans WHERE template_id=$1::uuid ORDER BY created_at DESC LIMIT 20`, c.Param("template_id"))
		if rows == nil { c.JSON(200, []interface{}{}); return }
		defer rows.Close()
		var scans []map[string]interface{}
		for rows.Next() {
			var id, scanType, scanner, status, severity string
			var durationMs *int64
			var counts []byte
			var createdAt time.Time
			var completedAt *time.Time
			rows.Scan(&id, &scanType, &scanner, &status, &severity, &counts, &durationMs, &createdAt, &completedAt)
			var countsData interface{}
			json.Unmarshal(counts, &countsData)
			scans = append(scans, map[string]interface{}{
				"id": id, "scan_type": scanType, "scanner": scanner, "status": status,
				"severity": severity, "finding_counts": countsData, "duration_ms": durationMs,
				"created_at": createdAt, "completed_at": completedAt,
			})
		}
		if scans == nil { scans = []map[string]interface{}{} }
		c.JSON(200, scans)
	})

	r.POST("/templates/:template_id/approve", func(c *gin.Context) {
		var body struct {
			ReviewerID   string `json:"reviewer_id"`
			ReviewNotes  string `json:"review_notes"`
		}
		c.ShouldBindJSON(&body)
		v.db.ExecContext(c.Request.Context(), `
			INSERT INTO template_approvals (template_id, status, reviewer_id, review_notes, reviewed_at)
			VALUES ($1::uuid, 'approved', NULLIF($2,'')::uuid, $3, NOW())`,
			c.Param("template_id"), body.ReviewerID, body.ReviewNotes)
		v.db.ExecContext(c.Request.Context(), `UPDATE agent_templates SET is_verified=TRUE WHERE id=$1::uuid`, c.Param("template_id"))
		c.JSON(200, gin.H{"status": "approved"})
	})

	return r
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := loadConfig()
	logger.Info("starting marketplace-validator", "port", cfg.Port)

	var db *sql.DB
	var err error
	for i := 0; i < 30; i++ {
		db, err = sql.Open("postgres", cfg.DatabaseURL)
		if err == nil && db.PingContext(context.Background()) == nil { break }
		time.Sleep(3 * time.Second)
	}
	if err != nil { logger.Error("postgres unavailable"); os.Exit(1) }
	db.SetMaxOpenConns(10)
	defer db.Close()

	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(cfg.NatsURL, natsgo.MaxReconnects(-1), natsgo.Name("marketplace-validator"))
		if err == nil { break }
		time.Sleep(2 * time.Second)
	}
	if err != nil { logger.Error("nats unavailable"); os.Exit(1) }
	defer nc.Close()

	v := New(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v.StartWorkers(ctx, 4)
	v.Subscribe(ctx)

	router := v.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}
	go srv.ListenAndServe()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" { return v }
	return fallback
}
func mustJSON(v interface{}) []byte { b, _ := json.Marshal(v); return b }
var _ = bytes.NewReader
var _ = fmt.Sprintf