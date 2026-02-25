import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend } from 'k6/metrics';

// Custom metrics
const startupTime = new Trend('agent_startup_time');
const executionTime = new Trend('agent_execution_time');

export const options = {
  scenarios: {
    agent_storm: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '30s', target: 5 },   // 5 concurrent agent creations
        { duration: '1m', target: 10 },    // Ramp to 10
        { duration: '30s', target: 0 },    // Ramp down
      ],
      gracefulRampDown: '30s',
    },
  },
  thresholds: {
    agent_startup_time: ['p(95)<5000'],   // 95% start within 5s
    http_req_failed: ['rate<0.05'],        // <5% failure rate
  },
};

const BASE_URL = 'http://localhost:8000/api/v1';

export default function() {
  const agentName = `storm-agent-${Date.now()}-${__VU}`;
  
  // Create agent
  const createRes = http.post(`${BASE_URL}/agents`, JSON.stringify({
    name: agentName,
    image: 'alpine:latest',
    config: {
      command: 'echo "starting" && sleep 5 && echo "done"'
    }
  }), {
    headers: { 'Content-Type': 'application/json' },
  });
  
  check(createRes, {
    'agent created': (r) => r.status === 201,
  });
  
  if (createRes.status !== 201) return;
  
  const agentId = JSON.parse(createRes.body).id;
  
  // Measure startup time
  const startTime = Date.now();
  const startRes = http.post(`${BASE_URL}/agents/${agentId}/start`);
  const startupDuration = Date.now() - startTime;
  
  startupTime.add(startupDuration);
  
  check(startRes, {
    'agent started': (r) => JSON.parse(r.body).success === true,
  });
  
  if (startRes.status === 200) {
    // Wait for execution
    sleep(10);
    
    // Check logs to verify execution
    const logsRes = http.get(`${BASE_URL}/agents/${agentId}/logs?limit=10`);
    const logs = JSON.parse(logsRes.body);
    const hasStarted = logs.some(log => log.message.includes('starting'));
    const hasDone = logs.some(log => log.message.includes('done'));
    
    check(logsRes, {
      'logs contain execution': () => hasStarted && hasDone,
    });
  }
  
  // Clean up
  http.del(`${BASE_URL}/agents/${agentId}`);
}