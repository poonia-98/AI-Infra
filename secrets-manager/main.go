package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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
	MasterKey      string // 32-byte hex master encryption key
	Port           string
	RotationWorkers int
}

func loadConfig() Config {
	return Config{
		DatabaseURL:    getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:        getEnv("NATS_URL", "nats://nats:4222"),
		MasterKey:      getEnv("MASTER_ENCRYPTION_KEY", "change-this-32-byte-master-key!!"),
		Port:           getEnv("PORT", "8097"),
		RotationWorkers: 4,
	}
}

var (
	secretsRead    = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "platform_secrets_reads_total"}, []string{"org_id"})
	secretsWritten = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_secrets_writes_total"})
	secretsRotated = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_secrets_rotated_total"})
)

func init() {
	prometheus.MustRegister(secretsRead, secretsWritten, secretsRotated)
}

// ── Crypto ────────────────────────────────────────────────────────────────

type Vault struct {
	masterKey [32]byte
}

func NewVault(masterKey string) *Vault {
	key := sha256.Sum256([]byte(masterKey))
	return &Vault{masterKey: key}
}

func (v *Vault) Encrypt(plaintext string) ([]byte, error) {
	block, err := aes.NewCipher(v.masterKey[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return ciphertext, nil
}

func (v *Vault) Decrypt(ciphertext []byte) (string, error) {
	block, err := aes.NewCipher(v.masterKey[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ct := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// ── Secrets Manager ────────────────────────────────────────────────────────

type SecretsManager struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	vault  *Vault
	logger *slog.Logger
}

func NewSecretsManager(cfg Config, db *sql.DB, nc *natsgo.Conn) *SecretsManager {
	return &SecretsManager{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		vault:  NewVault(cfg.MasterKey),
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

func (sm *SecretsManager) WriteSecret(ctx context.Context, orgID, path, secretType, description, value string,
	allowedAgents []string, rotationDays int, createdBy string) (string, error) {

	encrypted, err := sm.vault.Encrypt(value)
	if err != nil {
		return "", fmt.Errorf("encrypt: %w", err)
	}

	agentsJSON, _ := json.Marshal(allowedAgents)

	var secretID string
	err = sm.db.QueryRowContext(ctx, `
		INSERT INTO vault_secrets
		  (organisation_id, path, name, description, secret_type, encrypted_value,
		   encryption_key_id, allowed_agents, rotation_enabled, rotation_interval_days,
		   next_rotation_at, created_by)
		VALUES
		  ($1::uuid, $2, $3, $4, $5, $6, 'default', $7::text[], $8, $9,
		   CASE WHEN $8 THEN NOW() + ($9 || ' days')::interval ELSE NULL END, $10)
		ON CONFLICT (organisation_id, path) DO UPDATE SET
		  encrypted_value=EXCLUDED.encrypted_value,
		  description=EXCLUDED.description,
		  secret_type=EXCLUDED.secret_type,
		  allowed_agents=EXCLUDED.allowed_agents,
		  rotation_enabled=EXCLUDED.rotation_enabled,
		  rotation_interval_days=EXCLUDED.rotation_interval_days,
		  updated_at=NOW()
		RETURNING id::text`,
		orgID, path, lastSegment(path), description, secretType, encrypted,
		string(agentsJSON), rotationDays > 0, rotationDays, createdBy).Scan(&secretID)
	if err != nil {
		return "", err
	}

	// Archive current version
	var version int
	sm.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0) + 1 FROM secret_versions WHERE secret_id=$1::uuid`,
		secretID).Scan(&version)
	sm.db.ExecContext(ctx, `
		UPDATE secret_versions SET is_current=FALSE WHERE secret_id=$1::uuid`,
		secretID)
	sm.db.ExecContext(ctx, `
		INSERT INTO secret_versions (secret_id, version, encrypted_value, encryption_key_id, is_current, created_by)
		VALUES ($1::uuid, $2, $3, 'default', TRUE, $4)`,
		secretID, version, encrypted, createdBy)

	sm.logAccess(ctx, secretID, orgID, "service", "secrets-manager", "write", "", true, "")
	secretsWritten.Inc()
	return secretID, nil
}

func (sm *SecretsManager) ReadSecret(ctx context.Context, orgID, path, accessorType, accessorID, agentID string) (string, error) {
	var secretID string
	var encryptedValue []byte
	var allowedAgents []byte

	err := sm.db.QueryRowContext(ctx, `
		SELECT id::text, encrypted_value, allowed_agents
		FROM vault_secrets
		WHERE organisation_id=$1::uuid AND path=$2 AND is_active=TRUE
		  AND (expires_at IS NULL OR expires_at > NOW())`,
		orgID, path).Scan(&secretID, &encryptedValue, &allowedAgents)
	if err != nil {
		sm.logAccess(ctx, "", orgID, accessorType, accessorID, "read", agentID, false, "not found")
		return "", fmt.Errorf("secret not found: %s", path)
	}

	// Check access control for agents
	if accessorType == "agent" && agentID != "" {
		var agents []string
		json.Unmarshal(allowedAgents, &agents)
		if !contains(agents, "*") && !contains(agents, agentID) {
			sm.logAccess(ctx, secretID, orgID, accessorType, accessorID, "read", agentID, false, "access denied")
			return "", fmt.Errorf("agent %s not allowed to access secret", agentID)
		}
	}

	value, err := sm.vault.Decrypt(encryptedValue)
	if err != nil {
		sm.logAccess(ctx, secretID, orgID, accessorType, accessorID, "read", agentID, false, "decrypt failed")
		return "", fmt.Errorf("decrypt failed")
	}

	sm.logAccess(ctx, secretID, orgID, accessorType, accessorID, "read", agentID, true, "")
	secretsRead.With(prometheus.Labels{"org_id": orgID}).Inc()
	return value, nil
}

func (sm *SecretsManager) RotateSecret(ctx context.Context, secretID string, newValue string, rotatedBy string) error {
	encrypted, err := sm.vault.Encrypt(newValue)
	if err != nil {
		return err
	}

	var orgID string
	sm.db.QueryRowContext(ctx, `SELECT organisation_id::text FROM vault_secrets WHERE id=$1::uuid`, secretID).Scan(&orgID)

	// Archive old version
	var version int
	sm.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0) + 1 FROM secret_versions WHERE secret_id=$1::uuid`, secretID).Scan(&version)
	sm.db.ExecContext(ctx, `UPDATE secret_versions SET is_current=FALSE WHERE secret_id=$1::uuid`, secretID)
	sm.db.ExecContext(ctx, `
		INSERT INTO secret_versions (secret_id, version, encrypted_value, encryption_key_id, is_current, created_by)
		VALUES ($1::uuid, $2, $3, 'default', TRUE, $4)`, secretID, version, encrypted, rotatedBy)

	_, err = sm.db.ExecContext(ctx, `
		UPDATE vault_secrets SET
		  encrypted_value=$1, last_rotated_at=NOW(),
		  next_rotation_at=CASE WHEN rotation_interval_days IS NOT NULL
		    THEN NOW() + (rotation_interval_days || ' days')::interval ELSE NULL END,
		  updated_at=NOW()
		WHERE id=$2::uuid`, encrypted, secretID)

	if err == nil {
		sm.logAccess(ctx, secretID, orgID, "service", rotatedBy, "rotate", "", true, "")
		secretsRotated.Inc()

		// Notify agents to refresh their injected secrets
		sm.nc.Publish("secrets.rotated", mustJSON(map[string]interface{}{
			"secret_id":  secretID,
			"rotated_at": time.Now().UTC(),
		}))
	}
	return err
}

func (sm *SecretsManager) BulkInjectForAgent(ctx context.Context, orgID, agentID string) (map[string]string, error) {
	rows, err := sm.db.QueryContext(ctx, `
		SELECT path, encrypted_value FROM vault_secrets
		WHERE organisation_id=$1::uuid AND is_active=TRUE
		  AND (expires_at IS NULL OR expires_at > NOW())
		  AND ($2 = ANY(allowed_agents) OR '*' = ANY(allowed_agents))`,
		orgID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	secrets := map[string]string{}
	for rows.Next() {
		var path string
		var enc []byte
		rows.Scan(&path, &enc)
		if val, err := sm.vault.Decrypt(enc); err == nil {
			// Convert path to env var name: prod/db/password -> DB_PASSWORD
			envKey := pathToEnvVar(path)
			secrets[envKey] = val
		}
	}
	return secrets, nil
}

func (sm *SecretsManager) logAccess(ctx context.Context, secretID, orgID, accessorType, accessorID, action, agentID string, success bool, errMsg string) {
	if secretID == "" {
		return
	}
	sm.db.ExecContext(ctx, `
		INSERT INTO secret_access_logs
		  (secret_id, organisation_id, accessor_type, accessor_id, action, agent_id, success, error_message)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, NULLIF($6,'')::uuid, $7, $8)`,
		secretID, orgID, accessorType, accessorID, action, agentID, success, errMsg)
}

func (sm *SecretsManager) RunRotationWorker(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sm.processRotations(ctx)
		}
	}
}

func (sm *SecretsManager) processRotations(ctx context.Context) {
	rows, err := sm.db.QueryContext(ctx, `
		SELECT id::text, organisation_id::text FROM vault_secrets
		WHERE rotation_enabled=TRUE AND next_rotation_at < NOW() AND is_active=TRUE
		LIMIT 100`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, orgID string
		rows.Scan(&id, &orgID)
		sm.logger.Info("auto-rotating secret", "secret_id", id, "org_id", orgID)
		// Signal the owning service to provide a new value
		sm.nc.Publish("secrets.rotation_needed", mustJSON(map[string]interface{}{
			"secret_id":       id,
			"organisation_id": orgID,
		}))
	}
}

// ── HTTP API ───────────────────────────────────────────────────────────────

func (sm *SecretsManager) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "secrets-manager"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := sm.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Write/update a secret
	r.PUT("/orgs/:org_id/secrets/:path", func(c *gin.Context) {
		path := c.Param("path")
		var body struct {
			Value         string   `json:"value" binding:"required"`
			SecretType    string   `json:"secret_type"`
			Description   string   `json:"description"`
			AllowedAgents []string `json:"allowed_agents"`
			RotationDays  int      `json:"rotation_days"`
			CreatedBy     string   `json:"created_by"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if body.SecretType == "" {
			body.SecretType = "generic"
		}
		if body.AllowedAgents == nil {
			body.AllowedAgents = []string{"*"}
		}
		id, err := sm.WriteSecret(c.Request.Context(), c.Param("org_id"), path,
			body.SecretType, body.Description, body.Value,
			body.AllowedAgents, body.RotationDays, body.CreatedBy)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"id": id, "path": path})
	})

	// Read a secret value (returns only to authorized callers)
	r.GET("/orgs/:org_id/secrets/:path", func(c *gin.Context) {
		accessorType := c.GetHeader("X-Accessor-Type")
		accessorID := c.GetHeader("X-Accessor-ID")
		agentID := c.GetHeader("X-Agent-ID")
		if accessorType == "" {
			accessorType = "service"
		}
		if accessorID == "" {
			accessorID = "api"
		}
		value, err := sm.ReadSecret(c.Request.Context(), c.Param("org_id"),
			c.Param("path"), accessorType, accessorID, agentID)
		if err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		// Encode value in base64 to avoid JSON escaping issues
		c.JSON(200, gin.H{
			"path":  c.Param("path"),
			"value": base64.StdEncoding.EncodeToString([]byte(value)),
		})
	})

	// List secret paths (never values)
	r.GET("/orgs/:org_id/secrets", func(c *gin.Context) {
		rows, err := sm.db.QueryContext(c.Request.Context(), `
			SELECT id::text, path, name, secret_type, description,
			       rotation_enabled, last_rotated_at, expires_at, created_at
			FROM vault_secrets WHERE organisation_id=$1::uuid AND is_active=TRUE
			ORDER BY path`, c.Param("org_id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var secrets []map[string]interface{}
		for rows.Next() {
			var id, path, name, stype, desc string
			var rotEnabled bool
			var rotatedAt, expiresAt *time.Time
			var createdAt time.Time
			rows.Scan(&id, &path, &name, &stype, &desc, &rotEnabled, &rotatedAt, &expiresAt, &createdAt)
			secrets = append(secrets, map[string]interface{}{
				"id": id, "path": path, "name": name, "secret_type": stype,
				"description": desc, "rotation_enabled": rotEnabled,
				"last_rotated_at": rotatedAt, "expires_at": expiresAt, "created_at": createdAt,
			})
		}
		if secrets == nil {
			secrets = []map[string]interface{}{}
		}
		c.JSON(200, secrets)
	})

	// Rotate a secret manually
	r.POST("/orgs/:org_id/secrets/:path/rotate", func(c *gin.Context) {
		var body struct {
			NewValue  string `json:"new_value" binding:"required"`
			RotatedBy string `json:"rotated_by"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		var secretID string
		sm.db.QueryRowContext(c.Request.Context(), `
			SELECT id::text FROM vault_secrets
			WHERE organisation_id=$1::uuid AND path=$2 AND is_active=TRUE`,
			c.Param("org_id"), c.Param("path")).Scan(&secretID)
		if secretID == "" {
			c.JSON(404, gin.H{"error": "secret not found"})
			return
		}
		if err := sm.RotateSecret(c.Request.Context(), secretID, body.NewValue, body.RotatedBy); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"status": "rotated", "secret_id": secretID})
	})

	// Bulk inject for agent (called by executor before starting agent)
	r.GET("/orgs/:org_id/agents/:agent_id/inject", func(c *gin.Context) {
		secrets, err := sm.BulkInjectForAgent(
			c.Request.Context(), c.Param("org_id"), c.Param("agent_id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		// Values encoded to prevent log leakage
		encoded := map[string]string{}
		for k, v := range secrets {
			encoded[k] = base64.StdEncoding.EncodeToString([]byte(v))
		}
		c.JSON(200, gin.H{"secrets": encoded, "count": len(secrets)})
	})

	// Delete secret
	r.DELETE("/orgs/:org_id/secrets/:path", func(c *gin.Context) {
		sm.db.ExecContext(c.Request.Context(), `
			UPDATE vault_secrets SET is_active=FALSE, updated_at=NOW()
			WHERE organisation_id=$1::uuid AND path=$2`, c.Param("org_id"), c.Param("path"))
		c.JSON(200, gin.H{"status": "deleted"})
	})

	// Secret access logs
	r.GET("/orgs/:org_id/secrets/:path/logs", func(c *gin.Context) {
		rows, err := sm.db.QueryContext(c.Request.Context(), `
			SELECT sal.accessor_type, sal.accessor_id, sal.action, sal.success,
			       sal.error_message, sal.accessed_at
			FROM secret_access_logs sal
			JOIN vault_secrets vs ON vs.id=sal.secret_id
			WHERE vs.organisation_id=$1::uuid AND vs.path=$2
			ORDER BY sal.accessed_at DESC LIMIT 100`,
			c.Param("org_id"), c.Param("path"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var logs []map[string]interface{}
		for rows.Next() {
			var aType, aID, action string
			var success bool
			var errMsg *string
			var ts time.Time
			rows.Scan(&aType, &aID, &action, &success, &errMsg, &ts)
			logs = append(logs, map[string]interface{}{
				"accessor_type": aType, "accessor_id": aID,
				"action": action, "success": success,
				"error": errMsg, "accessed_at": ts,
			})
		}
		if logs == nil {
			logs = []map[string]interface{}{}
		}
		c.JSON(200, logs)
	})

	return r
}

// ── Main ───────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting secrets-manager", "port", cfg.Port)

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
			natsgo.Name("secrets-manager"))
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

	sm := NewSecretsManager(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sm.RunRotationWorker(ctx)

	router := sm.setupRouter()
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

// ── Helpers ────────────────────────────────────────────────────────────────

func lastSegment(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

func pathToEnvVar(path string) string {
	result := make([]byte, 0, len(path))
	for _, b := range []byte(path) {
		if b == '/' || b == '-' || b == '.' {
			result = append(result, '_')
		} else if b >= 'a' && b <= 'z' {
			result = append(result, b-32)
		} else {
			result = append(result, b)
		}
	}
	return string(result)
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}