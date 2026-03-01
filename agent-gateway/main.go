package main

import (
	"bytes"
	"context"
	"crypto/subtle"
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
	DatabaseURL string
	NatsURL     string
	BackendURL  string
	ControlPlaneAPIKey string
	Port        string
}

func loadConfig() Config {
	return Config{
		DatabaseURL: getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:     getEnv("NATS_URL", "nats://nats:4222"),
		BackendURL:  getEnv("CONTROL_PLANE_URL", "http://backend:8000"),
		ControlPlaneAPIKey: getEnv("CONTROL_PLANE_API_KEY", ""),
		Port:        getEnv("PORT", "8094"),
	}
}

var (
	messagesRouted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_agent_messages_routed_total",
	}, []string{"message_type"})
	agentsSpawned = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "platform_agents_spawned_total",
	})
)

func init() {
	prometheus.MustRegister(messagesRouted, agentsSpawned)
}

type Message struct {
	ID            string                 `json:"id"`
	FromAgentID   string                 `json:"from_agent_id"`
	ToAgentID     string                 `json:"to_agent_id,omitempty"`
	ToGroup       string                 `json:"to_group,omitempty"`
	MessageType   string                 `json:"message_type"`
	Subject       string                 `json:"subject,omitempty"`
	Payload       map[string]interface{} `json:"payload"`
	CorrelationID string                 `json:"correlation_id,omitempty"`
	ReplyTo       string                 `json:"reply_to,omitempty"`
	Priority      int                    `json:"priority"`
	ExpiresAt     *time.Time             `json:"expires_at,omitempty"`
}

type SpawnRequest struct {
	ParentAgentID  string                 `json:"parent_agent_id" binding:"required"`
	Name           string                 `json:"name" binding:"required"`
	Image          string                 `json:"image" binding:"required"`
	Config         map[string]interface{} `json:"config"`
	EnvVars        map[string]string      `json:"env_vars"`
	SpawnReason    string                 `json:"spawn_reason"`
	OrganisationID string                 `json:"organisation_id"`
}

type Gateway struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	http   *http.Client
	logger *slog.Logger
}

