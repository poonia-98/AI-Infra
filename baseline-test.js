import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate } from 'k6/metrics';
const errorRate = new Rate('errors');
const BASE_URL = __ENV.BASE_URL || 'http://host.docker.internal:8000/api/v1';
export const options = {
  stages: [
    { duration: '30s', target: 20 },
    { duration: '1m', target: 50 },
    { duration: '1m', target: 100 },
    { duration: '30s', target: 0 },
  ],
  thresholds: { http_req_duration: ['p(95)<500'], errors: ['rate<0.01'] },
};
export default function () {
  let r = http.get(BASE_URL + '/agents');
  check(r, { 'agents 200': (r) => r.status === 200 });
  errorRate.add(r.status !== 200);
  sleep(0.5);
  r = http.get(BASE_URL + '/logs?limit=50');
  check(r, { 'logs 200': (r) => r.status === 200 });
  errorRate.add(r.status !== 200);
  sleep(0.5);
  r = http.get(BASE_URL + '/system/health');
  check(r, { 'health 200': (r) => r.status === 200 });
  errorRate.add(r.status !== 200);
  sleep(0.5);
}