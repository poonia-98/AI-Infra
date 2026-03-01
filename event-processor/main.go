package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	natsgo "github.com/nats-io/nats.go"
)

var (
	processed atomic.Int64
	errCount  atomic.Int64
	serviceAPIKey string
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var httpClient = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
	},
}

func post(ctx context.Context, url string, body []byte) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if serviceAPIKey != "" {
		req.Header.Set("X-Service-API-Key", serviceAPIKey)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		errCount.Add(1)
		return
	}
	resp.Body.Close()
}

func handleAgentEvent(data []byte, controlURL, wsURL string) {
	var ev map[string]interface{}
	if err := json.Unmarshal(data, &ev); err != nil {
		errCount.Add(1)
		return
	}

	// Forward to WS gateway
	go func() {
		payload, _ := json.Marshal(map[string]interface{}{"subject": "agents.events", "data": ev})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		post(ctx, wsURL+"/broadcast", payload)
	}()

	// Persist event to control plane
	go func() {
		agentID, _ := ev["agent_id"].(string)
		evType, _ := ev["event_type"].(string)
		if agentID == "" || evType == "" {
			return
		}
		body, _ := json.Marshal(map[string]interface{}{
			"agent_id": agentID, "event_type": evType,
			"payload": ev, "source": "event-processor",
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		post(ctx, controlURL+"/api/v1/events", body)
	}()

	processed.Add(1)
}

func handleLogEvent(data []byte, controlURL, wsURL string) {
	var ev map[string]interface{}
	if err := json.Unmarshal(data, &ev); err != nil {
		errCount.Add(1)
		return
	}

	// Forward to WS gateway
	go func() {
		payload, _ := json.Marshal(map[string]interface{}{"subject": "logs.stream", "data": ev})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		post(ctx, wsURL+"/broadcast", payload)
	}()

	// Persist log to control plane
	go func() {
		agentID, _ := ev["agent_id"].(string)
		msg, _ := ev["message"].(string)
		level, _ := ev["level"].(string)
		source, _ := ev["source"].(string)
		if agentID == "" || msg == "" {
			return
		}
		if level == "" {
			level = "info"
		}
		body, _ := json.Marshal(map[string]interface{}{
			"agent_id": agentID, "message": msg,
			"level": level, "source": source,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		post(ctx, controlURL+"/api/v1/logs", body)
	}()

	processed.Add(1)
}

func handleMetricEvent(data []byte, wsURL string) {
	var ev map[string]interface{}
	if err := json.Unmarshal(data, &ev); err != nil {
		errCount.Add(1)
		return
	}
	go func() {
		payload, _ := json.Marshal(map[string]interface{}{"subject": "metrics.stream", "data": ev})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		post(ctx, wsURL+"/broadcast", payload)
	}()
	processed.Add(1)
}

func main() {
	natsURL := env("NATS_URL", "nats://nats:4222")
	controlURL := env("CONTROL_PLANE_URL", "http://backend:8000")
	wsURL := env("WS_GATEWAY_URL", "http://ws-gateway:8084")
	serviceAPIKey = env("CONTROL_PLANE_API_KEY", "")

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
	log.Printf("Event processor connected to NATS: %s", natsURL)

	nc.QueueSubscribe("agents.events", "event-processors", func(msg *natsgo.Msg) {
		go handleAgentEvent(msg.Data, controlURL, wsURL)
	})
	nc.QueueSubscribe("logs.stream", "event-processors", func(msg *natsgo.Msg) {
		go handleLogEvent(msg.Data, controlURL, wsURL)
	})
	nc.QueueSubscribe("metrics.stream", "event-processors", func(msg *natsgo.Msg) {
		go handleMetricEvent(msg.Data, wsURL)
	})
	nc.QueueSubscribe("events.stream", "event-processors", func(msg *natsgo.Msg) {
		var ev map[string]interface{}
		if json.Unmarshal(msg.Data, &ev) == nil {
			payload, _ := json.Marshal(map[string]interface{}{"subject": "events.stream", "data": ev})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			post(ctx, wsURL+"/broadcast", payload)
			cancel()
		}
		processed.Add(1)
	})

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status":    "healthy",
			"processed": processed.Load(),
			"errors":    errCount.Load(),
		})
	})

	port := env("PORT", "8082")
	log.Printf("Event processor listening on :%s", port)
	r.Run(":" + port)
}
