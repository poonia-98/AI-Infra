import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate, Trend } from 'k6/metrics';
const errorRate = new Rate('errors');
const startTrend = new Trend('agent_start_ms', true);
const BASE_URL = __ENV.BASE_URL || 'http://host.docker.internal:8000/api/v1';
export const options = {
  stages: [
    { duration: '1m', target: 5 },
    { duration: '2m', target: 10 },
    { duration: '1m', target: 0 },
  ],
  thresholds: { errors: ['rate<0.05'], agent_start_ms: ['p(95)<10000'] },
};
export default function () {
  let r = http.post(BASE_URL + '/agents', JSON.stringify({
    name: 'perf-' + __VU + '-' + Date.now(),
    image: 'alpine:latest',
    agent_type: 'langgraph',
    config: {}
  }), { headers: { 'Content-Type': 'application/json' } });
  check(r, { 'create 201': (r) => r.status === 201 });
  errorRate.add(r.status !== 201);
  if (r.status !== 201) return;
  const id = JSON.parse(r.body).id;
  sleep(0.5);
  r = http.post(BASE_URL + '/agents/' + id + '/start');
  startTrend.add(r.timings.duration);
  check(r, { 'start ok': (r) => r.status === 200 });
  sleep(2);
  http.post(BASE_URL + '/agents/' + id + '/stop');
  sleep(0.5);
  http.del(BASE_URL + '/agents/' + id);
  sleep(1);
}