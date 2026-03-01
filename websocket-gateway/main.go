package main

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	natsgo "github.com/nats-io/nats.go"
	"golang.org/x/time/rate"
)

type AccessClaims struct {
	Sub   string `json:"sub"`
	OrgID string `json:"org_id"`
	Role  string `json:"role"`
	SID   string `json:"sid"`
	Type  string `json:"type"`
	jwt.RegisteredClaims
}

type Client struct {
	id      string
	conn    *websocket.Conn
	send    chan []byte
	hub     *Hub
	agentID string
	mu      sync.Mutex
}

type Hub struct {
	clients    map[string]*Client
	mu         sync.RWMutex
	register   chan *Client
	unregister chan *Client
	totalConns atomic.Int64
}

func NewHub() *Hub {
	return &Hub{
		clients:    make(map[string]*Client),
		register:   make(chan *Client, 100),
		unregister: make(chan *Client, 100),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case c := <-h.register:
			h.mu.Lock()
			h.clients[c.id] = c
			h.mu.Unlock()
			h.totalConns.Add(1)
		case c := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[c.id]; ok {
				delete(h.clients, c.id)
				close(c.send)
			}
			h.mu.Unlock()
		}
	}
}

func (h *Hub) Broadcast(subject string, data interface{}, agentID string) {
	msg, err := json.Marshal(map[string]interface{}{
		"subject": subject,
		"data":    data,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		if c.agentID == "" || agentID == "" || c.agentID == agentID {
			select {
			case c.send <- msg:
			default:
			}
		}
	}
}

func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (c *Client) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() { ticker.Stop(); c.conn.Close() }()
	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			c.mu.Lock()
			err := c.conn.WriteMessage(websocket.TextMessage, msg)
			c.mu.Unlock()
			if err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			c.mu.Lock()
			err := c.conn.WriteMessage(websocket.PingMessage, nil)
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (c *Client) readPump() {
	defer func() { c.hub.unregister <- c; c.conn.Close() }()
	c.conn.SetReadLimit(4096)
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
		var m map[string]interface{}
		if json.Unmarshal(msg, &m) == nil {
			if id, ok := m["agent_id"].(string); ok {
				c.agentID = id
			}
		}
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func parseBearer(header string) string {
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}

func validateJWT(raw, secret string) (*AccessClaims, error) {
	claims := &AccessClaims{}
	t, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (interface{}, error) {
		return []byte(secret), nil
	})
	if err != nil || !t.Valid {
		return nil, err
	}
	if claims.Type != "access" || claims.Sub == "" || claims.OrgID == "" || claims.Role == "" || claims.SID == "" {
		return nil, jwt.ErrTokenInvalidClaims
	}
	return claims, nil
}

func normalizeOrigin(v string) string {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

func buildAllowedOrigins(raw string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, item := range strings.Split(raw, ",") {
		n := normalizeOrigin(item)
		if n != "" {
			out[n] = struct{}{}
		}
	}
	return out
}

func requireWSAuth(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := parseBearer(c.GetHeader("Authorization"))
		if token == "" {
			token = strings.TrimSpace(c.Query("token"))
		}
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
			return
		}
		claims, err := validateJWT(token, secret)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		c.Set("claims", claims)
		c.Next()
	}
}

func requireAdmin(secret, serviceKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if serviceKey != "" {
			got := c.GetHeader("X-Service-API-Key")
			if got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(serviceKey)) == 1 {
				c.Next()
				return
			}
		}
		token := parseBearer(c.GetHeader("Authorization"))
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
			return
		}
		claims, err := validateJWT(token, secret)
		if err != nil || claims.Role != "admin" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		c.Next()
	}
}

func main() {
	natsURL := env("NATS_URL", "nats://nats:4222")
	jwtSecret := env("JWT_SECRET", "")
	serviceAPIKey := env("CONTROL_PLANE_API_KEY", "")
	allowedOrigins := buildAllowedOrigins(env("ALLOWED_ORIGINS", "https://localhost:3000"))
	broadcastLimiter := rate.NewLimiter(rate.Every(50*time.Millisecond), 20)

	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			origin := normalizeOrigin(r.Header.Get("Origin"))
			_, ok := allowedOrigins[origin]
			return ok
		},
	}

	var nc *natsgo.Conn
	var err error
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(natsURL, natsgo.RetryOnFailedConnect(true), natsgo.MaxReconnects(-1))
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Fatalf("NATS: %v", err)
	}
	defer nc.Close()
	log.Printf("WS Gateway connected to NATS: %s", natsURL)

	hub := NewHub()
	go hub.Run()

	subjects := map[string]string{
		"agents.events":  "",
		"logs.stream":    "agent_id",
		"events.stream":  "agent_id",
		"metrics.stream": "agent_id",
		"alerts.stream":  "agent_id",
	}
	for subj, field := range subjects {
		s, f := subj, field
		_, _ = nc.Subscribe(s, func(msg *natsgo.Msg) {
			var data map[string]interface{}
			if err := json.Unmarshal(msg.Data, &data); err != nil {
				return
			}
			agentID := ""
			if f != "" {
				agentID, _ = data[f].(string)
			}
			hub.Broadcast(s, data, agentID)
		})
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "healthy", "connections": hub.Count()})
	})

	r.POST("/broadcast", requireAdmin(jwtSecret, serviceAPIKey), func(c *gin.Context) {
		if !broadcastLimiter.Allow() {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limited"})
			return
		}
		var payload map[string]interface{}
		if err := c.ShouldBindJSON(&payload); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
			return
		}
		subject, _ := payload["subject"].(string)
		data := payload["data"]
		agentID := ""
		if d, ok := data.(map[string]interface{}); ok {
			agentID, _ = d["agent_id"].(string)
		}
		hub.Broadcast(subject, data, agentID)
		c.JSON(http.StatusOK, gin.H{"ok": true, "connections": hub.Count()})
	})

	r.GET("/ws", requireWSAuth(jwtSecret), func(c *gin.Context) {
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		clientID := c.Query("client_id")
		if clientID == "" {
			if v, ok := c.Get("claims"); ok {
				if claims, ok := v.(*AccessClaims); ok {
					clientID = claims.Sub
				}
			}
		}
		if clientID == "" {
			clientID = conn.RemoteAddr().String()
		}
		cl := &Client{
			id:      clientID,
			conn:    conn,
			send:    make(chan []byte, 256),
			hub:     hub,
			agentID: c.Query("agent_id"),
		}
		hub.register <- cl
		go cl.writePump()
		cl.readPump()
	})

	port := env("PORT", "8084")
	log.Printf("WS Gateway listening on :%s", port)
	_ = r.Run(":" + port)
}