func NewGateway(cfg Config, db *sql.DB, nc *natsgo.Conn) *Gateway {
	return &Gateway{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		http:   &http.Client{Timeout: 15 * time.Second},
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// ── NATS subscriptions ────────────────────────────────────────────────────

func (g *Gateway) Subscribe(ctx context.Context) {
	// Agent-to-agent direct messages: agent.msg.<to_agent_id>
	g.nc.Subscribe("agent.msg.*", func(msg *natsgo.Msg) {
		var m Message
		if err := json.Unmarshal(msg.Data, &m); err != nil {
			return
		}
		g.routeMessage(ctx, m)
	})

	// Broadcast to a group: agent.group.<group_name>
	g.nc.Subscribe("agent.group.*", func(msg *natsgo.Msg) {
		var m Message
		if err := json.Unmarshal(msg.Data, &m); err != nil {
			return
		}
		g.broadcastToGroup(ctx, m)
	})

	// Agent spawn requests: agent.spawn
	g.nc.Subscribe("agent.spawn", func(msg *natsgo.Msg) {
		var req SpawnRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			return
		}
		childID, err := g.spawnAgent(ctx, req)
		if err != nil {
			g.logger.Error("spawn failed", "parent", req.ParentAgentID, "error", err)
			return
		}
		// Reply to parent via NATS
		replyPayload, _ := json.Marshal(map[string]interface{}{
			"child_agent_id": childID, "status": "spawned",
		})
		if msg.Reply != "" {
			g.nc.Publish(msg.Reply, replyPayload)
		}
		g.nc.Publish("agent.msg."+req.ParentAgentID, replyPayload)
	})

	g.logger.Info("agent gateway subscriptions active")
}

func (g *Gateway) routeMessage(ctx context.Context, m Message) {
	// Persist to agent_messages table
	payload, _ := json.Marshal(m.Payload)
	var msgID string
	g.db.QueryRowContext(ctx, `
		INSERT INTO agent_messages
		  (from_agent_id, to_agent_id, message_type, subject, payload,
		   correlation_id, priority)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, $5::jsonb, NULLIF($6,'')::uuid, $7)
		RETURNING id::text`,
		m.FromAgentID, m.ToAgentID, m.MessageType, m.Subject,
		string(payload), m.CorrelationID, m.Priority).Scan(&msgID)

	// Forward to target agent's inbox subject
	if m.ToAgentID != "" {
		forwardPayload, _ := json.Marshal(map[string]interface{}{
			"message_id":      msgID,
			"from_agent_id":   m.FromAgentID,
			"message_type":    m.MessageType,
			"subject":         m.Subject,
			"payload":         m.Payload,
			"correlation_id":  m.CorrelationID,
		})
		g.nc.Publish("agent.inbox."+m.ToAgentID, forwardPayload)
	}

	messagesRouted.With(prometheus.Labels{"message_type": m.MessageType}).Inc()
	g.logger.Debug("message routed", "from", m.FromAgentID, "to", m.ToAgentID, "type", m.MessageType)
}

func (g *Gateway) broadcastToGroup(ctx context.Context, m Message) {
	// Find all agents in the group (labelled with group=<name>)
	rows, err := g.db.QueryContext(ctx, `
		SELECT id::text FROM agents
		WHERE labels->>'group'=$1 AND status='running'`, m.ToGroup)
	if err != nil {
		return
	}
	defer rows.Close()
	payload, _ := json.Marshal(m.Payload)
	var count int
	for rows.Next() {
		var agentID string
		if err := rows.Scan(&agentID); err == nil {
			g.nc.Publish("agent.inbox."+agentID, payload)
			count++
		}
	}
	g.logger.Debug("broadcast sent", "group", m.ToGroup, "recipients", count)
}

func (g *Gateway) spawnAgent(ctx context.Context, req SpawnRequest) (string, error) {
	// Create the child agent via backend API
	configJSON, _ := json.Marshal(req.Config)
	envJSON, _ := json.Marshal(req.EnvVars)
	payload, _ := json.Marshal(map[string]interface{}{
		"name":            req.Name,
		"image":           req.Image,
		"config":          json.RawMessage(configJSON),
		"env_vars":        json.RawMessage(envJSON),
		"organisation_id": req.OrganisationID,
		"labels":          map[string]string{"spawned_by": req.ParentAgentID},
	})

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		g.cfg.BackendURL+"/api/v1/agents", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if g.cfg.ControlPlaneAPIKey != "" {
		httpReq.Header.Set("X-Service-API-Key", g.cfg.ControlPlaneAPIKey)
	}

	resp, err := g.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("backend call failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("backend returned %d", resp.StatusCode)
	}

	var result struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	// Record spawn tree
	g.db.ExecContext(ctx, `
		INSERT INTO agent_spawn_tree (parent_agent_id, child_agent_id, spawn_reason)
		VALUES ($1::uuid, $2::uuid, $3)
		ON CONFLICT DO NOTHING`,
		req.ParentAgentID, result.ID, req.SpawnReason)

	// Auto-start the child
	startReq, _ := http.NewRequestWithContext(ctx, "POST",
		g.cfg.BackendURL+"/api/v1/agents/"+result.ID+"/start", nil)
	if g.cfg.ControlPlaneAPIKey != "" {
		startReq.Header.Set("X-Service-API-Key", g.cfg.ControlPlaneAPIKey)
	}
	startResp, err := g.http.Do(startReq)
	if err == nil {
		startResp.Body.Close()
	}

	agentsSpawned.Inc()
	g.logger.Info("agent spawned", "parent", req.ParentAgentID, "child", result.ID, "name", req.Name)
	return result.ID, nil
}

func (g *Gateway) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	requireServiceAuth := func() gin.HandlerFunc {
		return func(c *gin.Context) {
			got := c.GetHeader("X-Service-API-Key")
			if g.cfg.ControlPlaneAPIKey == "" || subtle.ConstantTimeCompare([]byte(got), []byte(g.cfg.ControlPlaneAPIKey)) != 1 {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
				return
			}
			c.Next()
		}
	}

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "agent-gateway"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := g.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))
	protected := r.Group("/")
	protected.Use(requireServiceAuth())

	// Send a message via REST
	protected.POST("/messages", func(c *gin.Context) {
		var m Message
		if err := c.ShouldBindJSON(&m); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if m.Priority == 0 {
			m.Priority = 5
		}
		g.routeMessage(c.Request.Context(), m)
		c.JSON(202, gin.H{"status": "routed"})
	})

	// Get inbox for an agent
	protected.GET("/agents/:agent_id/inbox", func(c *gin.Context) {
		agentID := c.Param("agent_id")
		rows, err := g.db.QueryContext(c.Request.Context(), `
			SELECT id::text, from_agent_id::text, message_type, subject,
			       payload, correlation_id::text, status, created_at
			FROM agent_messages
			WHERE to_agent_id=$1::uuid AND status='pending'
			ORDER BY priority ASC, created_at ASC
			LIMIT 100`, agentID)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var messages []map[string]interface{}
		for rows.Next() {
			var id, from, msgType, subject, status string
			var corrID *string
			var payload []byte
			var ts time.Time
			rows.Scan(&id, &from, &msgType, &subject, &payload, &corrID, &status, &ts)
			var pl interface{}
			json.Unmarshal(payload, &pl)
			messages = append(messages, map[string]interface{}{
				"id": id, "from_agent_id": from, "message_type": msgType,
				"subject": subject, "payload": pl,
				"correlation_id": corrID, "status": status, "created_at": ts,
			})
		}
		if messages == nil {
			messages = []map[string]interface{}{}
		}
		c.JSON(200, messages)
	})

	// Mark message as read
	protected.POST("/messages/:id/read", func(c *gin.Context) {
		g.db.ExecContext(c.Request.Context(), `
			UPDATE agent_messages SET status='read', read_at=NOW() WHERE id=$1::uuid`,
			c.Param("id"))
		c.JSON(200, gin.H{"status": "ok"})
	})

	// Spawn an agent
	protected.POST("/agents/spawn", func(c *gin.Context) {
		var req SpawnRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		childID, err := g.spawnAgent(c.Request.Context(), req)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"child_agent_id": childID, "parent_agent_id": req.ParentAgentID})
	})

	// Get spawn tree for an agent
	protected.GET("/agents/:agent_id/children", func(c *gin.Context) {
		rows, err := g.db.QueryContext(c.Request.Context(), `
			SELECT st.child_agent_id::text, a.name, a.status, st.spawn_reason, st.created_at
			FROM agent_spawn_tree st JOIN agents a ON a.id=st.child_agent_id
			WHERE st.parent_agent_id=$1::uuid`, c.Param("agent_id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var children []map[string]interface{}
		for rows.Next() {
			var id, name, status, reason string
			var ts time.Time
			rows.Scan(&id, &name, &status, &reason, &ts)
			children = append(children, map[string]interface{}{
				"child_agent_id": id, "name": name, "status": status,
				"spawn_reason": reason, "spawned_at": ts,
			})
		}
		if children == nil {
			children = []map[string]interface{}{}
		}
		c.JSON(200, children)
	})

	return r
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting agent-gateway", "port", cfg.Port)

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
	db.SetMaxOpenConns(15)
	defer db.Close()

	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(cfg.NatsURL, natsgo.MaxReconnects(-1),
			natsgo.ReconnectWait(2*time.Second), natsgo.Name("agent-gateway"))
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

	gw := NewGateway(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gw.Subscribe(ctx)

	router := gw.setupRouter()
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
