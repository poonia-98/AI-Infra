.PHONY: up down build logs ps restart clean agent-image

up:
	docker-compose up -d

down:
	docker-compose down

build:
	docker-compose build

build-agent:
	docker build -t agent-runtime:latest ./agent-runtime

logs:
	docker-compose logs -f

ps:
	docker-compose ps

restart:
	docker-compose restart

clean:
	docker-compose down -v --remove-orphans

# Build the agent runtime image first (required before starting agents)
setup: build-agent build up
	@echo "\n✅ Platform is up!"
	@echo "   Frontend:   http://localhost:3000"
	@echo "   API:        http://localhost:8000/docs"
	@echo "   Prometheus: http://localhost:9090"
	@echo "   Grafana:    http://localhost:3001 (admin/admin)"
	@echo "   NATS:       http://localhost:8222"

status:
	@echo "=== Service Health ==="
	@curl -s http://localhost:8000/health | python3 -m json.tool 2>/dev/null || echo "Backend offline"
	@curl -s http://localhost:8081/health | python3 -m json.tool 2>/dev/null || echo "Executor offline"
	@curl -s http://localhost:8082/health | python3 -m json.tool 2>/dev/null || echo "Event processor offline"
	@curl -s http://localhost:8083/health | python3 -m json.tool 2>/dev/null || echo "Metrics collector offline"
	@curl -s http://localhost:8084/health | python3 -m json.tool 2>/dev/null || echo "WS Gateway offline"