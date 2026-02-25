import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate } from 'k6/metrics';

const errorRate = new Rate('errors');

export const options = {
  stages: [
    { duration: '1m', target: 10 },
    { duration: '2m', target: 20 },
    { duration: '1m', target: 30 },
    { duration: '2m', target: 30 },
    { duration: '1m', target: 0 },
  ],
  thresholds: {
    http_req_duration: ['p(95)<500'],
    errors: ['rate<0.1'],
  },
};

const BASE_URL = 'http://192.168.116.148:8000/api/v1';

export default function() {
  let res = http.get(`${BASE_URL}/agents`);
  check(res, { 'list agents status 200': (r) => r.status === 200 });
  errorRate.add(res.status !== 200);
  sleep(1);

  const agentPayload = JSON.stringify({
    name: `load-test-agent-${Date.now()}`,
    image: 'alpine:latest',
    config: { command: 'echo "load test" && sleep 1' }
  });

  res = http.post(`${BASE_URL}/agents`, agentPayload, {
    headers: { 'Content-Type': 'application/json' },
  });

  check(res, {
    'create agent status 201': (r) => r.status === 201,
    'agent has id': (r) => JSON.parse(r.body).id !== undefined,
  });
  errorRate.add(res.status !== 201);

  if (res.status === 201) {
    const agentId = JSON.parse(res.body).id;

    res = http.post(`${BASE_URL}/agents/${agentId}/start`);
    check(res, { 'start agent success': (r) => JSON.parse(r.body).success === true });
    errorRate.add(!JSON.parse(res.body).success);
    sleep(2);

    res = http.get(`${BASE_URL}/agents/${agentId}/logs?limit=10`);
    check(res, { 'get logs status 200': (r) => r.status === 200 });

    res = http.post(`${BASE_URL}/agents/${agentId}/stop`);
    check(res, { 'stop agent success': (r) => JSON.parse(r.body).success === true });

    res = http.del(`${BASE_URL}/agents/${agentId}`);
    check(res, { 'delete agent status 204': (r) => r.status === 204 });
  }

  sleep(2);
}